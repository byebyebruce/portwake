// Package config defines the managed-project model, defaults and validation.
package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultIdleTimeout  = 15 * time.Minute
	DefaultStartTimeout = 3 * time.Minute
	DefaultStopTimeout  = 10 * time.Second
	DefaultBind         = "0.0.0.0"

	ModeTCP  = "tcp"
	ModeHTTP = "http"

	// TypeCompose manages a docker compose project (the default).
	TypeCompose = "compose"
	// TypeDocker manages one docker container started with `docker run`.
	TypeDocker = "docker"
	// TypeExec manages a foreground process started directly.
	TypeExec = "exec"
)

// Duration is a time.Duration that (un)marshals as a Go duration string ("15m").
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	s := string(b)
	if s == "0" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// Route maps one entry port to an upstream address of a project.
type Route struct {
	Listen        int    `json:"listen" yaml:"listen"`
	Bind          string `json:"bind,omitempty" yaml:"bind,omitempty"`
	Mode          string `json:"mode,omitempty" yaml:"mode,omitempty"`
	Upstream      string `json:"upstream" yaml:"upstream"`
	HealthService string `json:"health_service,omitempty" yaml:"health_service,omitempty"`
}

// Project is a project managed by portwake. Type selects the runner and
// therefore which of the following fields are meaningful: compose uses
// dir/files/profiles, docker uses image/command/env/volumes/ports/network,
// and exec uses command/dir/env. env_file, stop_signal, stop_timeout and
// the lifecycle fields apply to the types noted in the README.
type Project struct {
	Name string `json:"name" yaml:"name"`
	Type string `json:"type,omitempty" yaml:"type,omitempty"`

	// compose
	Dir      string   `json:"dir,omitempty" yaml:"dir,omitempty"`
	Files    []string `json:"files,omitempty" yaml:"files,omitempty"`
	Profiles []string `json:"profiles,omitempty" yaml:"profiles,omitempty"`

	// docker
	Image         string   `json:"image,omitempty" yaml:"image,omitempty"`
	ContainerName string   `json:"container_name,omitempty" yaml:"container_name,omitempty"`
	Volumes       []string `json:"volumes,omitempty" yaml:"volumes,omitempty"`
	Ports         []string `json:"ports,omitempty" yaml:"ports,omitempty"`
	Network       string   `json:"network,omitempty" yaml:"network,omitempty"`

	// docker and exec
	Command     []string          `json:"command,omitempty" yaml:"command,omitempty"`
	Env         map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	EnvFile     string            `json:"env_file,omitempty" yaml:"env_file,omitempty"`
	StopSignal  string            `json:"stop_signal,omitempty" yaml:"stop_signal,omitempty"`
	StopTimeout *Duration         `json:"stop_timeout,omitempty" yaml:"stop_timeout,omitempty"`

	IdleTimeout  *Duration `json:"idle_timeout,omitempty" yaml:"idle_timeout,omitempty"`
	StartTimeout *Duration `json:"start_timeout,omitempty" yaml:"start_timeout,omitempty"`
	Routes       []Route   `json:"routes" yaml:"routes"`
}

// RunnerType returns the project type, defaulting to compose for
// configurations written before runners were introduced.
func (p Project) RunnerType() string {
	if p.Type == "" {
		return TypeCompose
	}
	return p.Type
}

// Idle returns the effective idle timeout; 0 means never stop automatically.
func (p Project) Idle() time.Duration {
	if p.IdleTimeout == nil {
		return DefaultIdleTimeout
	}
	return time.Duration(*p.IdleTimeout)
}

// StartLimit returns the effective start timeout.
func (p Project) StartLimit() time.Duration {
	if p.StartTimeout == nil || *p.StartTimeout <= 0 {
		return DefaultStartTimeout
	}
	return time.Duration(*p.StartTimeout)
}

// StopLimit returns the effective stop timeout: how long to wait after the
// stop signal before killing the project.
func (p Project) StopLimit() time.Duration {
	if p.StopTimeout == nil || *p.StopTimeout <= 0 {
		return DefaultStopTimeout
	}
	return time.Duration(*p.StopTimeout)
}

// Normalize fills route defaults in place.
func (p *Project) Normalize() {
	for i := range p.Routes {
		r := &p.Routes[i]
		if r.Bind == "" {
			r.Bind = DefaultBind
		}
		if r.Mode == "" {
			r.Mode = ModeTCP
		}
	}
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
var containerNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// signalNames maps a signal name without the SIG prefix to its canonical
// form. Only signals that make sense as a stop signal are listed.
var signalNames = map[string]string{
	"HUP":  "SIGHUP",
	"INT":  "SIGINT",
	"QUIT": "SIGQUIT",
	"KILL": "SIGKILL",
	"TERM": "SIGTERM",
	"USR1": "SIGUSR1",
	"USR2": "SIGUSR2",
}

// NormalizeSignal returns the canonical form ("SIGTERM") of a stop signal
// and whether it is valid. "TERM", "term" and "SIGTERM" are equivalent.
func NormalizeSignal(s string) (string, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "SIG")
	canon, ok := signalNames[s]
	return canon, ok
}

// ErrInvalid marks errors caused by a configuration that cannot be applied.
var ErrInvalid = errors.New("invalid configuration")

