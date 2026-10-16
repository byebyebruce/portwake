package project

import (
	"context"
	"sort"
	"sync"
	"time"

	"dcpentry/internal/compose"
	"dcpentry/internal/config"
	"dcpentry/internal/events"
)

// Manager holds one Controller per configured project and periodically
// reconciles their state with docker.
type Manager struct {
	runner compose.Runner
	bus    *events.Bus
	tuning Tuning

	mu    sync.RWMutex
	ctrls map[string]*Controller
}

func NewManager(runner compose.Runner, bus *events.Bus, tuning Tuning) *Manager {
	return &Manager{runner: runner, bus: bus, tuning: tuning, ctrls: map[string]*Controller{}}
}

func (m *Manager) Get(name string) *Controller {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ctrls[name]
}

// Apply makes the controller set match projects: new projects get a
// controller (and are reconciled right away), changed ones get the new
// config, removed ones are detached without being stopped.
func (m *Manager) Apply(ctx context.Context, projects []config.Project) {
	m.mu.Lock()
	want := map[string]bool{}
	var added []*Controller
	for _, p := range projects {
		want[p.Name] = true
		if c, ok := m.ctrls[p.Name]; ok {
			c.UpdateConfig(p)
			continue
		}
		c := NewController(p, m.runner, m.bus, m.tuning)
		m.ctrls[p.Name] = c
		added = append(added, c)
	}
	for name, c := range m.ctrls {
		if !want[name] {
			c.Remove()
			delete(m.ctrls, name)
		}
	}
	m.mu.Unlock()
	for _, c := range added {
		c.Reconcile(ctx)
	}
}

func (m *Manager) Snapshots() []Snapshot {
	m.mu.RLock()
	out := make([]Snapshot, 0, len(m.ctrls))
	for _, c := range m.ctrls {
		out = append(out, c.Snapshot())
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Run reconciles every interval until ctx is done.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.ReconcileAll(ctx)
		}
	}
}

func (m *Manager) ReconcileAll(ctx context.Context) {
	m.mu.RLock()
	ctrls := make([]*Controller, 0, len(m.ctrls))
	for _, c := range m.ctrls {
		ctrls = append(ctrls, c)
	}
	m.mu.RUnlock()
	for _, c := range ctrls {
		c.Reconcile(ctx)
	}
}
