package runner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type DockerRunner struct {
	dockerPath string
}

func NewDockerRunner() (*DockerRunner, error) {
	path, err := exec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("docker not found in PATH: %w", err)
	}
	return &DockerRunner{dockerPath: path}, nil
}

func (d *DockerRunner) Close() error {
	return nil
}

func (d *DockerRunner) Run(ctx context.Context, image string, cmd []string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	if out, err := exec.CommandContext(ctx, d.dockerPath, "pull", image).CombinedOutput(); err != nil {
		return -1, "", fmt.Errorf("pull: %w: %s", err, strings.TrimSpace(string(out)))
	}

	args := append([]string{"run", "--rm", image}, cmd...)
	out, err := exec.CommandContext(ctx, d.dockerPath, args...).CombinedOutput()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			if ctx.Err() != nil {
				return -1, string(out), fmt.Errorf("timeout: %w", ctx.Err())
			}
			return code, string(out), nil
		}
		return -1, string(out), fmt.Errorf("run: %w", err)
	}

	return 0, string(out), nil
}