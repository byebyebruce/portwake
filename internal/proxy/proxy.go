// Package proxy owns the entry listeners and forwards TCP / HTTP traffic to
// project upstreams, waking projects through their controller first.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"portwake/internal/config"
	"portwake/internal/events"
)

// Waker brings a project up and counts an active connection on it.
type Waker interface {
	Acquire(ctx context.Context, trigger string) (release func(), err error)
}

// Lookup returns the waker for a project, or nil when it is not managed.
type Lookup func(project string) Waker

type target struct {
	project string
	route   config.Route
}

type entry struct {
	port   int
	bind   string
	mode   string
	target atomic.Pointer[target]
	ln     net.Listener
	srv    *http.Server // http mode only
}

// Server is the set of entry listeners.
type Server struct {
	bus    *events.Bus
	lookup Lookup
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closing bool
	entries map[int]*entry
	httpSrv map[*http.Server]struct{} // live and retired servers still holding conns
	conns   map[net.Conn]struct{}
	connWG  sync.WaitGroup // TCP connections and upgraded HTTP connections
}

func New(bus *events.Bus, lookup Lookup) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{bus: bus, lookup: lookup, ctx: ctx, cancel: cancel, entries: map[int]*entry{},
		httpSrv: map[*http.Server]struct{}{}, conns: map[net.Conn]struct{}{}}
}

// Apply makes the listeners match the routes of projects. Either every
// listener change succeeds or the previous listeners are restored and an
// error is returned. Established connections are never closed.
func (s *Server) Apply(projects []config.Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := map[int]target{}
	for _, p := range projects {
		for _, r := range p.Routes {
			want[r.Listen] = target{project: p.Name, route: r}
		}
	}
	var toClose []*entry
	for port, e := range s.entries {
		t, ok := want[port]
		if !ok || t.route.Bind != e.bind || t.route.Mode != e.mode {
			toClose = append(toClose, e)
		}
	}
	for _, e := range toClose {
		e.ln.Close()
		delete(s.entries, e.port)
	}
	defer func() {
		for _, e := range toClose {
			if e.srv != nil {
				go s.retire(e.srv)
			}
		}
	}()
	var opened []*entry
	var openErr error
	for port, t := range want {
		if _, ok := s.entries[port]; ok {
			continue
		}
		e, err := s.open(t)
		if err != nil {
			openErr = err
			break
		}
		opened = append(opened, e)
		s.entries[port] = e
	}
	if openErr != nil {
		for _, e := range opened {
			e.ln.Close()
			delete(s.entries, e.port)
			if e.srv != nil {
				go s.retire(e.srv)
			}
		}
		for _, e := range toClose {
			if re, err := s.open(*e.target.Load()); err == nil {
				s.entries[re.port] = re
			} else {
				s.bus.Publish(events.Event{Type: events.Error, Port: e.port, Message: "restore listener", Error: err.Error()})
			}
		}
		return fmt.Errorf("%w: %w", config.ErrInvalid, openErr)
	}
	for port, t := range want {
		t := t
		s.entries[port].target.Store(&t)
	}
	return nil
}

// listenNetwork picks an explicit address family for the bind IP. With the
// generic "tcp" network, Go treats 0.0.0.0 as a wildcard and may create a
// dual-stack [::] socket, so a supposed IPv4 bind can end up IPv6-only on
// systems where IPv4-mapped connections do not work.
func listenNetwork(bind string) string {
	if bind == "" {
		bind = config.DefaultBind
	}
	ip := net.ParseIP(bind)
	switch {
	case ip == nil:
		return "tcp"
	case ip.To4() != nil:
		return "tcp4"
	default:
		return "tcp6"
	}
}

func (s *Server) open(t target) (*entry, error) {
	r := t.route
	ln, err := net.Listen(listenNetwork(r.Bind), net.JoinHostPort(r.Bind, fmt.Sprint(r.Listen)))
	if err != nil {
		return nil, fmt.Errorf("listen on port %d: %w", r.Listen, err)
	}
	e := &entry{port: r.Listen, bind: r.Bind, mode: r.Mode, ln: ln}
	e.target.Store(&t)
	if r.Mode == config.ModeHTTP {
		e.srv = &http.Server{Handler: s.httpHandler(e), ReadHeaderTimeout: 30 * time.Second}
		s.httpSrv[e.srv] = struct{}{}
		go e.srv.Serve(ln)
	} else {
		go s.acceptTCP(e)
	}
	return e, nil
}

// retire lets a closed http listener's server finish its in-flight requests
// and then forgets it. Idle keep-alive connections are closed.
func (s *Server) retire(srv *http.Server) {
	srv.Shutdown(s.ctx)
	s.mu.Lock()
	delete(s.httpSrv, srv)
	s.mu.Unlock()
}

