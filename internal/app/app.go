// Package app wires dcpentry together. It is the single entry point used by
// both the CLI and the tests.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"dcpentry/internal/compose"
	"dcpentry/internal/config"
	"dcpentry/internal/events"
	"dcpentry/internal/project"
	"dcpentry/internal/proxy"
	"dcpentry/internal/store"
	"dcpentry/internal/web"
)

type Options struct {
	DBPath             string
	WebAddr            string
	Runner             compose.Runner
	Import             []byte // YAML replacing the stored configuration at start
	Grace              time.Duration
	ReconcileInterval  time.Duration
	HealthPollInterval time.Duration
}

type App struct {
	opts  Options
	bus   *events.Bus
	store *store.Store
	mgr   *project.Manager
	proxy *proxy.Server

	cfgMu    sync.Mutex
	projects []config.Project

	web       *http.Server
	webAddr   string
	bgCancel  context.CancelFunc
	bgWG      sync.WaitGroup
	storeStop func()
	storeDone chan struct{}
}

func New(opts Options) (*App, error) {
	if opts.Runner == nil {
		return nil, errors.New("runner is required")
	}
	if opts.DBPath == "" {
		opts.DBPath = "dcpentry.db"
	}
	if opts.WebAddr == "" {
		opts.WebAddr = "127.0.0.1:9000"
	}
	if opts.Grace <= 0 {
		opts.Grace = 10 * time.Second
	}
	if opts.ReconcileInterval <= 0 {
		opts.ReconcileInterval = 10 * time.Second
	}
	bus := events.NewBus()
	a := &App{opts: opts, bus: bus}
	a.mgr = project.NewManager(opts.Runner, bus, project.Tuning{HealthPollInterval: opts.HealthPollInterval})
	a.proxy = proxy.New(bus, func(name string) proxy.Waker {
		if c := a.mgr.Get(name); c != nil {
			return c
		}
		return nil
	})
	return a, nil
}

// Start opens the database, applies the configuration and starts serving.
func (a *App) Start(ctx context.Context) error {
	st, err := store.Open(a.opts.DBPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	a.store = st
	if a.opts.Import != nil {
		ps, err := config.UnmarshalYAML(a.opts.Import)
		if err == nil {
			err = config.Validate(ps)
		}
		if err == nil {
			err = st.ReplaceProjects(ps)
		}
		if err != nil {
			st.Close()
			return fmt.Errorf("import: %w", err)
		}
	}
	projects, err := st.LoadProjects()
	if err == nil {
		err = config.Validate(projects)
	}
	if err != nil {
		st.Close()
		return fmt.Errorf("load config: %w", err)
	}

	ch, cancel := a.bus.Subscribe(4096)
	a.storeStop, a.storeDone = cancel, make(chan struct{})
	go func() { st.Consume(ch); close(a.storeDone) }()

	a.mgr.Apply(ctx, projects)
	if err := a.proxy.Apply(projects); err != nil {
		a.closeStore()
		return err
	}
	a.projects = projects

	ln, err := net.Listen("tcp", a.opts.WebAddr)
	if err != nil {
		a.proxy.Shutdown(context.Background())
		a.closeStore()
		return fmt.Errorf("web listen: %w", err)
	}
	a.webAddr = ln.Addr().String()
	a.web = &http.Server{Handler: web.Handler(a), ReadHeaderTimeout: 10 * time.Second}
	go a.web.Serve(ln)

	bg, bgCancel := context.WithCancel(context.Background())
	a.bgCancel = bgCancel
	a.bgWG.Add(2)
	go func() { defer a.bgWG.Done(); a.mgr.Run(bg, a.opts.ReconcileInterval) }()
	go func() { defer a.bgWG.Done(); a.pruneLoop(bg) }()
	return nil
}

func (a *App) pruneLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	a.store.Prune(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.store.Prune(time.Now())
		}
	}
}

func (a *App) closeStore() {
	a.storeStop()
	<-a.storeDone
	a.store.Close()
}

// Shutdown closes the entry listeners, waits up to Grace for connections,
// stops the web server and flushes the store. Projects are left running.
func (a *App) Shutdown(ctx context.Context) error {
	grace, cancel := context.WithTimeout(ctx, a.opts.Grace)
	defer cancel()
	a.proxy.Shutdown(grace)
	a.bgCancel()
	a.bgWG.Wait()
	wctx, wcancel := context.WithTimeout(ctx, time.Second)
	defer wcancel()
	if a.web.Shutdown(wctx) != nil {
		a.web.Close()
	}
	a.closeStore()
	return nil
}

func (a *App) WebAddr() string               { return a.webAddr }
func (a *App) Bus() *events.Bus              { return a.bus }
func (a *App) Snapshots() []project.Snapshot { return a.mgr.Snapshots() }

func (a *App) Projects() []config.Project {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	return slices.Clone(a.projects)
}

// UpdateProjects transforms the current configuration with fn, validates
// the result and applies it. Listener failures roll everything back.
func (a *App) UpdateProjects(fn func([]config.Project) ([]config.Project, error)) error {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	projects, err := fn(slices.Clone(a.projects))
	if err != nil {
		return err
	}
	if err := config.Validate(projects); err != nil {
		return err
	}
	if err := a.proxy.Apply(projects); err != nil {
		return err
	}
	if err := a.store.ReplaceProjects(projects); err != nil {
		a.proxy.Apply(a.projects)
		return fmt.Errorf("save config: %w", err)
	}
	a.mgr.Apply(context.Background(), projects)
	a.projects = projects
	a.bus.Publish(events.Event{Type: events.ConfigChange, Message: fmt.Sprintf("%d projects", len(projects))})
	return nil
}

func (a *App) StartProject(name string) error {
	c := a.mgr.Get(name)
	if c == nil {
		return web.ErrNotFound
	}
	return c.Start("manual")
}

func (a *App) StopProject(name string) error {
	c := a.mgr.Get(name)
	if c == nil {
		return web.ErrNotFound
	}
	return c.Stop(events.ReasonManual)
}

func (a *App) Events(q store.EventQuery) ([]events.Event, error) { return a.store.Events(q) }

func (a *App) Stats(project string, from, to time.Time) ([]store.ProjectStats, error) {
	return a.store.Stats(project, from, to)
}
