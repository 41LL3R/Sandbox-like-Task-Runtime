package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/backend/fake"
	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/run"
	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/store"
	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/worker"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	be := fake.New()

	// Создаём и запускаем воркер для тестов
	pool := worker.New(worker.Config{
		Workers:        1,
		QueueSize:      10,
		DefaultTimeout: 5 * time.Second,
	}, st, be, nil)
	pool.Start(t.Context())

	// Передаём pool в NewServer (4-м аргументом)
	srv := NewServer(Config{
		DefaultImage:   "default:1",
		DefaultTimeout: 5 * time.Second,
		MaxTimeout:     time.Hour,
	}, st, be, pool, nil, t.Context())

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		// Контекст t.Context() отменится автоматически в конце теста,
		// что остановит воркеры.
	})
	return ts, st
}

func do(t *testing.T, method, url, body string) (int, map[string]any, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

func waitStatus(t *testing.T, base, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, m, _ := do(t, "GET", base+"/runs/"+id, "")
		if s := run.Status(m["status"].(string)); s.Final() {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish", id)
	return nil
}

func TestCreateRunAndGetResult(t *testing.T) {
	ts, _ := newTestServer(t)

	code, m, raw := do(t, "POST", ts.URL+"/runs", `{"image":"hello-world"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /runs = %d %s", code, raw)
	}
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("no id in response: %s", raw)
	}

	m = waitStatus(t, ts.URL, id)
	if m["status"] != "succeeded" || m["exit_code"] != float64(0) {
		t.Fatalf("unexpected run: %v", m)
	}

	code, m, raw = do(t, "GET", ts.URL+"/runs/"+id+"/logs", "")
	if code != http.StatusOK || m["stdout"] != "hello\n" {
		t.Fatalf("GET logs = %d %s", code, raw)
	}

	_, _, text := do(t, "GET", ts.URL+"/runs/"+id+"/logs?stream=stdout", "")
	if text != "hello\n" {
		t.Fatalf("plain stdout = %q", text)
	}
}

func TestFailedRun(t *testing.T) {
	ts, _ := newTestServer(t)
	_, m, _ := do(t, "POST", ts.URL+"/runs", `{"image":"alpine","cmd":["exit","3"]}`)

	m = waitStatus(t, ts.URL, m["id"].(string))
	if m["status"] != "failed" || m["exit_code"] != float64(3) {
		t.Fatalf("unexpected run: %v", m)
	}
}

func TestDefaultImage(t *testing.T) {
	ts, _ := newTestServer(t)
	_, m, _ := do(t, "POST", ts.URL+"/runs", `{}`)
	spec := m["spec"].(map[string]any)
	if spec["image"] != "default:1" {
		t.Fatalf("image = %v, want default:1", spec["image"])
	}
}

func TestBadRequests(t *testing.T) {
	ts, _ := newTestServer(t)
	cases := map[string]string{
		"broken json":      `{"image":`,
		"unknown field":    `{"image":"a","comand":["x"]}`,
		"negative timeout": `{"image":"a","timeout_seconds":-1}`,
		"huge timeout":     `{"image":"a","timeout_seconds":999999}`,
		"two objects":      `{"image":"a"}{"image":"b"}`,
		"image with space": `{"image":"a b"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			code, m, raw := do(t, "POST", ts.URL+"/runs", body)
			if code != http.StatusBadRequest {
				t.Fatalf("code = %d, body %s", code, raw)
			}
			if msg, _ := m["error"].(string); msg == "" {
				t.Fatalf("no error message: %s", raw)
			}
		})
	}
}

func TestNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, path := range []string{"/runs/nope", "/runs/nope/logs"} {
		if code, _, _ := do(t, "GET", ts.URL+path, ""); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}

func TestListRuns(t *testing.T) {
	ts, _ := newTestServer(t)
	do(t, "POST", ts.URL+"/runs", `{"image":"a"}`)
	do(t, "POST", ts.URL+"/runs", `{"image":"b"}`)

	_, m, _ := do(t, "GET", ts.URL+"/runs", "")
	runs := m["runs"].([]any)
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	if first := runs[0].(map[string]any)["spec"].(map[string]any)["image"]; first != "a" {
		t.Errorf("runs are not in creation order: first image = %v", first)
	}
}
