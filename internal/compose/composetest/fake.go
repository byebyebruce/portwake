// Package composetest provides a fake runner for tests. It simulates any
// project type: "starting" a project opens real echo servers on each
// route's upstream address.
package composetest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"portwake/internal/config"
	"portwake/internal/runner"
)

type Fake struct {
	mu        sync.Mutex
	upDelay   map[string]time.Duration
	upErr     map[string]error
	stopDelay map[string]time.Duration
	warmup    map[string]time.Duration
	health    map[string]string // "project/service" -> health
	logs      map[string]string
	running   map[string]*instance
	upCalls   map[string]int
	stopCalls map[string]int
}

type instance struct {
	warmUntil time.Time
	listeners []net.Listener
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
}

func New() *Fake {
	return &Fake{
		upDelay:   map[string]time.Duration{},
		upErr:     map[string]error{},
		stopDelay: map[string]time.Duration{},
		warmup:    map[string]time.Duration{},
		health:    map[string]string{},
		logs:      map[string]string{},
		running:   map[string]*instance{},
		upCalls:   map[string]int{},
		stopCalls: map[string]int{},
	}
}

var _ runner.Runner = (*Fake)(nil)
var _ runner.LogSource = (*Fake)(nil)

// SetUpDelay makes Up for the project take d (honouring ctx cancellation).
func (f *Fake) SetUpDelay(project string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upDelay[project] = d
}

// SetStopDelay makes Stop for the project take d before stopping.
func (f *Fake) SetStopDelay(project string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopDelay[project] = d
}

// SetWarmup makes upstreams behave like docker's port proxy while the
// service inside the container is not listening yet: for d after Up they
// accept connections and close them immediately.
func (f *Fake) SetWarmup(project string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.warmup[project] = d
}

// SetUpError makes Up for the project fail with err (nil clears it).
func (f *Fake) SetUpError(project string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upErr[project] = err
}

// SetHealth sets the reported health of a service ("" = no healthcheck).
func (f *Fake) SetHealth(project, service, health string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health[project+"/"+service] = health
}

// SetLogs sets the text returned by Logs for the project.
func (f *Fake) SetLogs(project, logs string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs[project] = logs
}

func (f *Fake) Logs(ctx context.Context, p config.Project, lines int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs[p.Name], nil
}

func (f *Fake) UpCalls(project string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upCalls[project]
}

func (f *Fake) StopCalls(project string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopCalls[project]
}

func (f *Fake) IsRunning(project string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[project] != nil
}

func (f *Fake) Up(ctx context.Context, p config.Project) error {
	f.mu.Lock()
	f.upCalls[p.Name]++
	delay, upErr := f.upDelay[p.Name], f.upErr[p.Name]
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if upErr != nil {
		return upErr
	}
	return f.start(p)
}

// StartExternally simulates `docker compose up` run outside portwake.
func (f *Fake) StartExternally(p config.Project) error { return f.start(p) }

// StopExternally simulates `docker compose stop` run outside portwake.
func (f *Fake) StopExternally(project string) { f.stop(project) }

func (f *Fake) start(p config.Project) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[p.Name] != nil {
		return nil
	}
	inst := &instance{conns: map[net.Conn]struct{}{}, warmUntil: time.Now().Add(f.warmup[p.Name])}
	for _, r := range p.Routes {
		ln, err := net.Listen("tcp", r.Upstream)
		if err != nil {
			inst.close()
			return fmt.Errorf("fake up %s: %w", p.Name, err)
		}
		inst.listeners = append(inst.listeners, ln)
		ln = &warmListener{Listener: ln, until: inst.warmUntil}
		if r.Mode == config.ModeHTTP {
			go serveHTTP(inst, ln, p.Name)
		} else {
			go serveEcho(inst, ln)
		}
	}
	f.running[p.Name] = inst
	return nil
}

func (f *Fake) Stop(ctx context.Context, p config.Project) error {
	f.mu.Lock()
	f.stopCalls[p.Name]++
	delay := f.stopDelay[p.Name]
	f.mu.Unlock()
	time.Sleep(delay)
	f.stop(p.Name)
	return nil
}

func (f *Fake) stop(name string) {
	f.mu.Lock()
	inst := f.running[name]
	delete(f.running, name)
	f.mu.Unlock()
	if inst != nil {
		inst.close()
	}
}

func (f *Fake) Ps(ctx context.Context, p config.Project) ([]runner.ServiceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[p.Name] == nil {
		return nil, nil
	}
	out := []runner.ServiceStatus{{Service: "app", State: "running", Health: f.health[p.Name+"/app"]}}
	for _, r := range p.Routes {
		if r.HealthService != "" && r.HealthService != "app" {
			out = append(out, runner.ServiceStatus{Service: r.HealthService, State: "running", Health: f.health[p.Name+"/"+r.HealthService]})
		}
	}
	return out, nil
}

func (i *instance) track(c net.Conn) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.conns == nil {
		return false
	}
	i.conns[c] = struct{}{}
	return true
}

func (i *instance) untrack(c net.Conn) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.conns, c)
}

func (i *instance) close() {
	for _, ln := range i.listeners {
		ln.Close()
	}
	i.mu.Lock()
	conns := i.conns
	i.conns = nil
	i.mu.Unlock()
	for c := range conns {
		c.Close()
	}
}

func serveEcho(inst *instance, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if !inst.track(c) {
			c.Close()
			return
		}
		go func() {
			defer inst.untrack(c)
			defer c.Close()
			io.Copy(c, c)
		}()
	}
}

// serveHTTP answers "hello <project> <path>" and echoes after an Upgrade.
func serveHTTP(inst *instance, ln net.Listener, project string) {
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Upgrade") != "" {
				hj, _ := w.(http.Hijacker)
				c, brw, err := hj.Hijack()
				if err != nil {
					return
				}
				defer c.Close()
				fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", r.Header.Get("Upgrade"))
				echoBuffered(c, brw.Reader)
				return
			}
			fmt.Fprintf(w, "hello %s %s", project, r.URL.Path)
		}),
		ConnState: func(c net.Conn, s http.ConnState) {
			switch s {
			case http.StateNew:
				if !inst.track(c) {
					c.Close()
				}
			case http.StateClosed, http.StateHijacked:
				if s == http.StateClosed {
					inst.untrack(c)
				}
			}
		},
	}
	srv.Serve(ln)
}

func echoBuffered(c net.Conn, r *bufio.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := c.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// warmListener closes connections accepted before until.
type warmListener struct {
	net.Listener
	until time.Time
}

func (l *warmListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil || !time.Now().Before(l.until) {
			return c, err
		}
		c.Close()
	}
}
