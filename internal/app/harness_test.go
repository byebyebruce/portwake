package app_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"portwake/internal/app"
	"portwake/internal/compose/composetest"
)

type harness struct {
	t          *testing.T
	fake       *composetest.Fake
	app        *app.App
	db         string
	configPath string
	cancel     context.CancelFunc
	done       chan error
}

// start boots a full portwake with a fake compose runner and the given YAML
// imported as the initial configuration.
func start(t *testing.T, yaml string) *harness {
	t.Helper()
	h := &harness{t: t, fake: composetest.New(), db: filepath.Join(t.TempDir(), "portwake.db")}
	h.boot(yaml)
	return h
}

// startFile boots portwake in file mode, with path as the live configuration.
func startFile(t *testing.T, path string) *harness {
	t.Helper()
	h := &harness{t: t, fake: composetest.New(), db: filepath.Join(t.TempDir(), "portwake.db"), configPath: path}
	h.boot("")
	return h
}

func (h *harness) boot(yaml string) {
	h.t.Helper()
	opts := app.Options{
		DBPath:             h.db,
		WebAddr:            "127.0.0.1:0",
		Runner:             h.fake,
		Grace:              200 * time.Millisecond,
		ReconcileInterval:  30 * time.Millisecond,
		HealthPollInterval: 10 * time.Millisecond,
	}
	if h.configPath != "" {
		opts.ConfigPath = h.configPath
	} else if yaml != "" {
		opts.Import = []byte(yaml)
	}
	a, err := app.New(opts)
	if err != nil {
		h.t.Fatalf("app.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := a.Start(ctx); err != nil {
		cancel()
		h.t.Fatalf("app.Start: %v", err)
	}
	h.app, h.cancel = a, cancel
	h.done = make(chan error, 1)
	go func() { <-ctx.Done(); h.done <- a.Shutdown(context.Background()) }()
	h.t.Cleanup(h.stop)
}

// stop shuts the app down (idempotent).
func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Errorf("shutdown timed out")
	}
}

// restart stops the app and boots a new one on the same database and runner.
func (h *harness) restart() {
	h.t.Helper()
	h.stop()
	h.boot("")
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func addr(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// echo opens a TCP connection to the entry port, sends msg and returns the reply.
func echo(t *testing.T, port int, msg string) (string, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr(port), time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, msg+"\n"); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSuffix(line, "\n"), err
}

// openConn opens a long-lived TCP connection and verifies it echoes once.
func openConn(t *testing.T, port int) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr(port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "ping\n")
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("long-lived conn echo: %q %v", line, err)
	}
	c.SetDeadline(time.Time{})
	return c
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// never asserts cond stays false for d.
func never(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("unexpectedly: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- web API helpers ---

func (h *harness) url(path string) string { return "http://" + h.app.WebAddr() + path }

func (h *harness) do(method, path, contentType, body string) (int, string) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.url(path), strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *harness) getJSON(path string, v any) {
	h.t.Helper()
	code, body := h.do("GET", path, "", "")
	if code != 200 {
		h.t.Fatalf("GET %s: %d %s", path, code, body)
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		h.t.Fatalf("GET %s: decode: %v (%s)", path, err, body)
	}
}

type statusRow struct {
	Name         string     `json:"name"`
	State        string     `json:"state"`
	Conns        int        `json:"conns"`
	RunningSince *time.Time `json:"running_since"`
	IdleDeadline *time.Time `json:"idle_deadline"`
	LastError    string     `json:"last_error"`
}

func (h *harness) state(project string) string {
	h.t.Helper()
	var rows []statusRow
	h.getJSON("/api/status", &rows)
	for _, r := range rows {
		if r.Name == project {
			return r.State
		}
	}
	return "<missing>"
}

type eventRow struct {
	Type       string `json:"type"`
	Project    string `json:"project"`
	Port       int    `json:"port"`
	Reason     string `json:"reason"`
	Error      string `json:"error"`
	Trigger    string `json:"trigger"`
	DurationMs int64  `json:"duration_ms"`
	BytesIn    int64  `json:"bytes_in"`
	BytesOut   int64  `json:"bytes_out"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
}

func (h *harness) events(query string) []eventRow {
	h.t.Helper()
	var rows []eventRow
	h.getJSON("/api/events?"+query, &rows)
	return rows
}

func (h *harness) hasEvent(query string, match func(eventRow) bool) bool {
	for _, e := range h.events(query) {
		if match == nil || match(e) {
			return true
		}
	}
	return false
}

// projectYAML renders a single-route project.
func projectYAML(name string, listen, upstream int, extra string) string {
	return fmt.Sprintf(`
  - name: %s
    dir: /tmp
%s    routes:
      - listen: %d
        bind: 127.0.0.1
        upstream: 127.0.0.1:%d
`, name, extra, listen, upstream)
}

func config(projects ...string) string { return "projects:" + strings.Join(projects, "") }
