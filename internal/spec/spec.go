// Package spec описывает декларативный формат задачи (YAML)
package spec

import (
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Регулярные выражения.
var (
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	labelPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
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
	if err := validateTaskOptions(t); err != nil {
		return err
	}
	return nil
}

func validateTaskOptions(t Task) error {
	if t.Resources != nil {
		for name, resource := range map[string]*ResourceSpec{
			"requests": t.Resources.Requests,
			"limits":   t.Resources.Limits,
		} {
			if resource == nil {
				continue
			}

			if resource.CPU != "" {
				value, err := strconv.ParseFloat(resource.CPU, 64)
				if err != nil || value <= 0 {
					return fmt.Errorf(
						`field "resources.%s.cpu" must be a positive number`,
						name,
					)
				}
			}

			if resource.Memory != "" {
				if _, err := ParseMemory(resource.Memory); err != nil {
					return fmt.Errorf(
						`field "resources.%s.memory": %w`,
						name,
						err,
					)
				}
			}
		}
	}

	if t.Runner != nil {
		for i, label := range t.Runner.Labels {
			if !labelPattern.MatchString(label) {
				return fmt.Errorf(
					`field "runner.labels[%d]" contains invalid label`,
					i,
				)
			}
		}
	}

	if t.Inputs != nil {
		for name := range t.Inputs.Env {
			if !envNamePattern.MatchString(name) {
				return fmt.Errorf(
					`field "inputs.env": invalid variable name %q`,
					name,
				)
			}
		}

		for i, file := range t.Inputs.Files {
			if !path.IsAbs(file.Path) || path.Clean(file.Path) != file.Path ||
				file.Path == "/" || strings.Contains(file.Path, "..") {
				return fmt.Errorf(
					`field "inputs.files[%d].path" must be a clean absolute path`,
					i,
				)
			}
		}
	}

	if t.Outputs != nil {
		for i, name := range t.Outputs.Values {
			if !envNamePattern.MatchString(name) {
				return fmt.Errorf(
					`field "outputs.values[%d]" is not a valid environment variable name`,
					i,
				)
			}
		}

		for i, file := range t.Outputs.Files {
			if !path.IsAbs(file) || path.Clean(file) != file ||
				file == "/" || strings.Contains(file, "..") {
				return fmt.Errorf(
					`field "outputs.files[%d]" must be a clean absolute path`,
					i,
				)
			}
		}
	}

	return nil
}

func ParseMemory(value string) (int64, error) {
	units := []struct {
		suffix string
		factor int64
	}{
		{"Ki", 1 << 10},
		{"Mi", 1 << 20},
		{"Gi", 1 << 30},
		{"Ti", 1 << 40},
		{"K", 1000},
		{"M", 1000 * 1000},
		{"G", 1000 * 1000 * 1000},
	}

	for _, unit := range units {
		if strings.HasSuffix(value, unit.suffix) {
			number := strings.TrimSuffix(value, unit.suffix)
			n, err := strconv.ParseInt(number, 10, 64)
			if err != nil || n <= 0 || n > (1<<62)/unit.factor {
				return 0, fmt.Errorf("invalid memory value %q", value)
			}
			return n * unit.factor, nil
		}
	}

	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid memory value %q", value)
	}

	return n, nil
}
