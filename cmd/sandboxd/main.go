// sandboxd — сервер Task Sandbox: принимает задачи
// по HTTP и исполняет их в Docker-контейнерах.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/your-org/sandboxTaskRuntime/internal/api"
	"github.com/your-org/sandboxTaskRuntime/internal/backend/docker"
	"github.com/your-org/sandboxTaskRuntime/internal/store"
)

func main() {
	var (
		addr           = flag.String("addr", ":8080", "адрес HTTP-сервера")
		defaultImage   = flag.String("default-image", "alpine:3.20", "образ, если в задаче он не указан")
		defaultTimeout = flag.Duration("default-timeout", 10*time.Minute, "таймаут задачи по умолчанию")
		maxTimeout     = flag.Duration("max-timeout", time.Hour, "наибольший таймаут, который можно запросить")
		pullTimeout    = flag.Duration("pull-timeout", 10*time.Minute, "таймаут скачивания образа")
		maxLogBytes    = flag.Int("max-log-bytes", 1<<20, "сколько байт stdout и stderr хранить на запуск")
		debug          = flag.Bool("debug", false, "подробные логи (в том числе каждый HTTP-запрос)")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// ctx отменится по Ctrl+C или SIGTERM.
        // Он же корневой контекст для фоновых задач:
        // при остановке сервера запущенные контейнеры
        // будут остановлены и удалены.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	be, err := docker.New(ctx, docker.Options{
		PullTimeout: *pullTimeout,
		StopGrace:   5,
		MaxLogBytes: *maxLogBytes,
		Logger:      log,
	})
	if err != nil {
		log.Error("cannot connect to docker", "err", err)
		os.Exit(1)
	}
	defer be.Close()

	if n, err := be.CleanupOrphans(ctx); err != nil {
		log.Warn("cleanup of old containers failed", "err", err)
	} else if n > 0 {
		log.Info("removed containers left from previous run", "count", n)
	}

	st := store.NewMemory()
	apiSrv := api.NewServer(api.Config{
		DefaultImage:   *defaultImage,
		DefaultTimeout: *defaultTimeout,
		MaxTimeout:     *maxTimeout,
	}, st, be, log, ctx)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("server started", "addr", *addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			stop()
			apiSrv.Wait()
			os.Exit(1)
		}
	case <-ctx.Done():
	}

	log.Info("shutting down: stopping running containers")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	apiSrv.Wait()
	log.Info("bye")
}
