package app_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func projectJSON(name string, listen, upstream int) string {
	return fmt.Sprintf(`{"name":%q,"dir":"/tmp","routes":[{"listen":%d,"bind":"127.0.0.1","upstream":"127.0.0.1:%d"}]}`, name, listen, upstream)
}

type projectRow struct {
	Name   string `json:"name"`
	Routes []struct {
		Listen   int    `json:"listen"`
		Bind     string `json:"bind"`
		Mode     string `json:"mode"`
		Upstream string `json:"upstream"`
	} `json:"routes"`
}

func (h *harness) projects() []projectRow {
	h.t.Helper()
	var rows []projectRow
	h.getJSON("/api/projects", &rows)
	return rows
}

func TestCreatedProjectIsServedImmediatelyAndPersisted(t *testing.T) {
	h := start(t, "")
	entry, up := freePort(t), freePort(t)

	code, body := h.do("POST", "/api/projects", "application/json", projectJSON("api", entry, up))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	if got, err := echo(t, entry, "hi"); err != nil || got != "hi" {
		t.Fatalf("echo = %q %v", got, err)
	}
	ps := h.projects()
	if len(ps) != 1 || ps[0].Routes[0].Mode != "tcp" {
		t.Fatalf("projects = %+v", ps)
	}

	h.restart()
	if ps := h.projects(); len(ps) != 1 || ps[0].Name != "api" {
		t.Fatalf("after restart projects = %+v", ps)
	}
	if got, err := echo(t, entry, "again"); err != nil || got != "again" {
		t.Fatalf("echo after restart = %q %v", got, err)
	}
}

func TestInvalidProjectIsRejected(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))

	code, body := h.do("POST", "/api/projects", "application/json", projectJSON("other", entry, up))
	if code != http.StatusBadRequest || !strings.Contains(body, "already used") {
		t.Fatalf("duplicate port: %d %s", code, body)
	}
	code, _ = h.do("POST", "/api/projects", "application/json", projectJSON("web", freePort(t), up))
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate name: %d", code)
	}
	if n := len(h.projects()); n != 1 {
		t.Fatalf("projects = %d", n)
	}
}

func TestPortInUseRollsBackTheChange(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	code, body := h.do("PUT", "/api/projects/web", "application/json", projectJSON("web", busyPort, up))
	if code != http.StatusBadRequest || !strings.Contains(body, fmt.Sprint(busyPort)) {
		t.Fatalf("update to busy port: %d %s", code, body)
	}
	if ps := h.projects(); ps[0].Routes[0].Listen != entry {
		t.Fatalf("config changed despite failure: %+v", ps)
	}
	if got, err := echo(t, entry, "still"); err != nil || got != "still" {
		t.Fatalf("old port no longer served: %q %v", got, err)
	}
}

func TestChangingPortKeepsEstablishedConnections(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))
	c := openConn(t, entry)

	newEntry := freePort(t)
	if code, body := h.do("PUT", "/api/projects/web", "application/json", projectJSON("web", newEntry, up)); code != 200 {
		t.Fatalf("update: %d %s", code, body)
	}
	if got, err := echo(t, newEntry, "new"); err != nil || got != "new" {
		t.Fatalf("new port: %q %v", got, err)
	}
	if _, err := net.DialTimeout("tcp", addr(entry), 200*time.Millisecond); err == nil {
		t.Fatal("old port still accepting")
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(c, "old\n")
	if line, err := bufio.NewReader(c).ReadString('\n'); err != nil || line != "old\n" {
		t.Fatalf("established conn broken: %q %v", line, err)
	}
}

func TestChangedUpstreamIsUsedForNextStart(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))
	up2 := freePort(t)
	if code, body := h.do("PUT", "/api/projects/web", "application/json", projectJSON("web", entry, up2)); code != 200 {
		t.Fatalf("update: %d %s", code, body)
	}
	if got, err := echo(t, entry, "x"); err != nil || got != "x" {
		t.Fatalf("echo via new upstream: %q %v", got, err)
	}
	if c, err := net.DialTimeout("tcp", addr(up2), time.Second); err != nil {
		t.Fatalf("fake not started on new upstream: %v", err)
	} else {
		c.Close()
	}
}

func TestDeletedProjectStopsListeningButKeepsRunning(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "    idle_timeout: 100ms\n")))
	if _, err := echo(t, entry, "x"); err != nil {
		t.Fatal(err)
	}
	if code, body := h.do("DELETE", "/api/projects/web", "", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, body)
	}
	if _, err := net.DialTimeout("tcp", addr(entry), 200*time.Millisecond); err == nil {
		t.Fatal("deleted project still listening")
	}
	never(t, "deleted project stopped", 300*time.Millisecond, func() bool { return h.fake.StopCalls("web") > 0 })
	if code, _ := h.do("DELETE", "/api/projects/web", "", ""); code != http.StatusNotFound {
		t.Fatalf("second delete = %d", code)
	}
}

