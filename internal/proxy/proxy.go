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

	"dcpentry/internal/config"
	"dcpentry/internal/events"
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
	entries map[int]*entry
	httpSrv []*http.Server // every http server ever started; retired ones may still hold conns
	conns   map[net.Conn]struct{}
	connWG  sync.WaitGroup
}

func New(bus *events.Bus, lookup Lookup) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{bus: bus, lookup: lookup, ctx: ctx, cancel: cancel, entries: map[int]*entry{}, conns: map[net.Conn]struct{}{}}
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
		}
		for _, e := range toClose {
			if re, err := s.open(*e.target.Load()); err == nil {
				s.entries[re.port] = re
			} else {
				s.bus.Publish(events.Event{Type: events.Error, Port: e.port, Message: "restore listener", Error: err.Error()})
			}
		}
		return openErr
	}
	for port, t := range want {
		t := t
		s.entries[port].target.Store(&t)
	}
	return nil
}

func (s *Server) open(t target) (*entry, error) {
	r := t.route
	ln, err := net.Listen("tcp", net.JoinHostPort(r.Bind, fmt.Sprint(r.Listen)))
	if err != nil {
		return nil, fmt.Errorf("listen on port %d: %w", r.Listen, err)
	}
	e := &entry{port: r.Listen, bind: r.Bind, mode: r.Mode, ln: ln}
	e.target.Store(&t)
	if r.Mode == config.ModeHTTP {
		e.srv = &http.Server{Handler: s.httpHandler(e), ReadHeaderTimeout: 30 * time.Second}
		s.httpSrv = append(s.httpSrv, e.srv)
		go e.srv.Serve(ln)
	} else {
		go s.acceptTCP(e)
	}
	return e, nil
}

// Shutdown closes all listeners, waits for active connections until ctx is
// done and then force-closes the rest.
func (s *Server) Shutdown(ctx context.Context) {
	s.mu.Lock()
	for port, e := range s.entries {
		e.ln.Close()
		delete(s.entries, port)
	}
	servers := s.httpSrv
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
		s.connWG.Add(1)
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
			http.Error(w, "dcpentry: upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			http.Error(cw, "dcpentry: project not managed", http.StatusServiceUnavailable)
			return
		}
		release, err := wk.Acquire(r.Context(), trigger(e.port))
		if err != nil {
			http.Error(cw, "dcpentry: project failed to start: "+err.Error(), http.StatusServiceUnavailable)
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
