// Package docker — бэкенд исполнения на локальном демоне Docker.
// С демоном общаемся через официальный Go SDK (github.com/moby/moby/client),
// который ходит в Unix-сокет /var/run/docker.sock (FR-7).
package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/your-org/sandboxTaskRuntime/internal/backend"
)

// Метки, которыми помечаются наши контейнеры. По ним при старте сервера
// находятся контейнеры, оставшиеся от прошлого (упавшего) запуска.
const (
	LabelManaged = "sandbox.managed"
	LabelRunID   = "sandbox.run-id"
)

type Options struct {
	// PullTimeout — сколько можно ждать скачивания образа.
	// Не входит в таймаут самой задачи.
	PullTimeout time.Duration
	// StopGrace — сколько секунд ждать после SIGTERM перед SIGKILL.
	StopGrace int
	// MaxLogBytes — сколько байт stdout и stderr (каждого) сохранять.
	MaxLogBytes int
	Logger      *slog.Logger
}

type Backend struct {
	cli  *client.Client
	opts Options
	log  *slog.Logger
}

var _ backend.Backend = (*Backend)(nil)

// New подключается к демону Docker. Адрес берётся из DOCKER_HOST, по
// умолчанию unix:///var/run/docker.sock. Версия API согласуется
// автоматически.
func New(ctx context.Context, opts Options) (*Backend, error) {
	if opts.PullTimeout <= 0 {
		opts.PullTimeout = 10 * time.Minute
	}
	if opts.StopGrace < 0 {
		opts.StopGrace = 0
	}
	if opts.MaxLogBytes <= 0 {
		opts.MaxLogBytes = 1 << 20
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(pingCtx, client.PingOptions{}); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("docker daemon at %s is not reachable "+
			"(is it running, and do you have access to the socket?): %w",
			cli.DaemonHost(), err)
	}
	return &Backend{cli: cli, opts: opts, log: opts.Logger}, nil
}

func (b *Backend) Close() error {
	return b.cli.Close()
}

func (b *Backend) Start(ctx context.Context, spec backend.Spec) (string, error) {
	if err := b.ensureImage(ctx, spec.Image); err != nil {
		return "", err
	}

	created, err := b.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "sandbox-" + spec.RunID,
		Config: &container.Config{
			Image: spec.Image,
			Cmd:   spec.Cmd, // nil -> CMD из образа
			// Без TTY: тогда stdout и stderr приходят раздельно.
			Tty: false,
			Labels: map[string]string{
				LabelManaged: "true",
				LabelRunID:   spec.RunID,
			},
		},
		// Без AutoRemove: иначе контейнер может исчезнуть раньше,
		// чем мы заберём логи и код возврата. Удаляем сами в Remove.
		HostConfig: &container.HostConfig{},
	})
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	for _, w := range created.Warnings {
		b.log.Warn("docker warning", "run_id", spec.RunID, "warning", w)
	}

	if _, err := b.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		// Например, в образе нет указанной команды. Контейнер уже создан —
		// убираем его, чтобы не копились мёртвые контейнеры.
		_ = b.Remove(context.WithoutCancel(ctx), created.ID)
		return "", fmt.Errorf("start container: %w", err)
	}
	return created.ID, nil
}

// ensureImage скачивает образ, только если его нет локально.
func (b *Backend) ensureImage(ctx context.Context, image string) error {
	_, err := b.cli.ImageInspect(ctx, image)
	if err == nil {
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect image %q: %w", image, err)
	}

	b.log.Info("pulling image", "image", image)
	pullCtx, cancel := context.WithTimeout(ctx, b.opts.PullTimeout)
	defer cancel()

	resp, err := b.cli.ImagePull(pullCtx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %q: %w", image, err)
	}
	// Скачивание идёт, пока читается поток ответа. Wait дочитывает его
	// до конца и возвращает ошибку, если она пришла внутри потока.
	if err := resp.Wait(pullCtx); err != nil {
		return fmt.Errorf("pull image %q: %w", image, err)
	}
	b.log.Info("image pulled", "image", image)
	return nil
}

func (b *Backend) Wait(ctx context.Context, id string) (backend.ExitStatus, error) {
	// Условие "not-running" срабатывает и для уже завершившегося контейнера,
	// поэтому гонки «контейнер закончился раньше, чем мы начали ждать» нет.
	wait := b.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})

	var code int
	select {
	case res := <-wait.Result:
		if res.Error != nil && res.Error.Message != "" {
			return backend.ExitStatus{}, fmt.Errorf("wait container: %s", res.Error.Message)
		}
		code = int(res.StatusCode)
	case err := <-wait.Error:
		return backend.ExitStatus{}, fmt.Errorf("wait container: %w", err)
	}

	status := backend.ExitStatus{Code: code}
	// Inspect нужен, чтобы узнать, не убит ли контейнер из-за памяти.
	if ins, err := b.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{}); err == nil &&
		ins.Container.State != nil {
		status.OOMKilled = ins.Container.State.OOMKilled
	}
	return status, nil
}

func (b *Backend) Logs(ctx context.Context, id string) (backend.Logs, error) {
	rc, err := b.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return backend.Logs{}, fmt.Errorf("container logs: %w", err)
	}
	defer rc.Close()

	stdout := &cappedBuffer{limit: b.opts.MaxLogBytes}
	stderr := &cappedBuffer{limit: b.opts.MaxLogBytes}
	// Без TTY Docker склеивает stdout и stderr в один поток с 8-байтовыми
	// заголовками. StdCopy разбирает его обратно на два потока.
	if _, err := stdcopy.StdCopy(stdout, stderr, rc); err != nil {
		return backend.Logs{}, fmt.Errorf("read container logs: %w", err)
	}
	return backend.Logs{
		Stdout:    stdout.buf,
		Stderr:    stderr.buf,
		Truncated: stdout.truncated || stderr.truncated,
	}, nil
}

func (b *Backend) Stop(ctx context.Context, id string) error {
	grace := b.opts.StopGrace
	_, err := b.cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &grace})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("stop container: %w", err)
	}
	return nil
}

func (b *Backend) Remove(ctx context.Context, id string) error {
	_, err := b.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove container: %w", err)
	}
	return nil
}

// CleanupOrphans удаляет наши контейнеры, оставшиеся после прошлого запуска
// сервера (например, если он упал). Состояние хранится в памяти, так что
// доделать такие запуски всё равно нельзя.
func (b *Backend) CleanupOrphans(ctx context.Context) (int, error) {
	list, err := b.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelManaged+"=true"),
	})
	if err != nil {
		return 0, fmt.Errorf("list containers: %w", err)
	}
	var errs []error
	removed := 0
	for _, c := range list.Items {
		if err := b.Remove(ctx, c.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// cappedBuffer запоминает не больше limit байт, но принимает всё,
// чтобы поток логов дочитывался до конца.
type cappedBuffer struct {
	buf       []byte
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - len(c.buf); room > 0 {
		if len(p) > room {
			c.buf = append(c.buf, p[:room]...)
			c.truncated = true
		} else {
			c.buf = append(c.buf, p...)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}
