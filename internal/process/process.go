//go:build unix

// Package process manages foreground binaries started directly by
// portwake. Each process runs in its own process group; a small state file
// lets a restarted portwake adopt processes that are still running.
package process

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"portwake/internal/config"
	"portwake/internal/runner"
)

// DefaultMaxLogBytes is how large a project's log may get before it is
// rotated at the next fresh start.
const DefaultMaxLogBytes = 4 << 20

// Options configures a Runner.
type Options struct {
	// StateDir stores pid files and logs; it must be writable.
	StateDir string
	// MaxLogBytes rotates the log at a fresh start once it is larger than
	// this. Zero means DefaultMaxLogBytes.
	MaxLogBytes int64
}

// Runner starts and stops foreground binaries.
type Runner struct {
	stateDir string
	maxLog   int64

	mu    sync.Mutex
	procs map[string]*proc
}

type proc struct {
	name string
	pgid int
	cmd  *exec.Cmd
	done chan struct{}
}

type state struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Command   []string  `json:"command"`
}

var _ runner.Runner = (*Runner)(nil)
var _ runner.LogSource = (*Runner)(nil)
var _ runner.Alive = (*Runner)(nil)

// New creates a runner and the state directory.
func New(opts Options) (*Runner, error) {
	if opts.StateDir == "" {
		return nil, errors.New("process runner: state dir is required")
	}
	if opts.MaxLogBytes <= 0 {
		opts.MaxLogBytes = DefaultMaxLogBytes
	}
	if err := os.MkdirAll(filepath.Join(opts.StateDir, "logs"), 0o700); err != nil {
		return nil, fmt.Errorf("process runner: %w", err)
	}
	return &Runner{
		stateDir: opts.StateDir,
		maxLog:   opts.MaxLogBytes,
		procs:    map[string]*proc{},
	}, nil
}

func (r *Runner) statePath(name string) string { return filepath.Join(r.stateDir, name+".json") }
func (r *Runner) logPath(name string) string   { return filepath.Join(r.stateDir, "logs", name+".log") }

// Up starts the command unless the project's process group is already
// alive (for example after a portwake restart).
func (r *Runner) Up(ctx context.Context, p config.Project) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(p.Command) == 0 {
		return errors.New("exec project has no command")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.loadState(p.Name); ok && alive(st.PID) {
		return nil
	}
	env, err := buildEnv(p)
	if err != nil {
		return err
	}
	if err := r.rotateLog(p.Name); err != nil {
		return err
	}
	logf, err := os.OpenFile(r.logPath(p.Name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logf.Close()

	cmd := exec.Command(p.Command[0], p.Command[1:]...)
	cmd.Dir = p.Dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", p.Command[0], err)
	}
	pr := &proc{name: p.Name, pgid: cmd.Process.Pid, cmd: cmd, done: make(chan struct{})}
	r.procs[p.Name] = pr
	go r.wait(pr)
	if err := r.writeState(p, pr.pgid); err != nil {
		// Never leave a process we cannot track running.
		_ = syscall.Kill(-pr.pgid, syscall.SIGKILL)
		return err
	}
	return nil
}

// wait reaps the child and clears its state file.
func (r *Runner) wait(pr *proc) {
	_ = pr.cmd.Wait()
	close(pr.done)
	r.mu.Lock()
	if r.procs[pr.name] == pr {
		delete(r.procs, pr.name)
	}
	r.mu.Unlock()
	r.removeStateIf(pr.name, pr.pgid)
}

// Stop sends the configured stop signal to the process group and kills it
// if it does not exit within stop_timeout.
func (r *Runner) Stop(ctx context.Context, p config.Project) error {
	r.mu.Lock()
	pr := r.procs[p.Name]
	r.mu.Unlock()

	var pgid int
	var done <-chan struct{}
	if pr != nil {
		pgid, done = pr.pgid, pr.done
	} else {
		st, ok := r.loadState(p.Name)
		if !ok {
			return nil
		}
		pgid = st.PID
	}
	if !alive(pgid) {
		r.removeStateIf(p.Name, pgid)
		return nil
	}
	if err := syscall.Kill(-pgid, sigFor(p.StopSignal)); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal process group %d: %w", pgid, err)
	}
	if !waitGone(ctx, pgid, done, p.StopLimit()) {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		if !waitGone(context.Background(), pgid, done, 5*time.Second) {
			return fmt.Errorf("process group %d did not stop", pgid)
		}
	}
	r.removeStateIf(p.Name, pgid)
	return nil
}

