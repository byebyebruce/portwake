//go:build integration

package app_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"portwake/internal/app"
	"portwake/internal/compose"
	"portwake/internal/runner"
)

// TestRealComposeLifecycle drives a real nginx compose project:
// request → up → forward → idle → stop.
func TestRealComposeLifecycle(t *testing.T) {
	dir := t.TempDir()
	entry, up := freePort(t), freePort(t)
	name := fmt.Sprintf("portwake-it-%d", time.Now().UnixNano()%100000)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(fmt.Sprintf(`services:
  web:
    image: nginx:alpine
    ports: ["127.0.0.1:%d:80"]
`, up)), 0o644)
	t.Cleanup(func() { exec.Command("docker", "compose", "-p", name, "down", "-t", "1").Run() })

	a, err := app.New(app.Options{
		DBPath:  filepath.Join(t.TempDir(), "db"),
		WebAddr: "127.0.0.1:0",
		Runners: runner.Manager{Compose: compose.CLI{}},
		Import: []byte(fmt.Sprintf(`projects:
  - name: %s
    dir: %s
    idle_timeout: 3s
    routes:
      - listen: %d
        bind: 127.0.0.1
        mode: http
        upstream: 127.0.0.1:%d
`, name, dir, entry, up)),
		ReconcileInterval:  time.Second,
		HealthPollInterval: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Shutdown(context.Background())

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", entry))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "nginx") {
		t.Fatalf("got %d %.100s", resp.StatusCode, body)
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := compose.CLI{}.Ps(context.Background(), a.Projects()[0])
		if !runner.IsRunning(st) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("project not stopped after idle timeout")
}
