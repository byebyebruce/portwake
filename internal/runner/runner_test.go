package runner_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"portwake/internal/config"
	"portwake/internal/runner"
)

type stub struct {
	name  string
	calls *[]string
}

func (s *stub) Up(ctx context.Context, p config.Project) error {
	*s.calls = append(*s.calls, s.name+".Up")
	return nil
}

func (s *stub) Stop(ctx context.Context, p config.Project) error {
	*s.calls = append(*s.calls, s.name+".Stop")
	return nil
}

func (s *stub) Ps(ctx context.Context, p config.Project) ([]runner.ServiceStatus, error) {
	*s.calls = append(*s.calls, s.name+".Ps")
	return []runner.ServiceStatus{{Service: "x", State: "running"}}, nil
}

type logStub struct {
	stub
}

func (s *logStub) Logs(ctx context.Context, p config.Project, lines int) (string, error) {
	*s.calls = append(*s.calls, s.name+".Logs")
	return "hello", nil
}

func TestManagerDispatchesByType(t *testing.T) {
	var calls []string
	m := runner.Manager{
		Compose: &logStub{stub{name: "compose", calls: &calls}},
		Docker:  &stub{name: "docker", calls: &calls},
		Exec:    &logStub{stub{name: "exec", calls: &calls}},
	}
	for typ, want := range map[string]string{
		"":                 "compose",
		config.TypeCompose: "compose",
		config.TypeDocker:  "docker",
		config.TypeExec:    "exec",
	} {
		p := config.Project{Name: "p", Type: typ}
		if err := m.Up(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if err := m.Stop(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Ps(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		_, logErr := m.Logs(context.Background(), p, 10)
		if want == "docker" && logErr == nil {
			t.Fatalf("docker Logs should fail: %v", logErr)
		}
		if want != "docker" && logErr != nil {
			t.Fatalf("%s Logs: %v", want, logErr)
		}
		joined := strings.Join(calls, " ")
		if !strings.Contains(joined, want+".Up") || !strings.Contains(joined, want+".Stop") || !strings.Contains(joined, want+".Ps") {
			t.Fatalf("type %q did not dispatch to %q: %v", typ, want, calls)
		}
		calls = nil
	}
}

func TestManagerMissingRunner(t *testing.T) {
	m := runner.Manager{}
	p := config.Project{Name: "p", Type: config.TypeExec}
	if err := m.Up(context.Background(), p); !errors.Is(err, runner.ErrNoRunner) {
		t.Fatalf("Up error = %v, want ErrNoRunner", err)
	}
	if _, err := m.Ps(context.Background(), p); !errors.Is(err, runner.ErrNoRunner) {
		t.Fatalf("Ps error = %v, want ErrNoRunner", err)
	}
}

func TestManagerLogsUnsupported(t *testing.T) {
	m := runner.Manager{Docker: &stub{}}
	_, err := m.Logs(context.Background(), config.Project{Type: config.TypeDocker}, 10)
	if err == nil || !strings.Contains(err.Error(), "do not provide logs") {
		t.Fatalf("Logs error = %v", err)
	}
}

func TestManagerAliveDefaultsTrue(t *testing.T) {
	var calls []string
	m := runner.Manager{Exec: &stub{name: "exec", calls: &calls}}
	if !m.Alive(config.Project{Type: config.TypeExec}) {
		t.Fatal("Alive = false without an Alive implementation")
	}
}

func TestIsRunning(t *testing.T) {
	if runner.IsRunning(nil) {
		t.Fatal("empty status is running")
	}
	if !runner.IsRunning([]runner.ServiceStatus{{State: "exited"}, {State: "running"}}) {
		t.Fatal("running status not detected")
	}
}
