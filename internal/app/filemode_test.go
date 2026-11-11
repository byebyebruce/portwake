package app_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"portwake/internal/app"
	"portwake/internal/compose/composetest"
	"portwake/internal/runner"
	"portwake/internal/store"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readConfig(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFileModeLoadsConfigAndPersistsWebEdits(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	path := writeConfig(t, config(projectYAML("web", entry, up, "    idle_timeout: 5m\n")))
	h := startFile(t, path)

	if got, err := echo(t, entry, "file"); err != nil || got != "file" {
		t.Fatalf("echo from file config = %q %v", got, err)
	}

	// Create through the API: the YAML file is updated, not the database.
	e2, u2 := freePort(t), freePort(t)
	if code, body := h.do("POST", "/api/projects", "application/json", projectJSON("api", e2, u2)); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	saved := readConfig(t, path)
	if !strings.Contains(saved, "name: web") || !strings.Contains(saved, "name: api") {
		t.Fatalf("file after create:\n%s", saved)
	}
	st, err := store.Open(h.db)
	if err != nil {
		t.Fatal(err)
	}
	if dbProjects, err := st.LoadProjects(); err != nil || len(dbProjects) != 0 {
		t.Fatalf("db projects = %+v, %v; want none", dbProjects, err)
	}
	st.Close()

	// A rejected change must leave the file untouched.
	if code, _ := h.do("POST", "/api/projects", "application/json", projectJSON("dup", entry, up)); code != http.StatusBadRequest {
		t.Fatalf("duplicate port = %d", code)
	}
	if after := readConfig(t, path); after != saved {
		t.Fatalf("failed change modified the file:\n%s", after)
	}

	// Updates are written through; stop first so the next connection starts
	// the project with the new upstream.
	if code, body := h.do("POST", "/api/projects/web/stop", "", ""); code != http.StatusAccepted {
		t.Fatalf("stop: %d %s", code, body)
	}
	eventually(t, "stopped", func() bool { return h.state("web") == "stopped" })
	up2 := freePort(t)
	if code, body := h.do("PUT", "/api/projects/web", "application/json", projectJSON("web", entry, up2)); code != http.StatusOK {
		t.Fatalf("update: %d %s", code, body)
	}
	if saved := readConfig(t, path); !strings.Contains(saved, fmt.Sprintf("127.0.0.1:%d", up2)) {
		t.Fatalf("file after update:\n%s", saved)
	}
	if got, err := echo(t, entry, "updated"); err != nil || got != "updated" {
		t.Fatalf("echo after update = %q %v", got, err)
	}

	// Deleting removes the project from the file too.
	if code, body := h.do("DELETE", "/api/projects/api", "", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, body)
	}
	if saved := readConfig(t, path); strings.Contains(saved, "name: api") {
		t.Fatalf("file still has deleted project:\n%s", saved)
	}

	// Restarting reads the file again.
	h.restart()
	if ps := h.projects(); len(ps) != 1 || ps[0].Name != "web" {
		t.Fatalf("projects after restart = %+v", ps)
	}
	if got, err := echo(t, entry, "again"); err != nil || got != "again" {
		t.Fatalf("echo after restart = %q %v", got, err)
	}

	// Importing YAML through the web API replaces the file content.
	ie, iu := freePort(t), freePort(t)
	if code, body := h.do("PUT", "/api/config.yaml", "application/yaml", config(projectYAML("imp", ie, iu, ""))); code != http.StatusOK {
		t.Fatalf("import: %d %s", code, body)
	}
	saved = readConfig(t, path)
	if !strings.Contains(saved, "name: imp") || strings.Contains(saved, "name: web") {
		t.Fatalf("file after import:\n%s", saved)
	}
}

func TestFileModeWriteFailureRollsBack(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	path := writeConfig(t, config(projectYAML("web", entry, up, "")))
	h := startFile(t, path)

	// Make the atomic rename fail by replacing the file with a directory.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	e2, u2 := freePort(t), freePort(t)
	if code, body := h.do("POST", "/api/projects", "application/json", projectJSON("api", e2, u2)); code != http.StatusInternalServerError {
		t.Fatalf("create with unwritable config = %d %s", code, body)
	}
	if _, err := net.DialTimeout("tcp", addr(e2), 200*time.Millisecond); err == nil {
		t.Fatal("new listener survived the rollback")
	}
	if got, err := echo(t, entry, "ok"); err != nil || got != "ok" {
		t.Fatalf("old config no longer served: %q %v", got, err)
	}
	if ps := h.projects(); len(ps) != 1 || ps[0].Name != "web" {
		t.Fatalf("config changed despite failure: %+v", ps)
	}
}

func fakeRunners() runner.Manager {
	f := composetest.New()
	return runner.Manager{Compose: f, Docker: f, Exec: f}
}

func TestFileModeRejectsImportOption(t *testing.T) {
	_, err := app.New(app.Options{
		Runners:    fakeRunners(),
		ConfigPath: "config.yaml",
		Import:     []byte("projects: []\n"),
	})
	if err == nil || !strings.Contains(err.Error(), "import") {
		t.Fatalf("New error = %v, want import conflict", err)
	}
}

func TestFileModeMissingConfigFailsToStart(t *testing.T) {
	a, err := app.New(app.Options{
		DBPath:     filepath.Join(t.TempDir(), "portwake.db"),
		WebAddr:    "127.0.0.1:0",
		Runners:    fakeRunners(),
		ConfigPath: filepath.Join(t.TempDir(), "missing.yaml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err == nil {
		a.Shutdown(context.Background())
		t.Fatal("Start succeeded with a missing config file")
	}
}
