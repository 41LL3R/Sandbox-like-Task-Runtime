// Package store хранит запуски. Пока только в памяти: после перезапуска
// сервера история теряется. Интерфейс Store позволит потом заменить
// хранение на файлы или БД, не трогая остальной код.
package store

import (
	"errors"
	"sync"

	"github.com/your-org/sandboxTaskRuntime/internal/run"
)

var (
	ErrNotFound = errors.New("run not found")
	ErrExists   = errors.New("run already exists")
)

type Store interface {
	Create(r run.Run) error
	Get(id string) (run.Run, error)
	// List возвращает запуски в порядке создания.
	List() ([]run.Run, error)
	// Update атомарно меняет запуск через fn и возвращает новую версию.
	Update(id string, fn func(r *run.Run)) (run.Run, error)
}

// Memory — потокобезопасное хранилище в памяти.
// Наружу всегда отдаются копии, чтобы никто не менял запуск в обход Update.
type Memory struct {
	mu    sync.RWMutex
	runs  map[string]*run.Run
	order []string
}

func NewMemory() *Memory {
	return &Memory{runs: make(map[string]*run.Run)}
}

func (m *Memory) Create(r run.Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.ID]; ok {
		return ErrExists
	}
	m.runs[r.ID] = &r
	m.order = append(m.order, r.ID)
	return nil
}

func (m *Memory) Get(id string) (run.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok {
		return run.Run{}, ErrNotFound
	}
	return *r, nil
}

func (m *Memory) List() ([]run.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]run.Run, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, *m.runs[id])
	}
	return out, nil
}

func (m *Memory) Update(id string, fn func(r *run.Run)) (run.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return run.Run{}, ErrNotFound
	}
	fn(r)
	return *r, nil
}