// Ps reports the process as a single "process" service.
func (r *Runner) Ps(ctx context.Context, p config.Project) ([]runner.ServiceStatus, error) {
	st, ok := r.loadState(p.Name)
	if !ok || !alive(st.PID) {
		return nil, nil
	}
	return []runner.ServiceStatus{{Service: "process", State: "running"}}, nil
}

// Alive reports whether the tracked process group still exists.
func (r *Runner) Alive(p config.Project) bool {
	st, ok := r.loadState(p.Name)
	return ok && alive(st.PID)
}

// Logs returns the last lines written to the project's log file. The log
// can grow to gigabytes, so only a bounded tail is read.
func (r *Runner) Logs(ctx context.Context, p config.Project, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	f, err := os.Open(r.logPath(p.Name))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	const maxTail = 1 << 20
	start := int64(0)
	if fi.Size() > maxTail {
		start = fi.Size() - maxTail
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	s := string(b)
	if start > 0 {
		// The first line is probably cut in half.
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return lastLines(s, lines), nil
}

func lastLines(s string, n int) string {
	if n <= 0 {
		n = 200
	}
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

// rotateLog keeps one previous run's log so a fresh start does not
// truncate the output that might explain a crash.
func (r *Runner) rotateLog(name string) error {
	path := r.logPath(name)
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Size() == 0 || fi.Size() < r.maxLog {
		return nil
	}
	return os.Rename(path, path+".1")
}

func (r *Runner) loadState(name string) (state, bool) {
	b, err := os.ReadFile(r.statePath(name))
	if err != nil {
		return state{}, false
	}
	var st state
	if json.Unmarshal(b, &st) != nil || st.PID <= 0 {
		return state{}, false
	}
	return st, true
}

func (r *Runner) writeState(p config.Project, pid int) error {
	b, err := json.Marshal(state{PID: pid, StartedAt: time.Now(), Command: p.Command})
	if err != nil {
		return err
	}
	path := r.statePath(p.Name)
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

func (r *Runner) removeStateIf(name string, pid int) {
	if st, ok := r.loadState(name); ok && st.PID == pid {
		_ = os.Remove(r.statePath(name))
	}
}

// alive reports whether any process in the group still exists. A process
// that is our unreaped child shows up until the wait goroutine reaps it.
func alive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// waitGone polls until the process group is gone, ctx is cancelled or d
// elapses. It returns true when the group is gone.
func waitGone(ctx context.Context, pgid int, done <-chan struct{}, d time.Duration) bool {
	if !alive(pgid) {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			done = nil
			if !alive(pgid) {
				return true
			}
		case <-tick.C:
			if !alive(pgid) {
				return true
			}
		case <-ctx.Done():
			return !alive(pgid)
		case <-timer.C:
			return !alive(pgid)
		}
	}
}

func sigFor(name string) syscall.Signal {
	canon, _ := config.NormalizeSignal(name)
	switch canon {
	case "SIGHUP":
		return syscall.SIGHUP
	case "SIGINT":
		return syscall.SIGINT
	case "SIGQUIT":
		return syscall.SIGQUIT
	case "SIGKILL":
		return syscall.SIGKILL
	case "SIGUSR1":
		return syscall.SIGUSR1
	case "SIGUSR2":
		return syscall.SIGUSR2
	default:
		return syscall.SIGTERM
	}
}

// buildEnv merges the parent environment, an optional env file and the
// project's env map (later sources win).
func buildEnv(p config.Project) ([]string, error) {
	vals := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vals[k] = v
		}
	}
	if p.EnvFile != "" {
		path := p.EnvFile
		if !filepath.IsAbs(path) && p.Dir != "" {
			path = filepath.Join(p.Dir, path)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("env_file: %w", err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, v, ok := parseEnvLine(sc.Text()); ok {
				vals[k] = v
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("env_file: %w", err)
		}
	}
	for k, v := range p.Env {
		vals[k] = v
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+vals[k])
	}
	return out, nil
}

// parseEnvLine parses one KEY=VALUE line. Blank lines and comments are
// skipped; a bare KEY inherits its value from the environment.
func parseEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		if k = strings.TrimSpace(k); k != "" {
			return k, os.Getenv(k), true
		}
		return "", "", false
	}
	k = strings.TrimSpace(k)
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	}
	return k, v, k != ""
}
