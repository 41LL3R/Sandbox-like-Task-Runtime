package runner

import (
    "bytes"
    "context"
    "fmt"
    "io"
    "time"

    "github.com/moby/moby/api/pkg/stdcopy"
    "github.com/moby/moby/api/types/container"
    "github.com/moby/moby/client"
)

type DockerRunner struct {
    cli *client.Client
}

func NewDockerRunner() (*DockerRunner, error) {
    cli, err := client.New(client.FromEnv)
    if err != nil {
        return nil, fmt.Errorf("docker client: %w", err)
    }
    return &DockerRunner{cli: cli}, nil
}

func (d *DockerRunner) Close() error {
    return d.cli.Close()
}

func (d *DockerRunner) Run(ctx context.Context, image string, cmd []string) (int, string, error) {
    ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
    defer cancel()
    
    // 0. Pull image
    pullReader, err := d.cli.ImagePull(ctx, image, client.ImagePullOptions{})
    if err != nil {
        return -1, "", fmt.Errorf("pull: %w", err)
    }

    if _, err := io.Copy(io.Discard, pullReader); err != nil {
        pullReader.Close()
        return -1, "", fmt.Errorf("pull read: %w", err)
    }
    pullReader.Close()

    // 1. Create
    resp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
        Config: &container.Config{
            Image: image,
            Cmd:   cmd,
            Tty:   false,
        },
        Name: "",
    })
    if err != nil {
        return -1, "", fmt.Errorf("create: %w", err)
    }

    defer func() {
        _, _ = d.cli.ContainerRemove(context.Background(), resp.ID, client.ContainerRemoveOptions{
            Force:         true,
            RemoveVolumes: true,
        })
    }()

    // 2. Start
    if _, err := d.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
        return -1, "", fmt.Errorf("start: %w", err)
    }

    // 3. Wait
    wait := d.cli.ContainerWait(ctx, resp.ID, client.ContainerWaitOptions{
        Condition: container.WaitConditionNotRunning,
    })

    var exitCode int
    select {
    case err := <-wait.Error:
        if err != nil {
            return -1, "", fmt.Errorf("wait: %w", err)
        }
    case status := <-wait.Result:
        exitCode = int(status.StatusCode)
    case <-ctx.Done():
        _, _ = d.cli.ContainerStop(context.Background(), resp.ID, client.ContainerStopOptions{})
        return -1, "", fmt.Errorf("timeout: %w", ctx.Err())
    }

    // 4. Logs
    logsReader, err := d.cli.ContainerLogs(context.Background(), resp.ID, client.ContainerLogsOptions{
        ShowStdout: true,
        ShowStderr: true,
    })
    if err != nil {
        return exitCode, "", fmt.Errorf("logs: %w", err)
    }
    defer logsReader.Close()

    var buf bytes.Buffer
    if _, err := stdcopy.StdCopy(&buf, &buf, logsReader); err != nil && err != io.EOF {
        return exitCode, "", fmt.Errorf("read logs: %w", err)
    }

    return exitCode, buf.String(), nil
}
