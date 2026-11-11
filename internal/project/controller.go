// Package project owns the per-project lifecycle state machine:
// stopped → starting → running → stopping → stopped.
package project

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"portwake/internal/config"
	"portwake/internal/events"
	"portwake/internal/runner"
)

type State string

const (
	Stopped  State = "stopped"
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
)

var (
	// ErrBusy is returned by manual operations that conflict with a transition.
	ErrBusy = errors.New("project is busy")
	// ErrNotFound is returned for projects that are not configured.
	ErrNotFound = errors.New("project not found")
)

// Tuning holds timings shared by all controllers.
type Tuning struct {
	HealthPollInterval time.Duration
}

// attempt is one start attempt; waiters joined to it see its outcome.
type attempt struct {
	done chan struct{}
	err  error
}

// Controller runs the lifecycle of a single project. All transitions happen
// under mu; runner calls run in goroutines outside the lock.
type Controller struct {
	runner runner.Runner
	bus    *events.Bus
	tuning Tuning

	mu           sync.Mutex
	cfg          config.Project
	state        State
	conns        int
	attempt      *attempt
	changed      chan struct{} // closed and replaced on every transition
	pendingStart string        // trigger of a start requested while stopping
	runningSince time.Time
	idleTimer    *time.Timer
	idleGen      int
	idleDeadline time.Time
	lastErr      string
	removed      bool
	transitions  int
}

func NewController(cfg config.Project, runner runner.Runner, bus *events.Bus, tuning Tuning) *Controller {
	return &Controller{
		cfg:     cfg,
		runner:  runner,
		bus:     bus,
		tuning:  tuning,
		state:   Stopped,
		changed: make(chan struct{}),
	}
}

