package app_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"portwake/internal/app"
	"portwake/internal/compose/composetest"
	pwconfig "portwake/internal/config"
	"portwake/internal/process"
	"portwake/internal/runner"
)

// TestExecHelperProcess is the binary started by the exec runner in this
// file. It is a no-op unless PW_APP_HELPER=1 is set.
func TestExecHelperProcess(t *testing.T) {
	if os.Getenv("PW_APP_HELPER") != "1" {
		return
	}
	ln, err := net.Listen("tcp", os.Getenv("PW_APP_ADDR"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(2)
	}
	fmt.Println("exec helper ready")
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

func snapshotState(a *app.App, name string) string {
	for _, s := range a.Snapshots() {
		if s.Name == name {
			return string(s.State)
		}
	}
	return ""
}

// TestExecProjectLifecycle runs a real binary through the whole
// start → proxy → idle stop → start again loop.
func TestExecProjectLifecycle(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	pr, err := process.New(process.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	fake := composetest.New()
	idle := pwconfig.Duration(300 * time.Millisecond)
	p := pwconfig.Project{
		Name:        "app",
		Type:        pwconfig.TypeExec,
		Command:     []string{os.Args[0], "-test.run=TestExecHelperProcess"},
		Env:         map[string]string{"PW_APP_HELPER": "1", "PW_APP_ADDR": addr(up)},
		IdleTimeout: &idle,
		Routes: []pwconfig.Route{{
			Listen: entry, Bind: "127.0.0.1", Mode: pwconfig.ModeTCP, Upstream: addr(up),
		}},
	}
	importYAML, err := pwconfig.MarshalYAML([]pwconfig.Project{p})
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(app.Options{
		DBPath:             filepath.Join(t.TempDir(), "portwake.db"),
		WebAddr:            "127.0.0.1:0",
		Runners:            runner.Manager{Compose: fake, Docker: fake, Exec: pr},
		Import:             importYAML,
		Grace:              200 * time.Millisecond,
		ReconcileInterval:  30 * time.Millisecond,
		HealthPollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := a.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pr.Stop(context.Background(), p)
		cancel()
		a.Shutdown(context.Background())
	})

	if got, err := echo(t, entry, "hello"); err != nil || got != "hello" {
		t.Fatalf("echo = %q %v", got, err)
	}
	eventually(t, "running", func() bool { return snapshotState(a, "app") == "running" })
	eventually(t, "idle stop", func() bool { return snapshotState(a, "app") == "stopped" })

	resp, err := http.Get("http://" + a.WebAddr() + "/api/projects/app/logs?lines=20")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "exec helper ready") {
		t.Fatalf("logs: %d %q", resp.StatusCode, body)
	}

	// A new connection starts the same binary again.
	if got, err := echo(t, entry, "again"); err != nil || got != "again" {
		t.Fatalf("second echo = %q %v", got, err)
	}
	eventually(t, "running again", func() bool { return snapshotState(a, "app") == "running" })
}
