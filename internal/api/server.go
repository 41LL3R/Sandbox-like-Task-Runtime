// Package api — HTTP API сервера.
//
//	POST /runs                создать запуск (FR-1)
//	GET  /runs                список запусков (FR-5)
//	GET  /runs/{id}           статус, код возврата, ошибка (FR-2, FR-4)
//	GET  /runs/{id}/logs      stdout и stderr (FR-3)
//	GET  /healthz             проверка, что сервер жив
//
// Исполнение запуска происходит в горутине прямо из createRun: пул,
// ограничений на параллелизм нет.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/your-org/sandboxTaskRuntime/internal/backend"
	"github.com/your-org/sandboxTaskRuntime/internal/run"
	"github.com/your-org/sandboxTaskRuntime/internal/store"
)

type Config struct {
	// DefaultImage — образ, если в запросе он не указан (FR-11).
	DefaultImage string
	// DefaultTimeout — таймаут задачи, если он не указан в запросе.
	DefaultTimeout time.Duration
	// MaxTimeout — наибольший таймаут, который можно запросить.
	MaxTimeout time.Duration
}

type Server struct {
	cfg     Config
	store   store.Store
	backend backend.Backend
	log     *slog.Logger
	newID   func() string

	// ctx — корневой контекст фоновых задач. Отменяется при остановке
	// сервера; исполнители это видят и останавливают контейнеры.
	ctx context.Context
	// wg ждёт завершения всех фоновых execute; нужен для graceful shutdown.
	wg sync.WaitGroup
}

func NewServer(cfg Config, st store.Store, be backend.Backend, log *slog.Logger, ctx context.Context) *Server {
	if log == nil {
		log = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 10 * time.Minute
	}
	return &Server{
		cfg:     cfg,
		store:   st,
		backend: be,
		log:     log,
		newID:   newRunID,
		ctx:     ctx,
	}
}

// Wait блокируется, пока не завершатся все фоновые execute.
// Вызывать после того, как ctx отменён, иначе он будет ждать вечно.
func (s *Server) Wait() {
	s.wg.Wait()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /runs", s.createRun)
	mux.HandleFunc("GET /runs", s.listRuns)
	mux.HandleFunc("GET /runs/{id}", s.getRun)
	mux.HandleFunc("GET /runs/{id}/logs", s.getLogs)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return s.logRequests(mux)
}

