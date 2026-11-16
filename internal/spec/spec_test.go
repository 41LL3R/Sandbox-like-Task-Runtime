package spec

import (
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	yaml := `
name: demo
image: alpine:3.20
timeout: 30
steps:
  - name: hello
    run: echo hello
  - name: world
    run: echo world
`
	task, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if task.Name != "demo" || task.Image != "alpine:3.20" || task.Timeout != 30 {
		t.Fatalf("unexpected task: %+v", task)
	}
	if len(task.Steps) != 2 || task.Steps[1].Run != "echo world" {
		t.Fatalf("unexpected steps: %+v", task.Steps)
	}
	if err := Validate(task); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"broken yaml":   "name: [",
		"unknown field": "name: x\nstepz:\n  - name: a\n    run: b",
		"multi-doc":     "name: a\nsteps:\n  - name: s\n    run: r\n---\nname: b",
		"empty doc":     "---\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yaml)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestValidateErrors(t *testing.T) {
	base := func() Task {
		return Task{Name: "x", Steps: []Step{{Name: "s", Run: "r"}}}
	}
	cases := map[string]func(*Task){
		"empty name":    func(t *Task) { t.Name = "" },
		"no steps":      func(t *Task) { t.Steps = nil },
		"empty steps":   func(t *Task) { t.Steps = []Step{} },
		"step no name":  func(t *Task) { t.Steps[0].Name = "" },
		"step no run":   func(t *Task) { t.Steps[0].Run = "" },
		"image whitesp": func(t *Task) { t.Image = "a b" },
		"neg timeout":   func(t *Task) { t.Timeout = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			task := base()
			mutate(&task)
			if err := Validate(task); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestBuildScript(t *testing.T) {
	steps := []Step{
		{Name: "one", Run: "echo 1"},
		{Name: "two", Run: "echo 2"},
	}
	s := BuildScript(steps)
	if !strings.HasPrefix(s, "#!/bin/sh\n") {
		t.Fatal("missing shebang")
	}
	if !strings.Contains(s, "set -e") {
		t.Fatal("missing set -e")
	}
	if !strings.Contains(s, "=== step 1/2: one") || !strings.Contains(s, "=== step 2/2: two") {
		t.Fatal("missing markers")
	}
}

func TestBuildScriptQuotesNames(t *testing.T) {
	s := BuildScript([]Step{{Name: "it's", Run: "true"}})
	if !strings.Contains(s, `'\''s`) {
		t.Fatalf("name was not escaped: %s", s)
	}
}
