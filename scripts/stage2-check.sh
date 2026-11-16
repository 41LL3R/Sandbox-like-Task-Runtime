#!/usr/bin/env bash
# Проверка критериев приёмки этапа 2 (YAML-задачи).
#
#   go run ./cmd/sandboxd
#   ./scripts/stage2-check.sh
set -euo pipefail

BASE="${SANDBOX_URL:-http://localhost:8080}"
fail() { echo "FAIL: $*" >&2; exit 1; }
field() { sed -n "s/^  \"$1\": \"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}$/\1/p" | head -n1; }

create_yaml() {
  local resp
  resp=$(curl -sS -X POST "$BASE/runs" \
    -H 'Content-Type: application/yaml' --data-binary "$1")
  local id
  id=$(echo "$resp" | field id)
  [ -n "$id" ] || fail "POST /runs (yaml) did not return id: $resp"
  echo "$id"
}

wait_final() {
  local id=$1 s
  for _ in $(seq 1 120); do
    s=$(curl -sS "$BASE/runs/$id" | field status)
    case "$s" in succeeded|failed) echo "$s"; return;; esac
    sleep 0.5
  done
  fail "run $id did not finish"
}

echo "== 1. Многошаговая задача: шаги идут по порядку, маркеры в логах"
ID=$(create_yaml 'name: multi
image: alpine:3.20
timeout: 30
steps:
  - name: first
    run: echo one
  - name: second
    run: echo two
')
s=$(wait_final "$ID")
[ "$s" = succeeded ] || fail "multi-step status = $s"
out=$(curl -sS "$BASE/runs/$ID/logs?stream=stdout")
echo "$out" | grep -q "=== step 1/2: first"  || fail "missing marker 1"
echo "$out" | grep -q "=== step 2/2: second" || fail "missing marker 2"
echo "$out" | grep -qx "one" || fail "missing output one"
echo "$out" | grep -qx "two" || fail "missing output two"
echo "   OK: оба шага отработали, маркеры на месте"

echo "== 2. Ошибка шага → failed, exit_code шага, следующие шаги не выполняются"
ID=$(create_yaml 'name: fail
image: alpine:3.20
timeout: 30
steps:
  - name: first
    run: echo before
  - name: boom
    run: exit 7
  - name: never
    run: echo "must not run"
')
s=$(wait_final "$ID")
[ "$s" = failed ] || fail "fail-step status = $s"
code=$(curl -sS "$BASE/runs/$ID" | field exit_code)
[ "$code" = 7 ] || fail "fail-step exit_code = $code, want 7"
out=$(curl -sS "$BASE/runs/$ID/logs?stream=stdout")
echo "$out" | grep -q "=== step 1/3: first" || fail "missing marker 1"
echo "$out" | grep -q "=== step 2/3: boom"  || fail "missing marker 2"
if echo "$out" | grep -q "=== step 3/3: never"; then
  fail "step 3 should not have run"
fi
if echo "$out" | grep -q "must not run"; then
  fail "step 3 output should not appear"
fi
echo "   OK: failed, exit_code=7, шаг 3 не выполнился"

echo "== 3. JSON-логи содержат поле truncated"
resp=$(curl -sS "$BASE/runs/$ID/logs")
echo "$resp" | grep -q '"truncated"' || fail "truncated field missing in JSON logs"
echo "   OK: truncated есть"

echo "== 4. Некорректный YAML отклоняется с понятным сообщением"
resp=$(curl -sS -w '\n%{http_code}' -X POST "$BASE/runs" \
  -H 'Content-Type: application/yaml' --data-binary 'name: x
steps:')
[ "$(echo "$resp" | tail -n1)" = 400 ] || fail "invalid YAML was not rejected: $resp"
echo "$resp" | head -n -1 | grep -q '"error"' || fail "no error message"
echo "   OK: 400 с сообщением"

echo "== 5. Неизвестное поле в YAML отклоняется"
resp=$(curl -sS -w '\n%{http_code}' -X POST "$BASE/runs" \
  -H 'Content-Type: application/yaml' --data-binary 'name: x
stepz:
  - name: a
    run: b')
[ "$(echo "$resp" | tail -n1)" = 400 ] || fail "unknown field was not rejected: $resp"
echo "   OK: неизвестное поле → 400"

echo "== 6. Образ по умолчанию применяется, если image не указан"
ID=$(create_yaml 'name: no-image
steps:
  - name: whoami
    run: echo ok
')
s=$(wait_final "$ID")
[ "$s" = succeeded ] || fail "no-image status = $s"
echo "   OK: задача без image исполнилась"

echo
echo "Этап 2: все проверки пройдены."
