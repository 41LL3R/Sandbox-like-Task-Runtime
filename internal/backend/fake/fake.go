// Package fake — бэкенд-заглушка для тестов: ведёт себя как Docker,
// но ничего не запускает. Поведение задаётся первым словом команды:
//
//	(пусто)          успешно, stdout "hello\n"
//	exit N           stdout "out\n", stderr "err\n", код N
//	hang             «висит», пока не вызовут Stop (код 137)
//	fail-start       Start возвращает ошибку
package fake

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/backend"
)

type container struct {
	done   chan struct{}
	status backend.ExitStatus
	logs   backend.Logs
}

type Backend struct {
	mu         sync.Mutex
	next       int
	containers map[string]*container
	removed    map[string]bool
}

var _ backend.Backend = (*Backend)(nil)

func New() *Backend {
	return &Backend{containers: map[string]*container{}, removed: map[string]bool{}}
}

func (b *Backend) Start(ctx context.Context, spec backend.Spec) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c := &container{done: make(chan struct{})}
	word := ""
	if len(spec.Cmd) > 0 {
		word = spec.Cmd[0]
	}
	switch word {
	case "fail-start":
		return "", errors.New("start container: executable file not found")
	case "":
		c.logs.Stdout = []byte("hello\n")
		close(c.done)
	case "exit":
		code, _ := strconv.Atoi(spec.Cmd[1])
		c.status.Code = code
		c.logs = backend.Logs{Stdout: []byte("out\n"), Stderr: []byte("err\n")}
		close(c.done)
	case "hang":
		c.logs.Stdout = []byte("started\n")
	default:
		return "", fmt.Errorf("fake: unknown command %q", word)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	id := fmt.Sprintf("fake-%d", b.next)
	b.containers[id] = c
	return id, nil
}

func (b *Backend) get(id string) (*container, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.containers[id]
	if !ok {
		return nil, fmt.Errorf("fake: no container %s", id)
	}
	return c, nil
}

func (b *Backend) Wait(ctx context.Context, id string) (backend.ExitStatus, error) {
	c, err := b.get(id)
	if err != nil {
		return backend.ExitStatus{}, err
	}
	select {
	case <-c.done:
		return c.status, nil
	case <-ctx.Done():
		return backend.ExitStatus{}, ctx.Err()
	}
}

func (b *Backend) Logs(ctx context.Context, id string) (backend.Logs, error) {
	c, err := b.get(id)
	if err != nil {
		return backend.Logs{}, err
	}
	return c.logs, nil
}

func (b *Backend) Stop(ctx context.Context, id string) error {
	c, err := b.get(id)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-c.done:
	default:
		c.status = backend.ExitStatus{Code: 137}
		close(c.done)
	}
	return nil
}

func (b *Backend) Remove(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removed[id] = true
	return nil
}

// Removed сообщает, был ли контейнер удалён.
func (b *Backend) Removed(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.removed[id]
}

func (b *Backend) GetOutputs(
	ctx context.Context,
	containerID string,
	outputs backend.OutputsSpec,
) (map[string]string, map[string][]byte, error) {
	return nil, nil, nil
}
