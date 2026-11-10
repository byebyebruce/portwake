package app_test

import (
	"testing"
	"time"

	cfgpkg "portwake/internal/config"
)

func TestIdleProjectIsStoppedAfterIdleTimeout(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "    idle_timeout: 150ms\n")))

	if _, err := echo(t, entry, "x"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "idle stop", func() bool { return h.fake.StopCalls("web") == 1 && h.state("web") == "stopped" })
	eventually(t, "stop_done idle event", func() bool {
		return h.hasEvent("type=stop_done", func(e eventRow) bool { return e.Reason == "idle" && e.DurationMs > 0 })
	})
}

func TestOpenConnectionKeepsProjectRunning(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "    idle_timeout: 100ms\n")))

	c := openConn(t, entry)
	never(t, "stopped with an open connection", 300*time.Millisecond, func() bool { return h.fake.StopCalls("web") > 0 })
	c.Close()
	eventually(t, "idle stop after close", func() bool { return h.fake.StopCalls("web") == 1 })
}

func TestZeroIdleTimeoutNeverStops(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "    idle_timeout: 0\n")))

	if _, err := echo(t, entry, "x"); err != nil {
		t.Fatal(err)
	}
	never(t, "stopped", 300*time.Millisecond, func() bool { return h.fake.StopCalls("web") > 0 })
	var rows []statusRow
	h.getJSON("/api/status", &rows)
	if rows[0].IdleDeadline != nil {
		t.Fatalf("idle_deadline = %v, want none", rows[0].IdleDeadline)
	}
}

func TestStatusShowsIdleCountdownAndConnections(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "    idle_timeout: 1h\n")))

	c := openConn(t, entry)
	var rows []statusRow
	h.getJSON("/api/status", &rows)
	if rows[0].Conns != 1 || rows[0].IdleDeadline != nil || rows[0].RunningSince == nil {
		t.Fatalf("with open conn: %+v", rows[0])
	}
	c.Close()
	eventually(t, "idle countdown", func() bool {
		h.getJSON("/api/status", &rows)
		return rows[0].Conns == 0 && rows[0].IdleDeadline != nil && time.Until(*rows[0].IdleDeadline) > 50*time.Minute
	})
}

func TestConnectionDuringStopRestartsProject(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "    idle_timeout: 50ms\n")))
	if _, err := echo(t, entry, "x"); err != nil {
		t.Fatal(err)
	}
	h.fake.SetStopDelay("web", 200*time.Millisecond)
	eventually(t, "stopping", func() bool { return h.state("web") == "stopping" })

	got, err := echo(t, entry, "back")
	if err != nil || got != "back" {
		t.Fatalf("echo during stop = %q %v", got, err)
	}
	if h.fake.UpCalls("web") != 2 || h.fake.StopCalls("web") != 1 {
		t.Fatalf("up=%d stop=%d, want 2/1", h.fake.UpCalls("web"), h.fake.StopCalls("web"))
	}
}

func TestExternallyStoppedProjectIsDetectedAndRestartedOnDemand(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "")))
	if _, err := echo(t, entry, "x"); err != nil {
		t.Fatal(err)
	}
	h.fake.StopExternally("web")
	eventually(t, "detected stopped", func() bool { return h.state("web") == "stopped" })
	eventually(t, "stop_done external", func() bool {
		return h.hasEvent("type=stop_done", func(e eventRow) bool { return e.Reason == "external" })
	})
	if got, err := echo(t, entry, "y"); err != nil || got != "y" {
		t.Fatalf("echo after external stop = %q %v", got, err)
	}
	if n := h.fake.UpCalls("web"); n != 2 {
		t.Fatalf("Up calls = %d", n)
	}
}

func TestExternallyStartedProjectIsTakenOverAndIdleStopped(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	y := config(projectYAML("web", entry, up, "    idle_timeout: 100ms\n"))
	h := start(t, y)
	ps, _ := cfgpkg.UnmarshalYAML([]byte(y))
	ps[0].Normalize()
	if err := h.fake.StartExternally(ps[0]); err != nil {
		t.Fatal(err)
	}
	eventually(t, "detected running", func() bool { return h.state("web") == "running" })
	eventually(t, "idle stopped", func() bool { return h.fake.StopCalls("web") == 1 && !h.fake.IsRunning("web") })
}

func TestUnrelatedConfigChangeDoesNotResetIdleCountdown(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, config(projectYAML("web", entry, up, "    idle_timeout: 1h\n")))
	if _, err := echo(t, entry, "x"); err != nil {
		t.Fatal(err)
	}
	deadline := func() time.Time {
		var rows []statusRow
		h.getJSON("/api/status", &rows)
		for _, r := range rows {
			if r.Name == "web" && r.IdleDeadline != nil {
				return *r.IdleDeadline
			}
		}
		t.Fatal("no idle deadline")
		return time.Time{}
	}
	before := deadline()
	time.Sleep(20 * time.Millisecond)
	if code, body := h.do("POST", "/api/projects", "application/json", projectJSON("other", freePort(t), freePort(t))); code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	if after := deadline(); !after.Equal(before) {
		t.Fatalf("idle deadline moved from %v to %v", before, after)
	}
}
