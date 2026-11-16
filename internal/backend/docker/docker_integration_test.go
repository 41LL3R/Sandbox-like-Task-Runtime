//go:build integration

// Интеграционные тесты с настоящим Docker. Запуск:
//
//	go test -tags integration ./internal/backend/docker/
//
// Нужны доступ к демону и образы hello-world и alpine:3.20
// (скачаются сами, если есть интернет).
package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/backend"
)

func newBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(t.Context(), Options{MaxLogBytes: 1 << 20, StopGrace: 1})
	if err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// runToEnd проходит полный цикл: start → wait → logs → remove.
func runToEnd(t *testing.T, b *Backend, spec backend.Spec) (backend.ExitStatus, backend.Logs) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	id, err := b.Start(ctx, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if err := b.Remove(context.Background(), id); err != nil {
			t.Errorf("remove: %v", err)
		}
	}()
	st, err := b.Wait(ctx, id)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	logs, err := b.Logs(ctx, id)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	return st, logs
}

func TestHelloWorld(t *testing.T) {
	b := newBackend(t)
	st, logs := runToEnd(t, b, backend.Spec{RunID: "it-hello", Image: "hello-world"})
	if st.Code != 0 {
		t.Fatalf("exit code = %d", st.Code)
	}
	if !strings.Contains(string(logs.Stdout), "Hello from Docker!") {
		t.Fatalf("stdout = %q", logs.Stdout)
	}
}

func TestExitCodeAndSeparateStreams(t *testing.T) {
	b := newBackend(t)
	st, logs := runToEnd(t, b, backend.Spec{
		RunID: "it-exit",
		Image: "alpine:3.20",
		Cmd:   []string{"sh", "-c", "echo out; echo err >&2; exit 3"},
	})
	if st.Code != 3 {
		t.Fatalf("exit code = %d, want 3", st.Code)
	}
	if string(logs.Stdout) != "out\n" || string(logs.Stderr) != "err\n" {
		t.Fatalf("stdout = %q, stderr = %q", logs.Stdout, logs.Stderr)
	}
}

func TestStopAndRemove(t *testing.T) {
	b := newBackend(t)
	ctx := t.Context()
	id, err := b.Start(ctx, backend.Spec{RunID: "it-stop", Image: "alpine:3.20", Cmd: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	st, err := b.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Code == 0 {
		t.Errorf("stopped container exit code = 0")
	}
	if err := b.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	// Повторное удаление не должно быть ошибкой.
	if err := b.Remove(ctx, id); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

func TestMissingCommandIsStartError(t *testing.T) {
	b := newBackend(t)
	_, err := b.Start(t.Context(), backend.Spec{RunID: "it-nocmd", Image: "alpine:3.20", Cmd: []string{"no-such-binary"}})
	if err == nil {
		t.Fatal("expected start error for missing command")
	}
}
