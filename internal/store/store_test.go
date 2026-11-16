package store

import (
	"errors"
	"testing"

	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/run"
)

func TestMemory(t *testing.T) {
	m := NewMemory()
	if err := m.Create(run.Run{ID: "a", Status: run.StatusQueued}); err != nil {
		t.Fatal(err)
	}
	if err := m.Create(run.Run{ID: "a"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: err = %v", err)
	}
	m.Create(run.Run{ID: "b"})

	got, _ := m.Update("a", func(r *run.Run) { r.Status = run.StatusRunning })
	if got.Status != run.StatusRunning {
		t.Fatalf("status after update = %s", got.Status)
	}

	// Get отдаёт копию: её изменение не должно влиять на хранилище.
	r, _ := m.Get("a")
	r.Status = run.StatusFailed
	if again, _ := m.Get("a"); again.Status != run.StatusRunning {
		t.Fatal("store was modified through a returned copy")
	}

	list, _ := m.List()
	if len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" {
		t.Fatalf("list = %+v", list)
	}

	if _, err := m.Get("zzz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: err = %v", err)
	}
	if _, err := m.Update("zzz", func(*run.Run) {}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: err = %v", err)
	}
}
