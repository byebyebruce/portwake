// Package docker manages one container per project using the docker CLI.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"portwake/internal/config"
	"portwake/internal/runner"
)

// CLI runs the docker binary found on PATH.
type CLI struct {
	Docker string // docker binary; defaults to "docker"
}

var _ runner.Runner = CLI{}
var _ runner.LogSource = CLI{}

// ContainerName returns the name of the container managed for p.
func ContainerName(p config.Project) string {
	if p.ContainerName != "" {
		return p.ContainerName
	}
	return "portwake-" + p.Name
}

func (c CLI) bin() string {
	if c.Docker == "" {
		return "docker"
	}
	return c.Docker
}

func (c CLI) run(ctx context.Context, extra ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin(), extra...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(extra, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

// runCombined returns stdout and stderr together. Container logs may be
// written to either stream, so viewing them must not drop stderr.
func (c CLI) runCombined(ctx context.Context, extra ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin(), extra...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(extra, " "), err, strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

// notFound reports whether the docker error means the container does not
// exist.
func notFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "No such object") || strings.Contains(msg, "No such container")
}

type containerState struct {
	Status  string `json:"Status"`
	Running bool   `json:"Running"`
	Health  *struct {
		Status string `json:"Status"`
	} `json:"Health"`
}

func (c CLI) inspect(ctx context.Context, p config.Project) (containerState, bool, error) {
	out, err := c.run(ctx, "inspect", "--format", "{{json .State}}", ContainerName(p))
	if notFound(err) {
		return containerState{}, false, nil
	}
	if err != nil {
		return containerState{}, false, err
	}
	st, err := parseState(out)
	if err != nil {
		return containerState{}, false, err
	}
	return st, true, nil
}

func parseState(out []byte) (containerState, error) {
	var st containerState
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		return st, fmt.Errorf("parse docker inspect: %w", err)
	}
	return st, nil
}

func (c CLI) Up(ctx context.Context, p config.Project) error {
	st, exists, err := c.inspect(ctx, p)
	if err != nil {
		return err
	}
	if exists {
		if st.Running {
			return nil
		}
		_, err := c.run(ctx, "start", ContainerName(p))
		return err
	}
	_, err = c.run(ctx, c.runArgs(p)...)
	return err
}

// runArgs builds the arguments of `docker run` for p.
func (c CLI) runArgs(p config.Project) []string {
	args := []string{"run", "-d", "--name", ContainerName(p), "--label", "portwake.project=" + p.Name}
	if p.EnvFile != "" {
		path := p.EnvFile
		if !filepath.IsAbs(path) {
			if abs, err := filepath.Abs(path); err == nil {
				path = abs
			}
		}
		args = append(args, "--env-file", path)
	}
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+p.Env[k])
	}
	for _, v := range p.Volumes {
		args = append(args, "-v", v)
	}
	for _, port := range p.Ports {
		args = append(args, "-p", port)
	}
	if p.Network != "" {
		args = append(args, "--network", p.Network)
	}
	args = append(args, p.Image)
	return append(args, p.Command...)
}

func (c CLI) Stop(ctx context.Context, p config.Project) error {
	args := []string{"stop", "--time", strconv.Itoa(stopSeconds(p))}
	if p.StopSignal != "" {
		args = append(args, "--signal", p.StopSignal)
	}
	args = append(args, ContainerName(p))
	_, err := c.run(ctx, args...)
	if notFound(err) {
		return nil
	}
	return err
}

func stopSeconds(p config.Project) int {
	secs := int((p.StopLimit() + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	return secs
}

func (c CLI) Ps(ctx context.Context, p config.Project) ([]runner.ServiceStatus, error) {
	st, exists, err := c.inspect(ctx, p)
	if err != nil || !exists {
		return nil, err
	}
	health := ""
	if st.Health != nil {
		health = st.Health.Status
	}
	state := st.Status
	if st.Running {
		state = "running"
	}
	return []runner.ServiceStatus{{Service: "container", State: state, Health: health}}, nil
}

func (c CLI) Logs(ctx context.Context, p config.Project, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	out, err := c.runCombined(ctx, "logs", "--tail", strconv.Itoa(lines), ContainerName(p))
	if notFound(err) {
		return "", nil
	}
	return string(out), err
}
