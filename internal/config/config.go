// Package config defines the managed-project model, defaults and validation.
package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultIdleTimeout  = 15 * time.Minute
	DefaultStartTimeout = 3 * time.Minute
	DefaultBind         = "0.0.0.0"

	ModeTCP  = "tcp"
	ModeHTTP = "http"
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

// Project is a docker compose project managed by dcpentry.
type Project struct {
	Name         string    `json:"name" yaml:"name"`
	Dir          string    `json:"dir" yaml:"dir"`
	Files        []string  `json:"files,omitempty" yaml:"files,omitempty"`
	EnvFile      string    `json:"env_file,omitempty" yaml:"env_file,omitempty"`
	Profiles     []string  `json:"profiles,omitempty" yaml:"profiles,omitempty"`
	IdleTimeout  *Duration `json:"idle_timeout,omitempty" yaml:"idle_timeout,omitempty"`
	StartTimeout *Duration `json:"start_timeout,omitempty" yaml:"start_timeout,omitempty"`
	Routes       []Route   `json:"routes" yaml:"routes"`
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
		if p.Dir == "" {
			errs = append(errs, fmt.Errorf("project %q: dir is required", p.Name))
		}
		if p.IdleTimeout != nil && *p.IdleTimeout < 0 {
			errs = append(errs, fmt.Errorf("project %q: idle_timeout must not be negative", p.Name))
		}
		if len(p.Routes) == 0 {
			errs = append(errs, fmt.Errorf("project %q: at least one route is required", p.Name))
		}
		for _, r := range p.Routes {
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
