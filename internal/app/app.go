// Package app wires portwake together. It is the single entry point used by
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

	"portwake/internal/compose"
	"portwake/internal/config"
	"portwake/internal/events"
	"portwake/internal/project"
	"portwake/internal/proxy"
	"portwake/internal/store"
	"portwake/internal/web"
)

type Options struct {
	DBPath  string
	WebAddr string
	Runner  compose.Runner
	// Import is YAML that replaces the stored configuration at start (db mode).
	Import []byte
	// ConfigPath makes a YAML file the configuration source: it is read at
	// start and web/API changes are written back to it (file mode).
	ConfigPath         string
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
	if opts.ConfigPath != "" && opts.Import != nil {
		return nil, errors.New("config file mode cannot be combined with import")
	}
	if opts.DBPath == "" {
		opts.DBPath = "portwake.db"
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
	projects, err := a.loadProjects()
	if err != nil {
		st.Close()
		return err
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
	go func() { defer a.bgWG.Done(); a.maintain(bg) }()
	return nil
}

// loadProjects reads the initial configuration from its source: the YAML
// config file in file mode, or SQLite (optionally seeded by Import) in db mode.
func (a *App) loadProjects() ([]config.Project, error) {
	if a.opts.ConfigPath != "" {
		projects, err := config.LoadFile(a.opts.ConfigPath)
		if err != nil {
			return nil, fmt.Errorf("load config file: %w", err)
		}
		return projects, nil
	}
	if a.opts.Import != nil {
		projects, err := config.UnmarshalYAML(a.opts.Import)
		if err == nil {
			err = config.Validate(projects)
		}
		if err == nil {
			err = a.store.ReplaceProjects(projects)
		}
		if err != nil {
			return nil, fmt.Errorf("import: %w", err)
		}
	}
	projects, err := a.store.LoadProjects()
	if err == nil {
		err = config.Validate(projects)
	}
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return projects, nil
}

// persistProjects writes the configuration to its source.
func (a *App) persistProjects(projects []config.Project) error {
	if a.opts.ConfigPath != "" {
		return config.SaveFile(a.opts.ConfigPath, projects)
	}
	return a.store.ReplaceProjects(projects)
}

// maintain prunes old data hourly and reports event-write failures once a
// minute (reporting from the store itself could loop on a broken database).
func (a *App) maintain(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	lastPrune, reported := time.Time{}, 0
	for {
		if time.Since(lastPrune) >= time.Hour {
			if err := a.store.Prune(time.Now()); err != nil {
				a.bus.Publish(events.Event{Type: events.Error, Message: "prune old data", Error: err.Error()})
			}
			lastPrune = time.Now()
		}
		if n, err := a.store.WriteErrors(); n > reported {
			a.bus.Publish(events.Event{Type: events.Error, Message: fmt.Sprintf("%d events failed to persist", n-reported), Error: err.Error()})
			reported = n
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *App) closeStore() error {
	a.storeStop()
	<-a.storeDone
	return a.store.Close()
}

// Shutdown closes the entry listeners, waits up to Grace for connections,
// stops the web server and flushes the store. Projects are left running.
func (a *App) Shutdown(ctx context.Context) error {
	if a.web == nil {
		return errors.New("not started")
	}
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
	return a.closeStore()
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
	// Controllers for new projects exist before their listeners open, and
	// removed projects keep theirs until the listeners are gone, so no
	// connection ever sees an unknown project and a rollback loses nothing.
	ctx := context.Background()
	a.mgr.Apply(ctx, withRemoved(projects, a.projects))
	if err := a.proxy.Apply(projects); err != nil {
		a.mgr.Apply(ctx, a.projects)
		return err
	}
	if err := a.persistProjects(projects); err != nil {
		if rerr := a.proxy.Apply(a.projects); rerr != nil {
			a.bus.Publish(events.Event{Type: events.Error, Message: "restore listeners", Error: rerr.Error()})
		}
		a.mgr.Apply(ctx, a.projects)
		return fmt.Errorf("save config: %w", err)
	}
	a.mgr.Apply(ctx, projects)
	a.projects = projects
	a.bus.Publish(events.Event{Type: events.ConfigChange, Message: fmt.Sprintf("%d projects", len(projects))})
	return nil
}

// withRemoved returns next plus the projects of prev that next drops.
func withRemoved(next, prev []config.Project) []config.Project {
	out := slices.Clone(next)
	for _, p := range prev {
		if !slices.ContainsFunc(next, func(n config.Project) bool { return n.Name == p.Name }) {
			out = append(out, p)
		}
	}
	return out
}

func (a *App) StartProject(name string) error {
	c := a.mgr.Get(name)
	if c == nil {
		return project.ErrNotFound
	}
	return c.Start(events.TriggerManual)
}

func (a *App) StopProject(name string) error {
	c := a.mgr.Get(name)
	if c == nil {
		return project.ErrNotFound
	}
	return c.Stop(events.ReasonManual)
}

func (a *App) Events(q store.EventQuery) ([]events.Event, error) { return a.store.Events(q) }

func (a *App) Stats(project string, from, to time.Time) ([]store.ProjectStats, error) {
	return a.store.Stats(project, from, to)
}
