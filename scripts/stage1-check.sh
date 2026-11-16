#!/usr/bin/env bash
# Проверка критериев приёмки этапа 1 против запущенного сервера.
#
#   go run ./cmd/sandboxd          # в одном терминале
#   ./scripts/stage1-check.sh      # в другом
#
# Нужны только curl и docker (для проверки, что контейнеры удалены).
set -euo pipefail

BASE="${SANDBOX_URL:-http://localhost:8080}"
fail() { echo "FAIL: $*" >&2; exit 1; }

# Достаёт строковое или числовое поле верхнего уровня из JSON без jq.
field() { sed -n "s/^  \"$1\": \"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}$/\1/p" | head -n1; }

create() {
  local resp
  resp=$(curl -sS -X POST "$BASE/runs" -H 'Content-Type: application/json' -d "$1")
  local id
  id=$(echo "$resp" | field id)
  [ -n "$id" ] || fail "POST /runs did not return id: $resp"
  echo "$id"
}

status() { curl -sS "$BASE/runs/$1" | field status; }

wait_final() {
  local id=$1 s
  for _ in $(seq 1 120); do
    s=$(status "$id")
    case "$s" in succeeded|failed) echo "$s"; return;; esac
    sleep 0.5
  done
  fail "run $id did not finish"
}

echo "== 1. hello-world: запуск, код возврата, логи через API"
id=$(create '{"image":"hello-world"}')
echo "   run id: $id"
s=$(wait_final "$id")
[ "$s" = succeeded ] || fail "hello-world status = $s"
code=$(curl -sS "$BASE/runs/$id" | field exit_code)
[ "$code" = 0 ] || fail "hello-world exit_code = $code"
curl -sS "$BASE/runs/$id/logs?stream=stdout" | grep -q "Hello from Docker!" \
  || fail "hello-world logs do not contain greeting"
echo "   OK: succeeded, exit_code=0, логи содержат 'Hello from Docker!'"

echo "== 2. sleep 5: сервер отвечает сразу и сам замечает завершение"
start=$(date +%s)
id=$(create '{"image":"alpine:3.20","cmd":["sleep","5"]}')
[ $(( $(date +%s) - start )) -le 2 ] || fail "POST /runs waited for the container"
sleep 1.5
s=$(status "$id")
[ "$s" = running ] || fail "status while sleeping = $s, want running"
echo "   через ~1.5 с статус: $s"
s=$(wait_final "$id")
elapsed=$(( $(date +%s) - start ))
[ "$s" = succeeded ] || fail "sleep status = $s"
[ "$elapsed" -ge 5 ] || fail "finished too early ($elapsed s)"
echo "   OK: succeeded примерно через ${elapsed} с"

echo "== 3. ненулевой код возврата и раздельные stdout/stderr"
id=$(create '{"image":"alpine:3.20","cmd":["sh","-c","echo out; echo err >&2; exit 3"]}')
s=$(wait_final "$id")
code=$(curl -sS "$BASE/runs/$id" | field exit_code)
[ "$s" = failed ] && [ "$code" = 3 ] || fail "status=$s exit_code=$code, want failed/3"
[ "$(curl -sS "$BASE/runs/$id/logs?stream=stdout")" = out ] || fail "stdout mismatch"
[ "$(curl -sS "$BASE/runs/$id/logs?stream=stderr")" = err ] || fail "stderr mismatch"
echo "   OK: failed, exit_code=3, stdout='out', stderr='err'"

echo "== 4. таймаут: контейнер принудительно останавливается"
id=$(create '{"image":"alpine:3.20","cmd":["sleep","60"],"timeout_seconds":2}')
s=$(wait_final "$id")
[ "$s" = failed ] || fail "timeout status = $s"
curl -sS "$BASE/runs/$id" | grep -q '"error": "timeout' || fail "no timeout error"
echo "   OK: failed с ошибкой timeout"

echo "== 5. некорректный запрос отклоняется с понятным сообщением"
resp=$(curl -sS -w '\n%{http_code}' -X POST "$BASE/runs" -d '{"image": 5}')
[ "$(echo "$resp" | tail -n1)" = 400 ] || fail "bad request was not rejected: $resp"
echo "   OK: 400 — $(echo "$resp" | head -n -1 | field error)"

echo "== 6. завершённые контейнеры удалены"
left=$(docker ps -a --filter label=sandbox.managed=true -q | wc -l)
[ "$left" -eq 0 ] || fail "$left containers left"
echo "   OK: контейнеров с меткой sandbox.managed не осталось"

echo
echo "Этап 1: все проверки пройдены."