// Validate fills route defaults in place (see Normalize) and checks a
// complete project set. Errors wrap ErrInvalid.
func Validate(projects []Project) error {
	names := map[string]bool{}
	ports := map[int]string{}
	var errs []error
	for i := range projects {
		p := &projects[i]
		p.Normalize()
		if !nameRE.MatchString(p.Name) {
			errs = append(errs, fmt.Errorf("project %q: name must match %s", p.Name, nameRE))
		}
		if names[p.Name] {
			errs = append(errs, fmt.Errorf("project %q: duplicate name", p.Name))
		}
		names[p.Name] = true
		errs = append(errs, validateType(p)...)
		if p.IdleTimeout != nil && *p.IdleTimeout < 0 {
			errs = append(errs, fmt.Errorf("project %q: idle_timeout must not be negative", p.Name))
		}
		if p.StopTimeout != nil && *p.StopTimeout < 0 {
			errs = append(errs, fmt.Errorf("project %q: stop_timeout must not be negative", p.Name))
		}
		if p.StopSignal != "" {
			if canon, ok := NormalizeSignal(p.StopSignal); ok {
				p.StopSignal = canon
			} else {
				errs = append(errs, fmt.Errorf("project %q: invalid stop_signal %q", p.Name, p.StopSignal))
			}
		}
		if len(p.Routes) == 0 {
			errs = append(errs, fmt.Errorf("project %q: at least one route is required", p.Name))
		}
		for _, r := range p.Routes {
			if r.HealthService != "" && p.RunnerType() != TypeCompose {
				errs = append(errs, fmt.Errorf("project %q: port %d: health_service is only supported by compose projects", p.Name, r.Listen))
			}
			if r.Listen < 1 || r.Listen > 65535 {
				errs = append(errs, fmt.Errorf("project %q: invalid listen port %d", p.Name, r.Listen))
			} else if other, ok := ports[r.Listen]; ok {
				errs = append(errs, fmt.Errorf("project %q: port %d already used by project %q", p.Name, r.Listen, other))
			}
			ports[r.Listen] = p.Name
			if net.ParseIP(r.Bind) == nil {
				errs = append(errs, fmt.Errorf("project %q: port %d: invalid bind address %q", p.Name, r.Listen, r.Bind))
			}
			if r.Mode != ModeTCP && r.Mode != ModeHTTP {
				errs = append(errs, fmt.Errorf("project %q: port %d: mode must be tcp or http", p.Name, r.Listen))
			}
			if host, port, err := net.SplitHostPort(r.Upstream); err != nil || host == "" {
				errs = append(errs, fmt.Errorf("project %q: port %d: invalid upstream %q", p.Name, r.Listen, r.Upstream))
			} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				errs = append(errs, fmt.Errorf("project %q: port %d: invalid upstream port %q", p.Name, r.Listen, port))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalid, errors.Join(errs...))
	}
	return nil
}

// validateType checks the fields that belong to the project's runner type
// and rejects fields that the runner would silently ignore.
func validateType(p *Project) []error {
	var errs []error
	errf := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("project %q: "+format, append([]any{p.Name}, args...)...))
	}
	if len(p.Command) > 0 && p.Command[0] == "" {
		errf("command[0] must not be empty")
	}
	for k := range p.Env {
		if !envKeyRE.MatchString(k) {
			errf("invalid env key %q", k)
		}
	}
	if p.ContainerName != "" && !containerNameRE.MatchString(p.ContainerName) {
		errf("invalid container_name %q", p.ContainerName)
	}
	switch p.RunnerType() {
	case TypeCompose:
		if p.Dir == "" {
			errf("dir is required for compose projects")
		}
		if p.Image != "" {
			errf("image is only supported by docker projects")
		}
		if p.ContainerName != "" {
			errf("container_name is only supported by docker projects")
		}
		if len(p.Command) > 0 {
			errf("command is only supported by docker and exec projects")
		}
		if len(p.Env) > 0 {
			errf("env is only supported by docker and exec projects")
		}
		if len(p.Volumes) > 0 {
			errf("volumes is only supported by docker projects")
		}
		if len(p.Ports) > 0 {
			errf("ports is only supported by docker projects")
		}
		if p.Network != "" {
			errf("network is only supported by docker projects")
		}
		if p.StopSignal != "" {
			errf("stop_signal is only supported by docker and exec projects")
		}
	case TypeDocker:
		if p.Image == "" {
			errf("image is required for docker projects")
		}
		if p.Dir != "" {
			errf("dir is only supported by compose and exec projects")
		}
		if len(p.Files) > 0 {
			errf("files is only supported by compose projects")
		}
		if len(p.Profiles) > 0 {
			errf("profiles is only supported by compose projects")
		}
	case TypeExec:
		if len(p.Command) == 0 {
			errf("command is required for exec projects")
		}
		if p.Image != "" {
			errf("image is only supported by docker projects")
		}
		if p.ContainerName != "" {
			errf("container_name is only supported by docker projects")
		}
		if len(p.Files) > 0 {
			errf("files is only supported by compose projects")
		}
		if len(p.Profiles) > 0 {
			errf("profiles is only supported by compose projects")
		}
		if len(p.Volumes) > 0 {
			errf("volumes is only supported by docker projects")
		}
		if len(p.Ports) > 0 {
			errf("ports is only supported by docker projects")
		}
		if p.Network != "" {
			errf("network is only supported by docker projects")
		}
	default:
		errf("type must be compose, docker or exec")
	}
	return errs
}

// File is the YAML import/export document.
type File struct {
	Projects []Project `yaml:"projects"`
}

func MarshalYAML(projects []Project) ([]byte, error) {
	return yaml.Marshal(File{Projects: projects})
}

func UnmarshalYAML(b []byte) ([]Project, error) {
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.Projects == nil {
		f.Projects = []Project{}
	}
	return f.Projects, nil
}
