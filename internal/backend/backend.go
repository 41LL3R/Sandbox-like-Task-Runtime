// Package backend задаёт интерфейс бэкенда исполнения — места, где реально
// запускаются контейнеры. Сейчас есть одна реализация (локальный Docker),
// позже появятся Docker Swarm и Kubernetes. Сервер и воркер работают только
// с этим интерфейсом, поэтому новые бэкенды не потребуют их переписывать.
package backend

import "context"

// Spec — параметры контейнера для одного запуска.
type Spec struct {
	RunID string
	Image string
	// Cmd — команда; если пустая, используется CMD из образа.
	Cmd []string
}

// ExitStatus — как завершился контейнер.
type ExitStatus struct {
	Code      int
	OOMKilled bool // убит из-за превышения лимита памяти (пригодится на этапе 4)
}

// Logs — вывод контейнера, stdout и stderr раздельно.
type Logs struct {
	Stdout    []byte
	Stderr    []byte
	Truncated bool // вывод был длиннее лимита и обрезан
}

type Backend interface {
	// Start готовит образ, создаёт и запускает контейнер, возвращает его ID.
	Start(ctx context.Context, spec Spec) (string, error)
	// Wait блокируется до завершения контейнера.
	Wait(ctx context.Context, id string) (ExitStatus, error)
	// Logs возвращает вывод контейнера на текущий момент.
	Logs(ctx context.Context, id string) (Logs, error)
	// Stop останавливает контейнер (SIGTERM, затем SIGKILL).
	Stop(ctx context.Context, id string) error
	// Remove удаляет контейнер. Отсутствующий контейнер — не ошибка.
	Remove(ctx context.Context, id string) error
}
