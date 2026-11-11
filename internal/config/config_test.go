package config_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"portwake/internal/config"
)

func route() config.Route {
	return config.Route{Listen: 8080, Bind: "127.0.0.1", Mode: config.ModeTCP, Upstream: "127.0.0.1:18080"}
}

func TestRunnerTypeDefaultsToCompose(t *testing.T) {
	p := config.Project{Name: "web"}
	if got := p.RunnerType(); got != config.TypeCompose {
		t.Fatalf("RunnerType = %q, want compose", got)
	}
}

func TestValidatePerType(t *testing.T) {
	tests := []struct {
		name    string
		project config.Project
		wantErr string
	}{
		{
			name:    "compose requires dir",
			project: config.Project{Name: "web", Routes: []config.Route{route()}},
			wantErr: "dir is required",
		},
		{
			name: "compose rejects exec fields",
			project: config.Project{Name: "web", Dir: "/tmp", Command: []string{"./app"},
				Routes: []config.Route{route()}},
			wantErr: "command is only supported",
		},
		{
			name: "docker requires image",
			project: config.Project{Name: "web", Type: config.TypeDocker,
				Routes: []config.Route{route()}},
			wantErr: "image is required",
		},
		{
			name: "docker rejects dir",
			project: config.Project{Name: "web", Type: config.TypeDocker, Image: "nginx", Dir: "/tmp",
				Routes: []config.Route{route()}},
			wantErr: "dir is only supported",
		},
		{
			name: "exec requires command",
			project: config.Project{Name: "web", Type: config.TypeExec,
				Routes: []config.Route{route()}},
			wantErr: "command is required",
		},
		{
			name: "exec rejects image",
			project: config.Project{Name: "web", Type: config.TypeExec, Image: "nginx", Command: []string{"./app"},
				Routes: []config.Route{route()}},
			wantErr: "image is only supported",
		},
		{
			name: "unknown type",
			project: config.Project{Name: "web", Type: "kubernetes", Dir: "/tmp",
				Routes: []config.Route{route()}},
			wantErr: "type must be",
		},
		{
			name: "health_service only for compose",
			project: config.Project{Name: "web", Type: config.TypeDocker, Image: "nginx",
				Routes: []config.Route{{Listen: 8080, Bind: "127.0.0.1", Upstream: "127.0.0.1:18080", HealthService: "app"}}},
			wantErr: "health_service is only supported",
		},
		{
			name: "invalid env key",
			project: config.Project{Name: "web", Type: config.TypeExec, Command: []string{"./app"},
				Env:    map[string]string{"BAD-KEY": "x"},
				Routes: []config.Route{route()}},
			wantErr: "invalid env key",
		},
		{
			name: "invalid signal",
			project: config.Project{Name: "web", Type: config.TypeExec, Command: []string{"./app"}, StopSignal: "SIGWHAT",
				Routes: []config.Route{route()}},
			wantErr: "invalid stop_signal",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := config.Validate([]config.Project{tc.project})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate error = %v, want %q", err, tc.wantErr)
			}
			if !errors.Is(err, config.ErrInvalid) {
				t.Fatalf("error does not wrap ErrInvalid: %v", err)
			}
		})
	}
}

func TestValidateNormalizesSignalAndDefaults(t *testing.T) {
	p := config.Project{
		Name:       "app",
		Type:       config.TypeExec,
		Command:    []string{"./app"},
		StopSignal: "term",
		Routes:     []config.Route{route()},
	}
	projects := []config.Project{p}
	if err := config.Validate(projects); err != nil {
		t.Fatal(err)
	}
	if projects[0].StopSignal != "SIGTERM" {
		t.Fatalf("StopSignal = %q, want SIGTERM", projects[0].StopSignal)
	}
	if got := p.StopLimit(); got != config.DefaultStopTimeout {
		t.Fatalf("StopLimit = %s, want %s", got, config.DefaultStopTimeout)
	}
	if got := p.Idle(); got != config.DefaultIdleTimeout {
		t.Fatalf("Idle = %s, want %s", got, config.DefaultIdleTimeout)
	}
	if got := p.StartLimit(); got != config.DefaultStartTimeout {
		t.Fatalf("StartLimit = %s, want %s", got, config.DefaultStartTimeout)
	}
}

func TestValidateExecWithEverything(t *testing.T) {
	stop := config.Duration(2 * time.Second)
	p := config.Project{
		Name:        "app",
		Type:        config.TypeExec,
		Dir:         "/srv/app",
		Command:     []string{"./app", "--port", "18080"},
		Env:         map[string]string{"LOG_LEVEL": "info"},
		EnvFile:     ".env",
		StopSignal:  "SIGINT",
		StopTimeout: &stop,
		Routes:      []config.Route{route()},
	}
	if err := config.Validate([]config.Project{p}); err != nil {
		t.Fatal(err)
	}
	if got := p.StopLimit(); got != 2*time.Second {
		t.Fatalf("StopLimit = %s, want 2s", got)
	}
}

// TestExampleYAML keeps the shipped example valid and representative.
func TestExampleYAML(t *testing.T) {
	b, err := os.ReadFile("../../example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	projects, err := config.UnmarshalYAML(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(projects); err != nil {
		t.Fatal(err)
	}
	types := map[string]bool{}
	for _, p := range projects {
		types[p.RunnerType()] = true
	}
	for _, typ := range []string{config.TypeCompose, config.TypeDocker, config.TypeExec} {
		if !types[typ] {
			t.Fatalf("example.yaml has no %s project", typ)
		}
	}
}
