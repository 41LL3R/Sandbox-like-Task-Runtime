// Package run описывает запуск задачи: что запускаем и в каком он состоянии.
package run

import "time"

// Status — состояние запуска (FR-2).
type Status string

const (
	StatusQueued    Status = "queued"    // в очереди
	StatusRunning   Status = "running"   // выполняется
	StatusSucceeded Status = "succeeded" // завершён успешно
	StatusFailed    Status = "failed"    // завершён с ошибкой
)

// Final сообщает, что запуск больше не изменится.
func (s Status) Final() bool {
	return s == StatusSucceeded || s == StatusFailed
}

// Spec — что нужно запустить. На этапе 2 сюда добавится разобранный YAML.
type Spec struct {
	Image          string   `json:"image"`
	Cmd            []string `json:"cmd,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// Run — один запуск задачи со статусом, кодом возврата и логами.
type Run struct {
	ID     string `json:"id"`
	Spec   Spec   `json:"spec"`
	Status Status `json:"status"`

	// ExitCode равен nil, пока контейнер не завершился
	// (или если он так и не запустился).
	ExitCode *int `json:"exit_code"`
	// Error — понятная причина неуспеха: ошибка Docker, таймаут,
	// ненулевой код возврата.
	Error string `json:"error,omitempty"`

	ContainerID string `json:"container_id,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// Логи отдаются отдельной ручкой, поэтому в JSON запуска их нет.
	Stdout        []byte `json:"-"`
	Stderr        []byte `json:"-"`
	LogsTruncated bool   `json:"logs_truncated,omitempty"`
}