// Snapshot is a point-in-time view of a project.
type Snapshot struct {
	Name         string     `json:"name"`
	State        State      `json:"state"`
	Conns        int        `json:"conns"`
	RunningSince *time.Time `json:"running_since,omitempty"`
	IdleDeadline *time.Time `json:"idle_deadline,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
}

func (c *Controller) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Snapshot{Name: c.cfg.Name, State: c.state, Conns: c.conns, LastError: c.lastErr}
	if c.state == Running && !c.runningSince.IsZero() {
		t := c.runningSince
		s.RunningSince = &t
	}
	if c.idleTimer != nil {
		t := c.idleDeadline
		s.IdleDeadline = &t
	}
	return s
}

func (c *Controller) Config() config.Project {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// UpdateConfig swaps the configuration; timing changes apply to later timers.
func (c *Controller) UpdateConfig(cfg config.Project) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idleChanged := cfg.Idle() != c.cfg.Idle()
	c.cfg = cfg
	if idleChanged && c.state == Running && c.conns == 0 {
		c.armIdleLocked()
	}
}

// Remove detaches the controller: it stops managing the project but leaves
// it running.
func (c *Controller) Remove() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removed = true
	c.cancelIdleLocked()
}

// Acquire ensures the project is running (starting it if needed and waiting
// for readiness) and registers one active connection. The returned release
// must be called when the connection ends.
func (c *Controller) Acquire(ctx context.Context, trigger string) (release func(), err error) {
	var joined *attempt
	for {
		c.mu.Lock()
		switch c.state {
		case Running:
			c.conns++
			c.cancelIdleLocked()
			c.mu.Unlock()
			var once sync.Once
			return func() { once.Do(c.release) }, nil
		case Stopped:
			if joined != nil && joined.err != nil {
				c.mu.Unlock()
				return nil, joined.err
			}
			joined = c.startLocked(trigger)
		case Starting:
			joined = c.attempt
		case Stopping:
			if c.pendingStart == "" {
				c.pendingStart = trigger
			}
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *Controller) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conns--
	if c.conns == 0 && c.state == Running {
		c.armIdleLocked()
	}
}

// Start begins starting the project without waiting.
func (c *Controller) Start(trigger string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case Stopped:
		c.startLocked(trigger)
		return nil
	case Running, Starting:
		return nil
	default:
		return ErrBusy
	}
}

// Stop begins stopping the project without waiting.
func (c *Controller) Stop(reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case Running:
		c.stopLocked(reason)
		return nil
	case Stopped, Stopping:
		return nil
	default:
		return ErrBusy
	}
}

// Reconcile aligns the state with docker's view of the project. A result
// is discarded if any transition happened while docker was being queried.
func (c *Controller) Reconcile(ctx context.Context) {
	c.mu.Lock()
	gen, cfg := c.transitions, c.cfg
	c.mu.Unlock()
	st, err := c.runner.Ps(ctx, cfg)
	if err != nil {
		return
	}
	running := runner.IsRunning(st)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.removed || gen != c.transitions {
		return
	}
	switch {
	case c.state == Stopped && running:
		c.runningSince = time.Now()
		c.enterRunningLocked()
	case c.state == Running && !running:
		c.cancelIdleLocked()
		c.publish(events.Event{Type: events.StopDone, Reason: events.ReasonExternal, DurationMs: c.uptimeMs()})
		c.setStateLocked(Stopped)
	}
}

func (c *Controller) setStateLocked(s State) {
	c.state = s
	c.transitions++
	close(c.changed)
	c.changed = make(chan struct{})
	c.publish(events.Event{Type: events.State, State: string(s)})
}

func (c *Controller) publish(e events.Event) {
	e.Project = c.cfg.Name
	c.bus.Publish(e)
}

func (c *Controller) uptimeMs() int64 {
	if c.runningSince.IsZero() {
		return 0
	}
	return time.Since(c.runningSince).Milliseconds()
}

func (c *Controller) startLocked(trigger string) *attempt {
	a := &attempt{done: make(chan struct{})}
	c.attempt = a
	c.setStateLocked(Starting)
	cfg := c.cfg
	c.publish(events.Event{Type: events.StartBegin, Trigger: trigger})
	go c.runStart(a, cfg, trigger)
	return a
}

func (c *Controller) runStart(a *attempt, cfg config.Project, trigger string) {
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.StartLimit())
	defer cancel()
	err := c.runner.Up(ctx, cfg)
	if err == nil {
		err = c.waitReady(ctx, cfg)
	}
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("start timed out after %s: %w", cfg.StartLimit(), err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	a.err = err
	close(a.done)
	if err != nil {
		c.lastErr = err.Error()
		c.publish(events.Event{Type: events.StartFail, Trigger: trigger, Error: err.Error(), DurationMs: time.Since(t0).Milliseconds()})
		c.setStateLocked(Stopped)
		return
	}
	c.lastErr = ""
	c.runningSince = time.Now()
	c.publish(events.Event{Type: events.StartOK, Trigger: trigger, DurationMs: time.Since(t0).Milliseconds()})
	c.enterRunningLocked()
}

// waitReady waits until every route is ready: its health_service reports
// healthy (when it has a healthcheck), otherwise its upstream accepts TCP.
func (c *Controller) waitReady(ctx context.Context, cfg config.Project) error {
	poll := c.tuning.HealthPollInterval
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	for _, r := range cfg.Routes {
		for {
			if a, ok := c.runner.(runner.Alive); ok && !a.Alive(cfg) {
				return c.exitedDuringStart(cfg)
			}
			ready, err := c.routeReady(ctx, cfg, r)
			if err != nil {
				return err
			}
			if ready {
				break
			}
			select {
			case <-time.After(poll):
			case <-ctx.Done():
				return fmt.Errorf("port %d not ready: %w", r.Listen, ctx.Err())
			}
		}
	}
	return nil
}

// exitedDuringStart builds the start error for a process that died before
// becoming ready, including the tail of its output when available.
func (c *Controller) exitedDuringStart(cfg config.Project) error {
	msg := "process exited during startup"
	if ls, ok := c.runner.(runner.LogSource); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if out, err := ls.Logs(ctx, cfg, 20); err == nil {
			if tail := strings.TrimSpace(out); tail != "" {
				msg += ":\n" + tail
			}
		}
	}
	return errors.New(msg)
}

func (c *Controller) routeReady(ctx context.Context, cfg config.Project, r config.Route) (bool, error) {
	if r.HealthService != "" {
		st, err := c.runner.Ps(ctx, cfg)
		if err != nil {
			return false, nil
		}
		for _, s := range st {
			if s.Service != r.HealthService {
				continue
			}
			switch s.Health {
			case "healthy":
				return true, nil
			case "":
				// no healthcheck: fall back to a TCP probe
			default:
				return false, nil
			}
		}
	}
	return probe(ctx, r), nil
}

// probeHold is how long a probed connection must stay open (or produce
// data) to count as ready. Docker's port proxy accepts connections before
// the service inside the container listens and then closes them at once.
const probeHold = 150 * time.Millisecond

// probe checks an upstream: http routes must return any HTTP response, tcp
// routes must keep the connection open (or send a greeting).
func probe(ctx context.Context, r config.Route) bool {
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "tcp", r.Upstream)
	if err != nil {
		return false
	}
	defer conn.Close()
	if r.Mode == config.ModeHTTP {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := fmt.Fprintf(conn, "HEAD / HTTP/1.0\r\nHost: %s\r\n\r\n", r.Upstream); err != nil {
			return false
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	}
	conn.SetReadDeadline(time.Now().Add(probeHold))
	n, err := conn.Read(make([]byte, 1))
	if n > 0 {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (c *Controller) stopLocked(reason string) {
	c.cancelIdleLocked()
	c.setStateLocked(Stopping)
	cfg := c.cfg
	c.publish(events.Event{Type: events.StopBegin, Reason: reason})
	go c.runStop(cfg, reason)
}

func (c *Controller) runStop(cfg config.Project, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.StartLimit())
	defer cancel()
	err := c.runner.Stop(ctx, cfg)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.lastErr = err.Error()
		c.publish(events.Event{Type: events.Error, Message: "stop failed", Error: err.Error()})
		// The containers are most likely still up; go back to running.
		c.pendingStart = ""
		c.enterRunningLocked()
		return
	}
	c.publish(events.Event{Type: events.StopDone, Reason: reason, DurationMs: c.uptimeMs()})
	c.runningSince = time.Time{}
	c.setStateLocked(Stopped)
	if t := c.pendingStart; t != "" {
		c.pendingStart = ""
		c.startLocked(t)
	}
}

// enterRunningLocked moves to Running and starts the idle countdown when
// nothing is connected.
func (c *Controller) enterRunningLocked() {
	c.setStateLocked(Running)
	if c.conns == 0 {
		c.armIdleLocked()
	}
}

func (c *Controller) armIdleLocked() {
	c.cancelIdleLocked()
	idle := c.cfg.Idle()
	if idle <= 0 || c.removed {
		return
	}
	c.idleGen++
	gen := c.idleGen
	c.idleDeadline = time.Now().Add(idle)
	c.idleTimer = time.AfterFunc(idle, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if gen != c.idleGen || c.state != Running || c.conns != 0 {
			return
		}
		c.idleTimer = nil
		c.stopLocked(events.ReasonIdle)
	})
}

func (c *Controller) cancelIdleLocked() {
	if c.idleTimer != nil {
		c.idleTimer.Stop()
		c.idleTimer = nil
	}
	c.idleGen++
}
