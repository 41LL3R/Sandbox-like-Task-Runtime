package spec

import (
	"fmt"
	"strings"
)

// BuildScript превращает последовательность шагов в POSIX-shell-скрипт.
//
//	#!/bin/sh
//	set -e
//
//	echo '=== step 1/N: <name>'
//	<run>
//
// Флаг `set -e` даёт «ошибка шага → ошибка задачи»:
// если любая команда внутри шага падает (кроме `if`, `||`, `&&`, `while`),
// скрипт прерывается, и код возврата контейнера равен коду упавшей команды.
//
// Маркер `=== step i/N: <name>` печатается в stdout, чтобы в логах было
// видно, какой шаг исполняется.
// Маркеры нужны только для удобства чтения: финальный
// статус определяется по exit code контейнера, а не по логам.
func BuildScript(steps []Step, outputValues []string) string {
	var b strings.Builder
	b.Grow(64 + 32*len(steps))
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -e\n")
	n := len(steps)
	for i, s := range steps {
		fmt.Fprintf(&b, "\necho '=== step %d/%d: %s'\n", i+1, n, escapeForSingleQuote(s.Name))
		b.WriteString(s.Run)
		if !strings.HasSuffix(s.Run, "\n") {
			b.WriteString("\n")
		}
	}

	b.WriteString("\nmkdir -p /tmp/sandbox-outputs\n")

	for _, name := range outputValues {
		fmt.Fprintf(&b,
			"printf '%%s' \"${%s-}\" > '/tmp/sandbox-outputs/%s'\n",
			name, name,
		)
	}

	b.WriteString("\nexit 0\n")
	return b.String()
}

// escapeForSingleQuote экранирует строку для вставки внутрь
// shell-литерала в одинарных кавычках: ' → '\”.
func escapeForSingleQuote(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}
