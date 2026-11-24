// Package docker — бэкенд исполнения на локальном демоне Docker.
// С демоном общаемся через официальный Go SDK (github.com/moby/moby/client),
// который ходит в Unix-сокет /var/run/docker.sock (FR-7).
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"path"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/41LL3R/Sandbox-like-Task-Runtime/internal/backend"
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

	labels := map[string]string{
		LabelManaged: "true",
		LabelRunID:   spec.RunID,
	}

	for key, value := range spec.Labels {
		labels[key] = value
	}

	env := make([]string, 0, len(spec.Env))
	env = append(env, spec.Env...)

	hostConfig := &container.HostConfig{}

	if spec.Resources.CPULimit > 0 {
		hostConfig.NanoCPUs = int64(spec.Resources.CPULimit * 1e9)
	}

	if spec.Resources.CPURequest > 0 {
		shares := int64(math.Max(
			2,
			math.Min(262144, spec.Resources.CPURequest*1024),
		))
		hostConfig.CPUShares = shares
	}

	if spec.Resources.MemoryLimit > 0 {
		hostConfig.Memory = spec.Resources.MemoryLimit
	}

	if spec.Resources.MemoryRequest > 0 {
		hostConfig.MemoryReservation = spec.Resources.MemoryRequest
	}

	created, err := b.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "sandbox-" + spec.RunID,
		Config: &container.Config{
			Image:  spec.Image,
			Cmd:    spec.Cmd,
			Env:    env,
			Tty:    false,
			Labels: labels,
		},
		HostConfig: hostConfig,
	})

	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	for _, w := range created.Warnings {
		b.log.Warn("docker warning", "run_id", spec.RunID, "warning", w)
	}

	for _, file := range spec.InputFiles {
		if err := b.putFile(ctx, created.ID, file); err != nil {
			_ = b.Remove(context.WithoutCancel(ctx), created.ID)
			return "", fmt.Errorf("put input file %q: %w", file.Path, err)
		}
	}

	// Затем запускаем контейнер.
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

func (b *Backend) putFile(
	ctx context.Context,
	containerID string,
	file backend.InputFile,
) error {
	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	header := &tar.Header{
		Name: path.Base(file.Path),
		Mode: 0644,
		Size: int64(len(file.Content)),
	}

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}

	if _, err := tw.Write(file.Content); err != nil {
		return fmt.Errorf("write tar content: %w", err)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}

	_, err := b.cli.CopyToContainer(
		ctx,
		containerID,
		client.CopyToContainerOptions{
			DestinationPath: path.Dir(file.Path),
			Content:         bytes.NewReader(buf.Bytes()),
		},
	)
	if err != nil {
		return fmt.Errorf("copy file to container: %w", err)
	}

	return nil
}

func (b *Backend) GetOutputs(
	ctx context.Context,
	containerID string,
	spec backend.OutputsSpec,
) (map[string]string, map[string][]byte, error) {
	values := make(map[string]string)
	files := make(map[string][]byte)

	for _, name := range spec.Values {
		outputPath := "/tmp/sandbox-outputs/" + name

		data, err := b.getFile(ctx, containerID, outputPath)
		if err != nil {
			// Отсутствующее значение считаем пустым.
			if cerrdefs.IsNotFound(err) {
				values[name] = ""
				continue
			}
			return nil, nil, fmt.Errorf("read output value %q: %w", name, err)
		}

		values[name] = string(data)
	}

	for _, filePath := range spec.Files {
		data, err := b.getFile(ctx, containerID, filePath)
		if err != nil {
			return nil, nil, fmt.Errorf("read output file %q: %w", filePath, err)
		}

		files[filePath] = data
	}

	return values, files, nil
}

func (b *Backend) getFile(
	ctx context.Context,
	containerID string,
	filePath string,
) ([]byte, error) {
	result, err := b.cli.CopyFromContainer(
		ctx,
		containerID,
		client.CopyFromContainerOptions{
			SourcePath: filePath,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("copy file from container: %w", err)
	}
	defer result.Content.Close()

	tr := tar.NewReader(result.Content)

	for {
		header, err := tr.Next()

		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar archive: %w", err)
		}

		if header.Typeflag != tar.TypeReg &&
			header.Typeflag != tar.TypeRegA {
			continue
		}

		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read output file: %w", err)
		}

		return content, nil
	}

	return nil, fmt.Errorf("file %q not found in archive", filePath)
}
