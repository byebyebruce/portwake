// Package runner defines the operations portwake needs from a managed
// project and dispatches them to the implementation for its type
// (compose, docker or exec).
package runner

import (
	"context"
	"errors"
	"fmt"

	"portwake/internal/config"
)

// ServiceStatus is one row of a runner's status report; it mirrors the
// fields of `docker compose ps` that portwake cares about.
type ServiceStatus struct {
	Service string
	State   string // e.g. "running", "exited"
	Health  string // "", "starting", "healthy", "unhealthy"
}

// IsRunning reports whether any service of the project is running.
func IsRunning(st []ServiceStatus) bool {
	for _, s := range st {
		if s.State == "running" {
			return true
		}
	}
	return false
}

// Runner performs lifecycle operations for one project type.
type Runner interface {
	Up(ctx context.Context, p config.Project) error
	Stop(ctx context.Context, p config.Project) error
	Ps(ctx context.Context, p config.Project) ([]ServiceStatus, error)
}

// LogSource is implemented by runners that can return recent output. The
// returned text is the last lines of the project's output.
type LogSource interface {
	Logs(ctx context.Context, p config.Project, lines int) (string, error)
}

// Alive is implemented by runners that can tell whether the project's
// process is still alive. It lets a start fail fast instead of waiting out
// the whole start timeout. Runners without an implementation are assumed
// to be alive.
type Alive interface {
	Alive(p config.Project) bool
}

// ErrNoRunner is returned when a project type has no configured runner.
var ErrNoRunner = errors.New("no runner configured")

// Manager routes every call to the runner registered for the project type.
// A nil runner means that type is unavailable and calls fail with ErrNoRunner.
type Manager struct {
	Compose Runner
	Docker  Runner
	Exec    Runner
}

// For returns the runner for the project's type.
func (m Manager) For(p config.Project) (Runner, error) {
	var r Runner
	switch p.RunnerType() {
	case config.TypeCompose:
		r = m.Compose
	case config.TypeDocker:
		r = m.Docker
	case config.TypeExec:
		r = m.Exec
	default:
		return nil, fmt.Errorf("%w: %s", ErrNoRunner, p.RunnerType())
	}
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoRunner, p.RunnerType())
	}
	return r, nil
}

func (m Manager) Up(ctx context.Context, p config.Project) error {
	r, err := m.For(p)
	if err != nil {
		return err
	}
	return r.Up(ctx, p)
}

func (m Manager) Stop(ctx context.Context, p config.Project) error {
	r, err := m.For(p)
	if err != nil {
		return err
	}
	return r.Stop(ctx, p)
}

func (m Manager) Ps(ctx context.Context, p config.Project) ([]ServiceStatus, error) {
	r, err := m.For(p)
	if err != nil {
		return nil, err
	}
	return r.Ps(ctx, p)
}

// Logs returns recent output when the runner for the project type supports it.
func (m Manager) Logs(ctx context.Context, p config.Project, lines int) (string, error) {
	r, err := m.For(p)
	if err != nil {
		return "", err
	}
	ls, ok := r.(LogSource)
	if !ok {
		return "", fmt.Errorf("%s projects do not provide logs", p.RunnerType())
	}
	return ls.Logs(ctx, p, lines)
}

// Alive reports whether the project's process is still alive. It returns
// true when the runner cannot tell.
func (m Manager) Alive(p config.Project) bool {
	r, err := m.For(p)
	if err != nil {
		return true
	}
	if a, ok := r.(Alive); ok {
		return a.Alive(p)
	}
	return true
}
