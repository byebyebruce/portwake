// Package compose drives docker compose projects. Runner is the single
// replaceable dependency of dcpentry; CLI is the production implementation.
package compose

import (
	"context"

	"dcpentry/internal/config"
)

// ServiceStatus is one row of `docker compose ps`.
type ServiceStatus struct {
	Service string
	State   string // e.g. "running", "exited"
	Health  string // "", "starting", "healthy", "unhealthy"
}

// Runner performs compose operations for a project.
type Runner interface {
	Up(ctx context.Context, p config.Project) error
	Stop(ctx context.Context, p config.Project) error
	Ps(ctx context.Context, p config.Project) ([]ServiceStatus, error)
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