// createRunRequest — тело POST /runs. (Пока вариант бзе YAML).
type createRunRequest struct {
	Image          string   `json:"image"`
	Cmd            []string `json:"cmd"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req createRunRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid request body: unexpected data after JSON object")
		return
	}

	spec, err := s.validate(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rn := run.Run{
		ID:        s.newID(),
		Spec:      spec,
		Status:    run.StatusQueued,
		CreatedAt: time.Now(),
	}
	if err := s.store.Create(rn); err != nil {
		writeError(w, http.StatusInternalServerError, "save run: "+err.Error())
		return
	}

	s.wg.Add(1)
	go s.execute(rn.ID)

	s.log.Info("run created", "run_id", rn.ID, "image", spec.Image, "cmd", spec.Cmd)
	w.Header().Set("Location", "/runs/"+rn.ID)
	writeJSON(w, http.StatusCreated, rn)
}

func (s *Server) validate(req createRunRequest) (run.Spec, error) {
	image := strings.TrimSpace(req.Image)
	if image == "" {
		image = s.cfg.DefaultImage
	}
	if image == "" {
		return run.Spec{}, errors.New(`field "image" is required`)
	}
	if strings.ContainsAny(image, " \t\n") {
		return run.Spec{}, fmt.Errorf(`field "image": %q contains whitespace`, image)
	}
	if req.TimeoutSeconds < 0 {
		return run.Spec{}, errors.New(`field "timeout_seconds" must not be negative`)
	}
	if s.cfg.MaxTimeout > 0 && time.Duration(req.TimeoutSeconds)*time.Second > s.cfg.MaxTimeout {
		return run.Spec{}, fmt.Errorf(`field "timeout_seconds" must not exceed %d`,
			int(s.cfg.MaxTimeout/time.Second))
	}
	return run.Spec{Image: image, Cmd: req.Cmd, TimeoutSeconds: req.TimeoutSeconds}, nil
}

// execute: запускает контейнер, ждёт завершения,
// собирает логи, убирает за собой.
func (s *Server) execute(id string) {
	defer s.wg.Done()

	log := s.log.With("run_id", id)

	defer func() {
		if rec := recover(); rec != nil {
			log.Error("panic while executing run", "panic", rec)
			s.finish(id, nil, fmt.Sprintf("internal error: %v", rec), backend.Logs{})
		}
	}()

	r, err := s.store.Update(id, func(r *run.Run) { r.Status = run.StatusRunning })
	if err != nil {
		log.Error("run disappeared from store", "err", err)
		return
	}

	timeout := s.cfg.DefaultTimeout
	if r.Spec.TimeoutSeconds > 0 {
		timeout = time.Duration(r.Spec.TimeoutSeconds) * time.Second
	}

	log.Info("starting container", "image", r.Spec.Image, "cmd", r.Spec.Cmd)
	containerID, err := s.backend.Start(s.ctx, backend.Spec{
		RunID: id,
		Image: r.Spec.Image,
		Cmd:   r.Spec.Cmd,
	})
	if err != nil {
		log.Warn("container did not start", "err", err)
		s.finish(id, nil, err.Error(), backend.Logs{})
		return
	}
	startedAt := time.Now()
	s.store.Update(id, func(r *run.Run) {
		r.ContainerID = containerID
		r.StartedAt = &startedAt
	})
	log = log.With("container_id", shortID(containerID))

	// Для уборки требуется взять контекст без отмены
        // (даже при остановке сервера контейнер нужно
        // остановить и удалить (FR-10)).
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(s.ctx), 30*time.Second)
	defer cancelCleanup()
	defer func() {
		if err := s.backend.Remove(cleanupCtx, containerID); err != nil {
			log.Error("failed to remove container", "err", err)
		}
	}()

	waitCtx, cancelWait := context.WithTimeout(s.ctx, timeout)
	status, waitErr := s.backend.Wait(waitCtx, containerID)
	cancelWait()

	var reason string
	if waitErr != nil {
		switch {
		case s.ctx.Err() != nil:
			reason = "server is shutting down"
		case errors.Is(waitCtx.Err(), context.DeadlineExceeded):
			reason = fmt.Sprintf("timeout: task did not finish in %s", timeout)
		default:
			reason = waitErr.Error()
		}
		log.Warn("stopping container", "reason", reason)
		if err := s.backend.Stop(cleanupCtx, containerID); err != nil {
			log.Error("failed to stop container", "err", err)
		}
		// После остановки узнаём с каким кодом завершился контейнер.
		status, waitErr = s.backend.Wait(cleanupCtx, containerID)
	}

	logs, err := s.backend.Logs(cleanupCtx, containerID)
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

	s.finish(id, exitCode, reason, logs)
	log.Info("run finished", "exit_code", status.Code, "error", reason)
}

// finish переводит запуск в конечный статус
// (пустой reason вместе с кодом 0 означает успех).
func (s *Server) finish(id string, exitCode *int, reason string, logs backend.Logs) {
	now := time.Now()
	s.store.Update(id, func(r *run.Run) {
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

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	rn, ok := s.lookup(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, rn)
}

type logsResponse struct {
	ID        string     `json:"id"`
	Status    run.Status `json:"status"`
	Stdout    string     `json:"stdout"`
	Stderr    string     `json:"stderr"`
	Truncated bool       `json:"truncated,omitempty"`
}

// getLogs отдаёт логи запуска. Пока контейнер работает, логи читаются
// из бэкенда на лету; после завершения — из сохранённого результата.
//
// ?stream=stdout или ?stream=stderr отдаёт один поток простым текстом —
// удобно смотреть через curl.
func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	rn, ok := s.lookup(w, r)
	if !ok {
		return
	}

	logs := backend.Logs{Stdout: rn.Stdout, Stderr: rn.Stderr, Truncated: rn.LogsTruncated}
	if rn.Status == run.StatusRunning && rn.ContainerID != "" {
		if live, err := s.backend.Logs(r.Context(), rn.ContainerID); err == nil {
			logs = live
		} else if fresh, err := s.store.Get(rn.ID); err == nil {
			rn = fresh
			logs = backend.Logs{Stdout: rn.Stdout, Stderr: rn.Stderr, Truncated: rn.LogsTruncated}
		}
	}

	switch stream := r.URL.Query().Get("stream"); stream {
	case "":
		writeJSON(w, http.StatusOK, logsResponse{
			ID:        rn.ID,
			Status:    rn.Status,
			Stdout:    string(logs.Stdout),
			Stderr:    string(logs.Stderr),
			Truncated: logs.Truncated,
		})
	case "stdout", "stderr":
		data := logs.Stdout
		if stream == "stderr" {
			data = logs.Stderr
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	default:
		writeError(w, http.StatusBadRequest, `query parameter "stream" must be "stdout" or "stderr"`)
	}
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (run.Run, bool) {
	id := r.PathValue("id")
	rn, err := s.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("run %q not found", id))
		return run.Run{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return run.Run{}, false
	}
	return rn, true
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		s.log.Debug("http request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func newRunID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
