package main

import (
    "context"
    "fmt"
    "log"

    "github.com/your-org/sandboxTaskRuntime/internal/runner"
)

func main() {
    r, err := runner.NewDockerRunner()
    if err != nil {
        log.Fatalf("runner: %v", err)
    }
    defer r.Close()

    code, logs, err := r.Run(
        context.Background(),
        "alpine:3.20",
        []string{"sh", "-c", "echo hello && sleep 2 && echo done"},
    )
    if err != nil {
        log.Fatalf("run: %v", err)
    }

    fmt.Printf("exit code: %d\n", code)
    fmt.Println("--- logs ---")
    fmt.Println(logs)
}