func TestManualStartAndStop(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))

	if code, body := h.do("POST", "/api/projects/web/start", "", ""); code != http.StatusAccepted {
		t.Fatalf("start: %d %s", code, body)
	}
	eventually(t, "running", func() bool { return h.state("web") == "running" })
	if code, body := h.do("POST", "/api/projects/web/stop", "", ""); code != http.StatusAccepted {
		t.Fatalf("stop: %d %s", code, body)
	}
	eventually(t, "stopped", func() bool { return h.state("web") == "stopped" && !h.fake.IsRunning("web") })
	eventually(t, "manual stop event", func() bool {
		return h.hasEvent("type=stop_done", func(e eventRow) bool { return e.Reason == "manual" })
	})
	if code, _ := h.do("POST", "/api/projects/nope/start", "", ""); code != http.StatusNotFound {
		t.Fatalf("unknown project start = %d", code)
	}
}

func TestYAMLExportAndImport(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "    idle_timeout: 5m\n")))

	code, exported := h.do("GET", "/api/config.yaml", "", "")
	if code != 200 || !strings.Contains(exported, "name: web") || !strings.Contains(exported, "idle_timeout: 5m0s") {
		t.Fatalf("export: %d\n%s", code, exported)
	}

	code, body := h.do("PUT", "/api/config.yaml", "application/yaml", "projects:\n  - name: bad\n")
	if code != http.StatusBadRequest {
		t.Fatalf("invalid import: %d %s", code, body)
	}
	if ps := h.projects(); len(ps) != 1 || ps[0].Name != "web" {
		t.Fatalf("config changed by invalid import: %+v", ps)
	}

	e2, u2 := freePort(t), freePort(t)
	code, body = h.do("PUT", "/api/config.yaml", "application/yaml", config(projectYAML("a", e2, u2, "")))
	if code != 200 {
		t.Fatalf("import: %d %s", code, body)
	}
	if ps := h.projects(); len(ps) != 1 || ps[0].Name != "a" {
		t.Fatalf("after import: %+v", ps)
	}
	if got, err := echo(t, e2, "imp"); err != nil || got != "imp" {
		t.Fatalf("imported project: %q %v", got, err)
	}
}

func TestStatsSummarizeStartsAndTraffic(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))
	h.fake.SetUpDelay("web", 50*time.Millisecond)
	for i := 0; i < 3; i++ {
		if _, err := echo(t, entry, "12345"); err != nil {
			t.Fatal(err)
		}
	}
	type stats struct {
		Project    string `json:"project"`
		Conns      int64  `json:"conns"`
		Starts     int64  `json:"starts"`
		AvgStartMs int64  `json:"avg_start_ms"`
		BytesIn    int64  `json:"bytes_in"`
		BytesOut   int64  `json:"bytes_out"`
		Series     []struct {
			Hour  int64 `json:"hour"`
			Conns int64 `json:"conns"`
		} `json:"series"`
		Ports []struct {
			Port    int   `json:"port"`
			Conns   int64 `json:"conns"`
			BytesIn int64 `json:"bytes_in"`
		} `json:"ports"`
	}
	eventually(t, "stats", func() bool {
		var rows []stats
		h.getJSON("/api/stats", &rows)
		return len(rows) == 1 && rows[0].Starts == 1 && rows[0].AvgStartMs >= 50 &&
			rows[0].Conns == 3 && rows[0].BytesIn == 18 && rows[0].BytesOut == 18 && len(rows[0].Series) == 1 &&
			len(rows[0].Ports) == 1 && rows[0].Ports[0].Port == entry && rows[0].Ports[0].Conns == 3 && rows[0].Ports[0].BytesIn == 18
	})
}

func TestEventStreamPushesStateChanges(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))

	resp, err := http.Get(h.url("/api/stream"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	go echo(t, entry, "x")

	sc := bufio.NewScanner(resp.Body)
	deadline := time.After(5 * time.Second)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for {
		select {
		case <-deadline:
			t.Fatal("no running state pushed")
		case l := <-lines:
			if !strings.HasPrefix(l, "data: ") {
				continue
			}
			var e struct {
				Type, Project, State string
			}
			json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &e)
			if e.Type == "state" && e.Project == "web" && e.State == "running" {
				return
			}
		}
	}
}

func TestShutdownLeavesProjectsRunning(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))
	if _, err := echo(t, entry, "x"); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if !h.fake.IsRunning("web") || h.fake.StopCalls("web") != 0 {
		t.Fatal("project was stopped by shutdown")
	}
	if s := h.state("web"); s != "running" {
		t.Fatalf("state after restart = %s", s)
	}
}

func TestUIIsServed(t *testing.T) {
	h := start(t, "")
	code, body := h.do("GET", "/", "", "")
	if code != 200 || !strings.Contains(body, "<title>portwake</title>") {
		t.Fatalf("index: %d", code)
	}
	if code, _ := h.do("GET", "/vendor/alpine.min.js", "", ""); code != 200 {
		t.Fatalf("vendor asset: %d", code)
	}
}
