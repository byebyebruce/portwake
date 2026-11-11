package app_test

import (
	"net/http"
	"testing"
)

func TestProjectLogsAPI(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))
	h.fake.SetLogs("web", "line one\nline two\n")

	code, body := h.do("GET", "/api/projects/web/logs?lines=10", "", "")
	if code != http.StatusOK || body != "line one\nline two\n" {
		t.Fatalf("logs: %d %q", code, body)
	}

	code, _ = h.do("GET", "/api/projects/missing/logs", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("missing project logs: %d, want 404", code)
	}
}
