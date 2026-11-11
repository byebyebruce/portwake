package docker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"portwake/internal/config"
)

func TestContainerName(t *testing.T) {
	if got := ContainerName(config.Project{Name: "blog"}); got != "portwake-blog" {
		t.Fatalf("ContainerName = %q", got)
	}
	if got := ContainerName(config.Project{Name: "blog", ContainerName: "custom"}); got != "custom" {
		t.Fatalf("ContainerName = %q", got)
	}
}

func TestRunArgs(t *testing.T) {
	p := config.Project{
		Name:          "blog",
		Type:          config.TypeDocker,
		Image:         "nginx:alpine",
		EnvFile:       filepath.Join(t.TempDir(), ".env"),
		Env:           map[string]string{"B": "2", "A": "1"},
		Ports:         []string{"127.0.0.1:18080:80"},
		Volumes:       []string{"/srv/data:/data"},
		Network:       "host",
		Command:       []string{"nginx", "-g", "daemon off;"},
		ContainerName: "custom-name",
	}
	got := CLI{}.runArgs(p)
	want := []string{
		"run", "-d", "--name", "custom-name", "--label", "portwake.project=blog",
		"--env-file", p.EnvFile,
		"-e", "A=1", "-e", "B=2",
		"-v", "/srv/data:/data",
		"-p", "127.0.0.1:18080:80",
		"--network", "host",
		"nginx:alpine", "nginx", "-g", "daemon off;",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runArgs:\n got %q\nwant %q", got, want)
	}
}

func TestParseState(t *testing.T) {
	samples := []struct {
		in     string
		status string
		health string
	}{
		{`{"Status":"running","Running":true,"Health":{"Status":"healthy"}}`, "running", "healthy"},
		{`{"Status":"exited","Running":false}`, "exited", ""},
	}
	for _, s := range samples {
		st, err := parseState([]byte(s.in))
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != s.status {
			t.Fatalf("Status = %q, want %q", st.Status, s.status)
		}
		h := ""
		if st.Health != nil {
			h = st.Health.Status
		}
		if h != s.health {
			t.Fatalf("Health = %q, want %q", h, s.health)
		}
	}
}

func TestStopSeconds(t *testing.T) {
	if got := stopSeconds(config.Project{}); got != 10 {
		t.Fatalf("default stop seconds = %d", got)
	}
	d := config.Duration(2500 * time.Millisecond)
	if got := stopSeconds(config.Project{StopTimeout: &d}); got != 3 {
		t.Fatalf("stop seconds = %d, want 3", got)
	}
}

func TestLogsCombinesStdoutAndStderr(t *testing.T) {
	script := filepath.Join(t.TempDir(), "docker")
	body := "#!/bin/sh\necho standard-out\necho standard-err >&2\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := (CLI{Docker: script}).Logs(context.Background(), config.Project{Name: "web"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "standard-out") || !strings.Contains(out, "standard-err") {
		t.Fatalf("Logs = %q, want stdout and stderr", out)
	}
}

func TestNotFound(t *testing.T) {
	if !notFound(errString("docker inspect: exit status 1: Error: No such object: x")) {
		t.Fatal("No such object not detected")
	}
	if notFound(errString("docker ps: permission denied")) {
		t.Fatal("unrelated error reported as not found")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
