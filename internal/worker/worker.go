// Package worker исполняет запуски в фоне: берёт их из очереди, запускает
// контейнер через бэкенд, ждёт завершения, забирает логи и записывает
// итог в хранилище. Сервер при этом сразу отвечает клиенту и не ждёт
// окончания задачи.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/backend"
	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/run"
	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/store"
)

var ErrQueueFull = errors.New("run queue is full")

type Config struct {
	// Workers — сколько задач выполняется одновременно.
	Workers int
	// QueueSize — сколько задач может ждать в очереди.
	QueueSize int
	// DefaultTimeout — таймаут задачи, если он не указан в запросе.
	DefaultTimeout time.Duration
	// CleanupTimeout — сколько ждать остановки и удаления контейнера.
	CleanupTimeout time.Duration
}

type Pool struct {
	cfg     Config
	store   store.Store
	backend backend.Backend
	log     *slog.Logger
	queue   chan string
	wg      sync.WaitGroup
}

func New(cfg Config, st store.Store, be backend.Backend, log *slog.Logger) *Pool {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 100
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 10 * time.Minute
	}
	if cfg.CleanupTimeout <= 0 {
		cfg.CleanupTimeout = 30 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Pool{
		cfg:     cfg,
		store:   st,
		backend: be,
		log:     log,
		queue:   make(chan string, cfg.QueueSize),
	}
}

// Start запускает воркеры. Когда ctx отменяется (остановка сервера),
// выполняющиеся контейнеры останавливаются и удаляются.
func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.cfg.Workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-p.queue:
					p.execute(ctx, id)
				}
			}
		}()
	}
}

// Wait ждёт, пока все воркеры завершатся после отмены ctx.
func (p *Pool) Wait() {
	p.wg.Wait()
}

// Submit ставит запуск в очередь. Не блокируется: если очередь
// переполнена, сразу возвращает ErrQueueFull.
func (p *Pool) Submit(id string) error {
	select {
	case p.queue <- id:
		return nil
	default:
		return ErrQueueFull
	}
}

func (p *Pool) execute(ctx context.Context, id string) {
	log := p.log.With("run_id", id)

	// Что бы ни случилось (даже паника), запуск должен прийти
	// в конечный статус — требование надёжности из ТЗ.
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("panic while executing run", "panic", rec)
			p.finish(id, nil, fmt.Sprintf("internal error: %v", rec), backend.Logs{})
		}
	}()

	r, err := p.store.Update(id, func(r *run.Run) { r.Status = run.StatusRunning })
	if err != nil {
		log.Error("run disappeared from store", "err", err)
		return
	}

	timeout := p.cfg.DefaultTimeout
	if r.Spec.TimeoutSeconds > 0 {
		timeout = time.Duration(r.Spec.TimeoutSeconds) * time.Second
	}

	log.Info("starting container", "image", r.Spec.Image, "cmd", r.Spec.Cmd)
	containerID, err := p.backend.Start(ctx, backend.Spec{
		RunID: id,
		Image: r.Spec.Image,
		Cmd:   r.Spec.Cmd,
	})
	if err != nil {
		log.Warn("container did not start", "err", err)
		p.finish(id, nil, err.Error(), backend.Logs{})
		return
	}
	startedAt := time.Now()
	p.store.Update(id, func(r *run.Run) {
		r.ContainerID = containerID
		r.StartedAt = &startedAt
	})
	log = log.With("container_id", shortID(containerID))

	// Для уборки берём контекст без отмены: даже при остановке сервера
	// контейнер нужно остановить и удалить (FR-10).
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.CleanupTimeout)
	defer cancelCleanup()
	defer func() {
		if err := p.backend.Remove(cleanupCtx, containerID); err != nil {
			log.Error("failed to remove container", "err", err)
		}
	}()

	waitCtx, cancelWait := context.WithTimeout(ctx, timeout)
	status, waitErr := p.backend.Wait(waitCtx, containerID)
	cancelWait()

	var reason string
	if waitErr != nil {
		switch {
		case ctx.Err() != nil:
			reason = "server is shutting down"
		case errors.Is(waitCtx.Err(), context.DeadlineExceeded):
			reason = fmt.Sprintf("timeout: task did not finish in %s", timeout)
		default:
			reason = waitErr.Error()
		}
		log.Warn("stopping container", "reason", reason)
		if err := p.backend.Stop(cleanupCtx, containerID); err != nil {
			log.Error("failed to stop container", "err", err)
		}
		// После остановки узнаём, с каким кодом контейнер всё-таки завершился.
		status, waitErr = p.backend.Wait(cleanupCtx, containerID)
	}

	logs, err := p.backend.Logs(cleanupCtx, containerID)
	if err != nil {
		log.Error("failed to read logs", "err", err)
	}

	var exitCode *int
	if waitErr == nil {
		code := status.Code
		exitCode = &code
		if reason == "" {
			switch {
			case status.OOMKilled:
				reason = "container was killed: out of memory"
			case code != 0:
				reason = fmt.Sprintf("process exited with code %d", code)
			}
		}
	} else if reason == "" {
		reason = waitErr.Error()
	}

	p.finish(id, exitCode, reason, logs)
	log.Info("run finished", "exit_code", status.Code, "error", reason)
}

// finish переводит запуск в конечный статус. Пустой reason вместе с
// кодом 0 означает успех.
func (p *Pool) finish(id string, exitCode *int, reason string, logs backend.Logs) {
	now := time.Now()
	p.store.Update(id, func(r *run.Run) {
		r.ExitCode = exitCode
		r.Error = reason
		r.FinishedAt = &now
		r.Stdout = logs.Stdout
		r.Stderr = logs.Stderr
		r.LogsTruncated = logs.Truncated
		if reason == "" && exitCode != nil && *exitCode == 0 {
			r.Status = run.StatusSucceeded
		} else {
			r.Status = run.StatusFailed
		}
	})
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
