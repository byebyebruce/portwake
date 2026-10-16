package app_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dcpentry/internal/compose/composetest"
	cfgpkg "dcpentry/internal/config"
)

func TestTCPConnectionStartsStoppedProjectAndForwards(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))

	got, err := echo(t, entry, "hello")
	if err != nil || got != "hello" {
		t.Fatalf("echo through entry = %q, %v; want hello", got, err)
	}
	if n := h.fake.UpCalls("web"); n != 1 {
		t.Fatalf("Up calls = %d, want 1", n)
	}
	if s := h.state("web"); s != "running" {
		t.Fatalf("state = %s, want running", s)
	}
}

func TestConcurrentConnectionsDuringStartTriggerASingleUp(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))
	h.fake.SetUpDelay("web", 150*time.Millisecond)

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := fmt.Sprintf("m%d", i)
			got, err := echo(t, entry, msg)
			if err == nil && got != msg {
				err = fmt.Errorf("got %q want %q", got, msg)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := h.fake.UpCalls("web"); n != 1 {
		t.Fatalf("Up calls = %d, want 1", n)
	}
}

func TestAlreadyRunningProjectIsForwardedWithoutUp(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := &harness{t: t, fake: composetest.New(), db: filepath.Join(t.TempDir(), "db")}
	cfgYAML := config(projectYAML("web", entry, up, ""))
	ps, _ := cfgpkg.UnmarshalYAML([]byte(cfgYAML))
	if err := h.fake.StartExternally(ps[0]); err != nil {
		t.Fatal(err)
	}
	h.boot(cfgYAML)

	if s := h.state("web"); s != "running" {
		t.Fatalf("state at boot = %s, want running", s)
	}
	if got, err := echo(t, entry, "x"); err != nil || got != "x" {
		t.Fatalf("echo = %q %v", got, err)
	}
	if n := h.fake.UpCalls("web"); n != 0 {
		t.Fatalf("Up calls = %d, want 0", n)
	}
}

func TestStartTimeoutClosesWaitingConnectionAndRecordsFailure(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "    start_timeout: 100ms\n")))
	h.fake.SetUpDelay("web", time.Hour)

	if _, err := echo(t, entry, "x"); err == nil {
		t.Fatal("expected the waiting connection to be closed")
	}
	if s := h.state("web"); s != "stopped" {
		t.Fatalf("state = %s, want stopped", s)
	}
	eventually(t, "start_fail event", func() bool {
		return h.hasEvent("project=web&type=start_fail", func(e eventRow) bool {
			return strings.Contains(e.Error, "timed out") && e.Trigger == fmt.Sprintf("port %d", entry)
		})
	})
}

func TestUpFailureIsReportedAndRetriedOnNextConnection(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))
	h.fake.SetUpError("web", errors.New("compose: no such image"))

	if _, err := echo(t, entry, "x"); err == nil {
		t.Fatal("expected connection to fail")
	}
	eventually(t, "start_fail with compose output", func() bool {
		return h.hasEvent("type=start_fail", func(e eventRow) bool { return strings.Contains(e.Error, "no such image") })
	})
	var rows []statusRow
	h.getJSON("/api/status", &rows)
	if !strings.Contains(rows[0].LastError, "no such image") {
		t.Fatalf("last_error = %q", rows[0].LastError)
	}

	h.fake.SetUpError("web", nil)
	if got, err := echo(t, entry, "again"); err != nil || got != "again" {
		t.Fatalf("retry echo = %q %v", got, err)
	}
	if n := h.fake.UpCalls("web"); n != 2 {
		t.Fatalf("Up calls = %d, want 2", n)
	}
}

func TestHealthServiceMustBeHealthyBeforeForwarding(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	y := fmt.Sprintf(`projects:
  - name: web
    dir: /tmp
    routes:
      - listen: %d
        bind: 127.0.0.1
        upstream: 127.0.0.1:%d
        health_service: db
`, entry, up)
	h := start(t, y)
	h.fake.SetHealth("web", "db", "starting")

	result := make(chan error, 1)
	go func() { _, err := echo(t, entry, "x"); result <- err }()

	eventually(t, "project starting", func() bool { return h.state("web") == "starting" })
	never(t, "forwarded while unhealthy", 150*time.Millisecond, func() bool { return len(result) > 0 })
	h.fake.SetHealth("web", "db", "healthy")
	if err := <-result; err != nil {
		t.Fatalf("echo after healthy: %v", err)
	}
	if s := h.state("web"); s != "running" {
		t.Fatalf("state = %s", s)
	}
}

func TestTCPReadinessIgnoresPortProxyThatClosesConnections(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))
	h.fake.SetWarmup("web", 300*time.Millisecond)

	if got, err := echo(t, entry, "first"); err != nil || got != "first" {
		t.Fatalf("first connection = %q %v", got, err)
	}
}
