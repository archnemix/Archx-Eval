#!/usr/bin/env bash
# End-to-end smoke test: boots the relay against the sample bank, exercises the
# public API, and prints a compact report. Runs in CI and locally.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${PORT:-18080}"
DATA="$(mktemp -d)"
LOG="$(mktemp)"
trap 'kill "${PID:-0}" 2>/dev/null || true; rm -rf "$DATA"' EXIT

echo "== building =="
(cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o "$DATA/relayd" ./cmd/relayd)

echo "== starting relay on :$PORT =="
ADDR=":$PORT" \
DATA_DIR="$DATA/store" \
BANK_DIR="$ROOT/testdata/bank" \
RELAY_SALT="smoke-test-salt" \
ALLOW_LOOPBACK=1 \
"$DATA/relayd" >"$LOG" 2>&1 &
PID=$!

for _ in $(seq 1 50); do
  if curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then break; fi
  sleep 0.1
done

pass=0; fail=0
check() {
  local name="$1" want="$2" got="$3"
  if [[ "$got" == "$want" ]]; then
    printf '  PASS  %-42s %s\n' "$name" "$got"; pass=$((pass+1))
  else
    printf '  FAIL  %-42s want=%s got=%s\n' "$name" "$want" "$got"; fail=$((fail+1))
  fi
}

health_code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/health")
check "health reachable" "200" "$health_code"

cats=$(curl -s "http://127.0.0.1:$PORT/v1/categories")
check "categories listed" "true" "$(grep -q '"categories"' <<<"$cats" && echo true)"

body='{"categories":["python"],"size":5,"device":{"machine_id":"4c4c4544-0044-3510-8054-b4c04f4e4e32","disk_uuid":"9f1b2c3d4e5f60718293a4b5c6d7e8f9","os_family":"linux","arch":"x86_64"}}'

first=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$PORT/v1/draw")
first_code=$(tail -n1 <<<"$first")
first_body=$(sed '$d' <<<"$first")
check "first draw granted" "200" "$first_code"
check "draw returns 5 examples" "5" "$(grep -o '"count":[0-9]*' <<<"$first_body" | cut -d: -f2)"

leak=false
for bad in expected_answer reference_solution explanation '"tests"'; do
  if grep -q -- "$bad" <<<"$first_body"; then leak=true; fi
done
check "no answer material leaked" "false" "$leak"

second_code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$PORT/v1/draw")
check "second draw refused" "429" "$second_code"

retry=$(curl -s -D - -o /dev/null -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$PORT/v1/draw" | tr -d '\r' | awk -F': ' 'tolower($1)=="retry-after"{print $2}')
check "retry-after advertised" "true" "$([[ -n "$retry" ]] && echo true || echo false)"

# A rotated IP with the same machine id must still be the same person.
hdr=$(curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -H 'X-Forwarded-For: 198.51.100.77' -d "$body" "http://127.0.0.1:$PORT/v1/draw")
check "ip rotation alone still blocked" "429" "$hdr"

feedback='{"request_id":"smoke","model_id":"smoke-model","items":[{"example_id":"py-easy-001","category":"python","difficulty":"easy","passed":true,"score":1}]}'
fb_code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d "$feedback" "http://127.0.0.1:$PORT/v1/feedback")
check "feedback accepted" "200" "$fb_code"

echo
echo "== store layout =="
find "$DATA/store" -type f | sed "s|$DATA/store/||" | sort | head -20

echo
echo "== events (first 3) =="
cat "$DATA"/store/events/*.jsonl 2>/dev/null | head -3

echo
echo "result: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
