// Package spec описывает декларативный формат задачи (YAML)
package spec

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Лимиты разумности. Защищают от случайных гигантских YAML и
// от слишком больших аргументов, которые не влезут в argv Docker.
const (
	MaxYAMLBytes   = 1 << 20
	MaxSteps       = 100
	MaxNameLen     = 200
	MaxStepNameLen = 200
	MaxStepRunLen  = 8 * 1024
)

// Декларативное описание задачи из YAML.
type Task struct {
	Name      string     `yaml:"name"      json:"name"`
	Image     string     `yaml:"image,omitempty"     json:"image,omitempty"`
	Steps     []Step     `yaml:"steps"     json:"steps"`
	Resources *Resources `yaml:"resources,omitempty" json:"resources,omitempty"`
	Runner    *Runner    `yaml:"runner,omitempty"    json:"runner,omitempty"`
	Timeout   int        `yaml:"timeout,omitempty"   json:"timeout,omitempty"` // секунды
	Inputs    *Inputs    `yaml:"inputs,omitempty"    json:"inputs,omitempty"`
	Outputs   *Outputs   `yaml:"outputs,omitempty"   json:"outputs,omitempty"`
}

// Step - один шаг задачи: имя для логов и команда.
type Step struct {
	Name string `yaml:"name" json:"name"`
	Run  string `yaml:"run"  json:"run"`
}

type Resources struct {
	Requests *ResourceSpec `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits   *ResourceSpec `yaml:"limits,omitempty"   json:"limits,omitempty"`
}

// ResourceSpec - CPU и память строками:
// Docker принимает "2", "0.5", "4Gi", "512Mi".
type ResourceSpec struct {
	CPU    string `yaml:"cpu,omitempty"    json:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
}

type Runner struct {
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

type Inputs struct {
	Env   map[string]string `yaml:"env,omitempty"   json:"env,omitempty"`
	Files []InputFile       `yaml:"files,omitempty" json:"files,omitempty"`
}

type InputFile struct {
	Path    string `yaml:"path"              json:"path"`
	Content string `yaml:"content,omitempty" json:"content,omitempty"`
}

type Outputs struct {
	Files  []string `yaml:"files,omitempty"  json:"files,omitempty"`
	Values []string `yaml:"values,omitempty" json:"values,omitempty"`
}

// Parse разбирает YAML в Task. Отклоняет неизвестные поля,
// несколько документов и пустой вход.
// Семантическая проверка - отдельно, в Validate.
func Parse(data []byte) (Task, error) {
	if len(data) == 0 {
		return Task{}, errors.New("task is empty")
	}
	if len(data) > MaxYAMLBytes {
		return Task{}, fmt.Errorf("task is too large: %d bytes (max %d)", len(data), MaxYAMLBytes)
	}

	var t Task
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)

	if err := dec.Decode(&t); err != nil {
		if errors.Is(err, io.EOF) {
			return Task{}, errors.New("task is empty")
		}
		return Task{}, fmt.Errorf("invalid yaml: %w", err)
	}

	// Второй документ после `---` - не поддерживаем.
	var extra any
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
		// ok: один документ
	case err != nil:
		return Task{}, fmt.Errorf("invalid yaml: %w", err)
	default:
		return Task{}, errors.New("multiple YAML documents are not supported (use one file per task)")
	}

	if t.Name == "" && t.Image == "" && len(t.Steps) == 0 {
		return Task{}, errors.New("task is empty (no name, image, or steps)")
	}
	return t, nil
}

// Validate проверяет семантику задачи: обязательные поля, размеры.
// Возвращает первую ошибку с указанием пути, например "steps[2].run".
func Validate(t Task) error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New(`field "name" is required`)
	}
	if len(t.Name) > MaxNameLen {
		return fmt.Errorf(`field "name" is too long (%d bytes, max %d)`, len(t.Name), MaxNameLen)
	}
	if t.Image != "" && strings.ContainsAny(t.Image, " \t\n") {
		return fmt.Errorf(`field "image": %q contains whitespace`, t.Image)
	}

	if len(t.Steps) == 0 {
		return errors.New(`field "steps" is required and must contain at least one step`)
	}
	if len(t.Steps) > MaxSteps {
		return fmt.Errorf(`field "steps": too many steps (%d, max %d)`, len(t.Steps), MaxSteps)
	}
	for i, s := range t.Steps {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf(`field "steps[%d].name" is required`, i)
		}
		if len(s.Name) > MaxStepNameLen {
			return fmt.Errorf(`field "steps[%d].name" is too long (%d bytes, max %d)`,
				i, len(s.Name), MaxStepNameLen)
		}
		if strings.TrimSpace(s.Run) == "" {
			return fmt.Errorf(`field "steps[%d].run" is required`, i)
		}
		if len(s.Run) > MaxStepRunLen {
			return fmt.Errorf(`field "steps[%d].run" is too long (%d bytes, max %d)`,
				i, len(s.Run), MaxStepRunLen)
		}
	}

	if t.Timeout < 0 {
		return errors.New(`field "timeout" must not be negative`)
	}
	return nil
}
