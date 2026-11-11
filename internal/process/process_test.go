package process

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"portwake/internal/config"
)

// TestHelperProcess is the child process used by the tests. It is a no-op
// unless PW_PROCESS_HELPER=1 is set.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("PW_PROCESS_HELPER") != "1" {
		return
	}
	if os.Getenv("PW_HELPER_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "boom: cannot start")
		os.Exit(3)
	}
	addr := os.Getenv("PW_HELPER_ADDR")
	if os.Getenv("PW_HELPER_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(2)
	}
	fmt.Println("helper ready on", addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			io.Copy(c, c)
		}()
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func helperProject(addr string) config.Project {
	return config.Project{
		Name:    "app",
		Type:    config.TypeExec,
		Command: []string{os.Args[0], "-test.run=TestHelperProcess"},
		Env: map[string]string{
			"PW_PROCESS_HELPER": "1",
			"PW_HELPER_ADDR":    addr,
		},
		Routes: []config.Route{{Listen: 8080, Bind: "127.0.0.1", Upstream: addr}},
	}
}

func newRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return r, dir
}

func waitDial(t *testing.T, addr string, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		ok := err == nil
		if c != nil {
			c.Close()
		}
		if ok == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("addr %s never became dialable=%v", addr, want)
}

func waitAlive(t *testing.T, r *Runner, p config.Project, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.Alive(p) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Alive = %v, want %v", !want, want)
}

func TestLifecycle(t *testing.T) {
	r, dir := newRunner(t)
	addr := freeAddr(t)
	p := helperProject(addr)
	t.Cleanup(func() { _ = r.Stop(context.Background(), p) })

	if err := r.Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	waitDial(t, addr, true)

	st, err := r.Ps(context.Background(), p)
	if err != nil || len(st) != 1 || st[0].State != "running" {
		t.Fatalf("Ps = %+v, %v", st, err)
	}
	if !r.Alive(p) {
		t.Fatal("Alive = false after start")
	}
	if _, err := os.Stat(filepath.Join(dir, "app.json")); err != nil {
		t.Fatalf("state file: %v", err)
	}

	var logs string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		logs, err = r.Logs(context.Background(), p, 10)
		if err == nil && strings.Contains(logs, "helper ready") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logs, "helper ready") {
		t.Fatalf("logs = %q", logs)
	}

	if err := r.Stop(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	waitDial(t, addr, false)
	if r.Alive(p) {
		t.Fatal("Alive = true after stop")
	}
	st, err = r.Ps(context.Background(), p)
	if err != nil || st != nil {
		t.Fatalf("Ps after stop = %+v, %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.json")); !os.IsNotExist(err) {
		t.Fatalf("state file survived stop: %v", err)
	}
	if err := r.Stop(context.Background(), p); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

func TestRestartAdoptsRunningProcess(t *testing.T) {
	r1, dir := newRunner(t)
	addr := freeAddr(t)
	p := helperProject(addr)
	t.Cleanup(func() { _ = r1.Stop(context.Background(), p) })
	if err := r1.Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	waitDial(t, addr, true)

	r2, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	st, err := r2.Ps(context.Background(), p)
	if err != nil || len(st) != 1 || st[0].State != "running" {
		t.Fatalf("adopted Ps = %+v, %v", st, err)
	}
	if err := r2.Up(context.Background(), p); err != nil {
		t.Fatalf("Up when already running: %v", err)
	}
	if err := r2.Stop(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	waitDial(t, addr, false)
	if r1.Alive(p) {
		t.Fatal("original runner still sees the process as alive")
	}
}

func TestEarlyExitIsReported(t *testing.T) {
	r, _ := newRunner(t)
	addr := freeAddr(t)
	p := helperProject(addr)
	p.Env["PW_HELPER_FAIL"] = "1"
	if err := r.Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	waitAlive(t, r, p, false)
	deadline := time.Now().Add(5 * time.Second)
	var logs string
	for time.Now().Before(deadline) {
		logs, _ = r.Logs(context.Background(), p, 10)
		if strings.Contains(logs, "boom") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logs, "boom") {
		t.Fatalf("logs = %q", logs)
	}
}

func TestStopEscalatesToKill(t *testing.T) {
	r, _ := newRunner(t)
	addr := freeAddr(t)
	p := helperProject(addr)
	p.Env["PW_HELPER_IGNORE_TERM"] = "1"
	stop := config.Duration(200 * time.Millisecond)
	p.StopTimeout = &stop
	t.Cleanup(func() { _ = r.Stop(context.Background(), p) })
	if err := r.Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	waitDial(t, addr, true)
	start := time.Now()
	if err := r.Stop(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("stop returned before the timeout: %s", d)
	}
	waitDial(t, addr, false)
}

func TestMissingCommandBinary(t *testing.T) {
	r, _ := newRunner(t)
	p := config.Project{Name: "app", Type: config.TypeExec, Command: []string{"/nonexistent/portwake-test-binary"}}
	if err := r.Up(context.Background(), p); err == nil {
		t.Fatal("Up succeeded for a missing binary")
	}
	if r.Alive(p) {
		t.Fatal("missing binary reported alive")
	}
}

func TestBuildEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	content := "# comment\nexport FROM_FILE=file\nQUOTED=\"a b\"\nBARE_PATH\n"
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BARE_PATH", "/bin")
	t.Setenv("OVERRIDDEN", "parent")
	p := config.Project{
		Dir:     dir,
		EnvFile: ".env",
		Env:     map[string]string{"FROM_ENV": "env", "OVERRIDDEN": "project"},
	}
	env, err := buildEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["FROM_FILE"] != "file" || got["QUOTED"] != "a b" || got["BARE_PATH"] != "/bin" {
		t.Fatalf("env = %v", got)
	}
	if got["FROM_ENV"] != "env" || got["OVERRIDDEN"] != "project" {
		t.Fatalf("precedence wrong: %v", got)
	}
}

func TestLastLines(t *testing.T) {
	got := lastLines("a\nb\nc\n", 2)
	if got != "b\nc\n" {
		t.Fatalf("lastLines = %q", got)
	}
	if got := lastLines("", 2); got != "" {
		t.Fatalf("empty lastLines = %q", got)
	}
}

func TestLogsReadsBoundedTail(t *testing.T) {
	r, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const name = "big"
	f, err := os.Create(r.logPath(name))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(f, "line %06d %s\n", i, strings.Repeat("x", 10))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := r.Logs(context.Background(), config.Project{Name: name}, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := "line 199999 " + strings.Repeat("x", 10) + "\n"
	if !strings.HasSuffix(out, want) {
		t.Fatalf("tail = %q, want suffix %q", out, want)
	}
	if strings.Contains(out, "line 000000") {
		t.Fatal("Logs read the whole file instead of its tail")
	}
	if len(out) > 4096 {
		t.Fatalf("tail is %d bytes, want a bounded read", len(out))
	}
}
