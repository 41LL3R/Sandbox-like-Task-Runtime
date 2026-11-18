// Package api — HTTP API сервера.
//
//	POST /runs                создать запуск
//	                          Content-Type: application/json - простой JSON
//	                          Content-Type: application/yaml - YAML-задача
//	GET  /runs                список запусков
//	GET  /runs/{id}           статус, код возврата, ошибка
//	GET  /runs/{id}/logs      stdout и stderr
//	GET  /healthz             проверка, что сервер жив
//
// Исполнение запуска происходит в горутине прямо из createRun:
// пул, ограничений на параллелизм нет.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/backend"
	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/run"
	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/spec"
	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/store"
	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/worker"
)

type Config struct {
	// DefaultImage - образ, если в запросе он не указан.
	DefaultImage string
	// DefaultTimeout - таймаут задачи, если он не указан в запросе.
	DefaultTimeout time.Duration
	// MaxTimeout - наибольший таймаут, который можно запросить.
	MaxTimeout time.Duration
}

type Server struct {
	cfg     Config
	store   store.Store
	backend backend.Backend
	worker  *worker.Pool
	log     *slog.Logger
	newID   func() string
	ctx context.Context
}

func NewServer(cfg Config, st store.Store, be backend.Backend, w *worker.Pool, log *slog.Logger, ctx context.Context) *Server {
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
		worker:  w,
		log:     log,
		newID:   newRunID,
		ctx:     ctx,
	}
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

// createRunRequest - тело POST /runs для JSON-формата (этап 1).
// YAML-формат описан в пакете spec (этап 2).
type createRunRequest struct {
	Image          string   `json:"image"`
	Cmd            []string `json:"cmd"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// createRun принимает описание задачи и ставит её в очередь.
// Формат выбирается по заголовку Content-Type: application/yaml
// (или любой *yaml*) — YAML-задача; иначе - простой JSON.
func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(spec.MaxYAMLBytes))

	var (
		rnSpec run.Spec
		task   *spec.Task
		err    error
	)
	if strings.Contains(r.Header.Get("Content-Type"), "yaml") {
		rnSpec, task, err = s.parseYAMLRequest(r)
	} else {
		rnSpec, err = s.parseJSONRequest(r)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rnSpec.Task = task

	rn := run.Run{
		ID:        s.newID(),
		Spec:      rnSpec,
		Status:    run.StatusQueued,
		CreatedAt: time.Now(),
	}
	if err := s.store.Create(rn); err != nil {
		writeError(w, http.StatusInternalServerError, "save run: "+err.Error())
		return
	}
	
	if err := s.worker.Submit(rn.ID); err != nil {
		// Если очередь переполнена, задача не встанет в очередь.
		// Нужно перевести её в статус failed, чтобы она не висела вечно в queued.
		s.store.Update(rn.ID, func(r *run.Run) {
			r.Status = run.StatusFailed
			r.Error = "run queue is full"
			now := time.Now()
			r.FinishedAt = &now
		})
		writeError(w, http.StatusServiceUnavailable, "run queue is full, try again later")
		return
	}

	s.log.Info("run created",
		"run_id", rn.ID, "image", rnSpec.Image, "yaml", task != nil)
	w.Header().Set("Location", "/runs/"+rn.ID)
	writeJSON(w, http.StatusCreated, rn)
}

// parseJSONRequest разбирает тело POST /runs в формате JSON (этап 1):
// {"image": "...", "cmd": [...], "timeout_seconds": N}.
func (s *Server) parseJSONRequest(r *http.Request) (run.Spec, error) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req createRunRequest
	if err := dec.Decode(&req); err != nil {
		return run.Spec{}, errors.New("invalid request body: " + err.Error())
	}
	if dec.More() {
		return run.Spec{}, errors.New("invalid request body: unexpected data after JSON object")
	}

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
	return run.Spec{
		Image:          image,
		Cmd:            req.Cmd,
		TimeoutSeconds: req.TimeoutSeconds,
	}, nil
}

// parseYAMLRequest разбирает YAML-задачу (этап 2), валидирует её
// и конвертирует в run.Spec: шаги превращаются в shell-скрипт,
// image и timeout заполняются дефолтами из конфига.
//
// Возвращает также исходный *spec.Task для отдачи клиенту
// (поле run.Spec.Task). Сам Task в исполнении не участвует -
// там уже готовый Cmd.
func (s *Server) parseYAMLRequest(r *http.Request) (run.Spec, *spec.Task, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return run.Spec{}, nil, fmt.Errorf("task is too large (max %d bytes)", maxErr.Limit)
		}
		return run.Spec{}, nil, fmt.Errorf("read body: %w", err)
	}

	task, err := spec.Parse(body)
	if err != nil {
		return run.Spec{}, nil, err
	}
	if err := spec.Validate(task); err != nil {
		return run.Spec{}, nil, err
	}

	image := task.Image
	if image == "" {
		image = s.cfg.DefaultImage
	}
	if image == "" {
		return run.Spec{}, nil,
			errors.New(`field "image" is required and no default image is configured`)
	}

	timeout := task.Timeout
	if timeout == 0 {
		timeout = int(s.cfg.DefaultTimeout / time.Second)
	}
	if s.cfg.MaxTimeout > 0 && time.Duration(timeout)*time.Second > s.cfg.MaxTimeout {
		return run.Spec{}, nil, fmt.Errorf(`field "timeout" must not exceed %d`,
			int(s.cfg.MaxTimeout/time.Second))
	}

	return run.Spec{
		Image:          image,
		Cmd:            []string{"sh", "-c", spec.BuildScript(task.Steps)},
		TimeoutSeconds: timeout,
	}, &task, nil
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
	Truncated bool       `json:"truncated"`
}

// getLogs отдаёт логи запуска. Пока контейнер работает, логи читаются
// из бэкенда на лету; после завершения - из сохранённого результата.
//
// ?stream=stdout или ?stream=stderr отдаёт один поток простым текстом -
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

