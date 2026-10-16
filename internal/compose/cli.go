package compose

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"dcpentry/internal/config"
)

// CLI runs the `docker compose` command found on PATH.
type CLI struct {
	Docker string // docker binary; defaults to "docker"
}

func (c CLI) base(p config.Project) []string {
	args := []string{"compose", "-p", p.Name}
	for _, f := range p.Files {
		args = append(args, "-f", f)
	}
	if p.EnvFile != "" {
		args = append(args, "--env-file", p.EnvFile)
	}
	for _, pr := range p.Profiles {
		args = append(args, "--profile", pr)
	}
	return args
}

func (c CLI) run(ctx context.Context, p config.Project, extra ...string) ([]byte, error) {
	bin := c.Docker
	if bin == "" {
		bin = "docker"
	}
	args := append(c.base(p), extra...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = p.Dir
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

func (c CLI) Up(ctx context.Context, p config.Project) error {
	_, err := c.run(ctx, p, "up", "-d")
	return err
}

func (c CLI) Stop(ctx context.Context, p config.Project) error {
	_, err := c.run(ctx, p, "stop")
	return err
}

func (c CLI) Ps(ctx context.Context, p config.Project) ([]ServiceStatus, error) {
	out, err := c.run(ctx, p, "ps", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parsePs(out)
}

type psRow struct {
	Service string
	State   string
	Health  string
}

// parsePs accepts both the JSON array (older compose) and the
// one-object-per-line output (newer compose) of `ps --format json`.
func parsePs(out []byte) ([]ServiceStatus, error) {
	out = bytes.TrimSpace(out)
	var rows []psRow
	if len(out) == 0 {
		return nil, nil
	}
	if out[0] == '[' {
		if err := json.Unmarshal(out, &rows); err != nil {
			return nil, fmt.Errorf("parse compose ps: %w", err)
		}
	} else {
		sc := bufio.NewScanner(bytes.NewReader(out))
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var r psRow
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, fmt.Errorf("parse compose ps: %w", err)
			}
			rows = append(rows, r)
		}
	}
	st := make([]ServiceStatus, 0, len(rows))
	for _, r := range rows {
		st = append(st, ServiceStatus{Service: r.Service, State: r.State, Health: r.Health})
	}
	return st, nil
}
