package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/backend/fake"
	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/run"
	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/store"
)

type env struct {
	st   *store.Memory
	be   *fake.Backend
	pool *Pool
}

func setup(t *testing.T, ctx context.Context) env {
	t.Helper()
	e := env{st: store.NewMemory(), be: fake.New()}
	e.pool = New(Config{Workers: 2, QueueSize: 10, DefaultTimeout: 5 * time.Second}, e.st, e.be, nil)
	e.pool.Start(ctx)
	return e
}

func (e env) submit(t *testing.T, id string, spec run.Spec) {
	t.Helper()
	if err := e.st.Create(run.Run{ID: id, Spec: spec, Status: run.StatusQueued, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.Submit(id); err != nil {
		t.Fatal(err)
	}
}

func waitFinal(t *testing.T, st store.Store, id string) run.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := st.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status.Final() {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish in time", id)
	return run.Run{}
}

func TestSuccess(t *testing.T) {
	e := setup(t, t.Context())
	e.submit(t, "ok", run.Spec{Image: "img"})

	r := waitFinal(t, e.st, "ok")
	if r.Status != run.StatusSucceeded || r.ExitCode == nil || *r.ExitCode != 0 || r.Error != "" {
		t.Fatalf("unexpected result: %+v", r)
	}
	if string(r.Stdout) != "hello\n" {
		t.Errorf("stdout = %q", r.Stdout)
	}
	if r.StartedAt == nil || r.FinishedAt == nil {
		t.Error("timestamps are not set")
	}
	if !e.be.Removed(r.ContainerID) {
		t.Error("container was not removed")
	}
}

func TestNonZeroExit(t *testing.T) {
	e := setup(t, t.Context())
	e.submit(t, "bad", run.Spec{Image: "img", Cmd: []string{"exit", "3"}})

	r := waitFinal(t, e.st, "bad")
	if r.Status != run.StatusFailed || r.ExitCode == nil || *r.ExitCode != 3 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if !strings.Contains(r.Error, "code 3") {
		t.Errorf("error = %q", r.Error)
	}
	if string(r.Stdout) != "out\n" || string(r.Stderr) != "err\n" {
		t.Errorf("stdout = %q, stderr = %q", r.Stdout, r.Stderr)
	}
	if !e.be.Removed(r.ContainerID) {
		t.Error("container was not removed")
	}
}

func TestStartError(t *testing.T) {
	e := setup(t, t.Context())
	e.submit(t, "nostart", run.Spec{Image: "img", Cmd: []string{"fail-start"}})

	r := waitFinal(t, e.st, "nostart")
	if r.Status != run.StatusFailed || r.ExitCode != nil {
		t.Fatalf("unexpected result: %+v", r)
	}
	if !strings.Contains(r.Error, "executable file not found") {
		t.Errorf("error = %q", r.Error)
	}
}

func TestTimeoutStopsContainer(t *testing.T) {
	e := setup(t, t.Context())
	e.submit(t, "slow", run.Spec{Image: "img", Cmd: []string{"hang"}, TimeoutSeconds: 1})

	r := waitFinal(t, e.st, "slow")
	if r.Status != run.StatusFailed || !strings.Contains(r.Error, "timeout") {
		t.Fatalf("unexpected result: %+v", r)
	}
	if r.ExitCode == nil || *r.ExitCode != 137 {
		t.Errorf("exit code = %v, want 137", r.ExitCode)
	}
	if string(r.Stdout) != "started\n" {
		t.Errorf("logs of stopped container are lost: %q", r.Stdout)
	}
	if !e.be.Removed(r.ContainerID) {
		t.Error("container was not removed")
	}
}

func TestShutdownStopsRunningContainers(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	e := setup(t, ctx)
	e.submit(t, "long", run.Spec{Image: "img", Cmd: []string{"hang"}})

	// Ждём, пока контейнер реально запустится.
	for {
		r, _ := e.st.Get("long")
		if r.ContainerID != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	e.pool.Wait()

	r, _ := e.st.Get("long")
	if r.Status != run.StatusFailed || !strings.Contains(r.Error, "shutting down") {
		t.Fatalf("unexpected result: %+v", r)
	}
	if !e.be.Removed(r.ContainerID) {
		t.Error("container was not removed on shutdown")
	}
}

func TestQueueFull(t *testing.T) {
	p := New(Config{Workers: 1, QueueSize: 1}, store.NewMemory(), fake.New(), nil)
	// Воркеры не запущены, поэтому очередь не разбирается.
	if err := p.Submit("a"); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit("b"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
}