// Shutdown closes all listeners, waits for active connections until ctx is
// done and then force-closes the rest.
func (s *Server) Shutdown(ctx context.Context) {
	s.mu.Lock()
	s.closing = true
	for port, e := range s.entries {
		e.ln.Close()
		delete(s.entries, port)
	}
	servers := make([]*http.Server, 0, len(s.httpSrv))
	for srv := range s.httpSrv {
		servers = append(servers, srv)
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(srv *http.Server) {
			defer wg.Done()
			if srv.Shutdown(ctx) != nil {
				srv.Close()
			}
		}(srv)
	}
	tcpDone := make(chan struct{})
	go func() { s.connWG.Wait(); close(tcpDone) }()
	select {
	case <-tcpDone:
	case <-ctx.Done():
		s.cancel()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		<-tcpDone
	}
	wg.Wait()
}

// begin registers an active connection unless shutting down.
func (s *Server) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.connWG.Add(1)
	return true
}

func (s *Server) track(c net.Conn) {
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *Server) acceptTCP(e *entry) {
	for {
		c, err := e.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.bus.Publish(events.Event{Type: events.Error, Port: e.port, Message: "accept", Error: err.Error()})
			}
			return
		}
		if !s.begin() {
			c.Close()
			return
		}
		go s.handleTCP(e, c)
	}
}

func trigger(port int) string { return fmt.Sprintf("port %d", port) }

func (s *Server) handleTCP(e *entry, c net.Conn) {
	defer s.connWG.Done()
	s.track(c)
	defer s.untrack(c)
	defer c.Close()

	t := e.target.Load()
	w := s.lookup(t.project)
	if w == nil {
		return
	}
	release, err := w.Acquire(s.ctx, trigger(e.port))
	if err != nil {
		return
	}
	defer release()

	up, err := net.DialTimeout("tcp", t.route.Upstream, 10*time.Second)
	if err != nil {
		s.bus.Publish(events.Event{Type: events.Error, Project: t.project, Port: e.port, Message: "dial upstream", Error: err.Error()})
		return
	}
	s.track(up)
	defer s.untrack(up)
	defer up.Close()

	t0 := time.Now()
	remote := c.RemoteAddr().String()
	s.bus.Publish(events.Event{Type: events.ConnOpen, Project: t.project, Port: e.port, Remote: remote})
	var in, out int64
	done := make(chan struct{}, 2)
	go func() { in, _ = io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
	go func() { out, _ = io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
	s.bus.Publish(events.Event{Type: events.ConnClose, Project: t.project, Port: e.port, Remote: remote,
		BytesIn: in, BytesOut: out, DurationMs: time.Since(t0).Milliseconds()})
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(interface{ CloseWrite() error }); ok {
		tc.CloseWrite()
	} else {
		c.Close()
	}
}

func (s *Server) httpHandler(e *entry) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t := e.target.Load()
			pr.SetURL(&url.URL{Scheme: "http", Host: t.route.Upstream})
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			t := e.target.Load()
			s.bus.Publish(events.Event{Type: events.Error, Project: t.project, Port: e.port, Message: "proxy upstream", Error: err.Error()})
			http.Error(w, "portwake: upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Upgraded connections are hijacked and invisible to http.Server
		// Shutdown; track them like TCP connections so they get the grace
		// period, and cancel them (ReverseProxy then closes both ends) on
		// force-close.
		if r.Header.Get("Upgrade") != "" {
			if !s.begin() {
				http.Error(w, "portwake: shutting down", http.StatusServiceUnavailable)
				return
			}
			defer s.connWG.Done()
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
		r = r.WithContext(ctx)

		t := e.target.Load()
		t0 := time.Now()
		cw := &countingWriter{ResponseWriter: w}
		body := &countingReader{ReadCloser: r.Body}
		r.Body = body
		defer func() {
			s.bus.Publish(events.Event{Type: events.HTTPRequest, Project: t.project, Port: e.port, Remote: r.RemoteAddr,
				Method: r.Method, Path: r.URL.Path, Status: cw.status(), BytesIn: body.n, BytesOut: cw.n,
				DurationMs: time.Since(t0).Milliseconds()})
		}()
		wk := s.lookup(t.project)
		if wk == nil {
			http.Error(cw, "portwake: project not managed", http.StatusServiceUnavailable)
			return
		}
		release, err := wk.Acquire(r.Context(), trigger(e.port))
		if err != nil {
			http.Error(cw, "portwake: project failed to start: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer release()
		rp.ServeHTTP(cw, r)
	})
}

type countingWriter struct {
	http.ResponseWriter
	code int
	n    int64
}

func (w *countingWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

func (w *countingWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

// Unwrap lets http.ResponseController reach Flush/Hijack (used for upgrades).
func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type countingReader struct {
	io.ReadCloser
	n int64
}

func (r *countingReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	r.n += int64(n)
	return n, err
}
