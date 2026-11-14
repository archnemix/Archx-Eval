#!/usr/bin/env bash
# End-to-end smoke test against a real HTTP server.
#
# Unlike the unit tests this boots the actual binary, so it exercises the
# middleware chain, the filesystem layout and — critically — process restarts.
# The restart section is the regression test for the ban-reset hole that a
# sleeping Hugging Face Space would otherwise expose.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${PORT:-18080}"
WORK="$(mktemp -d)"
LOG="$WORK/relay.log"
PID=""

cleanup() {
  [[ -n "$PID" ]] && kill "$PID" 2>/dev/null || true
  wait "$PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

stop_relay() {
  [[ -n "$PID" ]] || return 0
  kill "$PID" 2>/dev/null || true
  for _ in $(seq 1 50); do
    kill -0 "$PID" 2>/dev/null || break
    sleep 0.1
  done
  PID=""
}

start_relay() {
  ADDR=":$PORT" \
  DATA_DIR="$WORK/store" \
  RATE_DIR="$WORK/rate" \
  BANK_DIR="$WORK/bank" \
  RELAY_SALT="smoke-test-salt" \
  ACCESS_TOKEN="smoke-token" \
  ADMIN_TOKEN="smoke-admin" \
  METRICS_TOKEN="smoke-metrics" \
  ALLOW_LOOPBACK=1 \
  LOG_FORMAT=text \
  "$WORK/relayd" >>"$LOG" 2>&1 &
  PID=$!
  for _ in $(seq 1 80); do
    if curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  echo "relay failed to start; log follows:"; cat "$LOG"; return 1
}

pass=0; fail=0
check() {
  local name="$1" want="$2" got="$3"
  if [[ "$got" == "$want" ]]; then
    printf '  PASS  %-46s %s\n' "$name" "$got"; pass=$((pass+1))
  else
    printf '  FAIL  %-46s want=%s got=%s\n' "$name" "$want" "$got"; fail=$((fail+1))
  fi
}
check_ne() {
  local name="$1" unwanted="$2" got="$3"
  if [[ "$got" != "$unwanted" ]]; then
    printf '  PASS  %-46s %s\n' "$name" "$got"; pass=$((pass+1))
  else
    printf '  FAIL  %-46s unexpectedly %s\n' "$name" "$got"; fail=$((fail+1))
  fi
}

post() { # post <path> <body> [extra-header]
  local extra=()
  [[ -n "${3:-}" ]] && extra=(-H "$3")
  curl -s -H 'Content-Type: application/json' -H 'Authorization: Bearer smoke-token' \
       "${extra[@]}" -d "$2" "http://127.0.0.1:$PORT$1"
}
status() { # status <curl args...>
  curl -s -o /dev/null -w '%{http_code}' "$@"
}

echo "== build =="
(cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o "$WORK/relayd" ./cmd/relayd)

# Work on a copy: the bank is read-only fixture data and must never be mutated
# by a test run (a stale usage.json there silently breaks every later run).
cp -r "$ROOT/testdata/bank" "$WORK/bank"
rm -f "$WORK/bank/usage.json"

echo "== start =="
start_relay

draw='{"categories":["python"],"size":5,"device":{"machine_id":"4c4c4544-0044-3510-8054-b4c04f4e4e32","disk_uuid":"9f1b2c3d4e5f60718293a4b5c6d7e8f9","os_family":"linux","arch":"x86_64"}}'
idA='4c4c4544-0044-3510-8054-b4c04f4e4e32'
idB='5d5d4544-0055-3510-8054-b4c04f4e4e88'

echo
echo "== authentication =="
check "health is public" "200" "$(status "http://127.0.0.1:$PORT/health")"
check "draw without token refused" "403" \
  "$(status -H 'Content-Type: application/json' -d "$draw" "http://127.0.0.1:$PORT/v1/draw")"
check "draw with wrong token refused" "403" \
  "$(status -H 'Content-Type: application/json' -H 'Authorization: Bearer wrong' -d "$draw" "http://127.0.0.1:$PORT/v1/draw")"
check "metrics without token refused" "403" "$(status "http://127.0.0.1:$PORT/metrics")"

echo
echo "== transport hardening =="
hdrs=$(curl -s -D - -o /dev/null "http://127.0.0.1:$PORT/health" | tr -d '\r')
for h in "X-Content-Type-Options: nosniff" "X-Frame-Options: DENY" \
         "Referrer-Policy: no-referrer" "Cross-Origin-Resource-Policy: same-origin"; do
  check "header ${h%%:*}" "1" "$(grep -ci "^$h$" <<<"$hdrs")"
done
check "csp present" "1" "$(grep -ci '^Content-Security-Policy: default-src' <<<"$hdrs")"
check "hsts present" "1" "$(grep -ci '^Strict-Transport-Security:' <<<"$hdrs")"
check "wrong content-type rejected" "400" \
  "$(status -H 'Content-Type: text/plain' -H 'Authorization: Bearer smoke-token' -d "$draw" "http://127.0.0.1:$PORT/v1/draw")"

echo
echo "== happy path =="
first=$(post /v1/draw "$draw")
check "first draw granted" "5" "$(grep -o '"count":[0-9]*' <<<"$first" | cut -d: -f2)"
leak=false
for bad in expected_answer reference_solution explanation '"tests"'; do
  grep -q -- "$bad" <<<"$first" && leak=true
done
check "no answer material leaked" "false" "$leak"

echo
echo "== window enforcement =="
check "second draw refused" "429" "$(status -H 'Content-Type: application/json' -H 'Authorization: Bearer smoke-token' -d "$draw" "http://127.0.0.1:$PORT/v1/draw")"
retry=$(curl -s -D - -o /dev/null -H 'Content-Type: application/json' -H 'Authorization: Bearer smoke-token' -d "$draw" "http://127.0.0.1:$PORT/v1/draw" | tr -d '\r' | awk -F': ' 'tolower($1)=="retry-after"{print $2}')
check_ne "retry-after advertised" "" "$retry"
check "ip rotation alone still blocked" "429" \
  "$(status -H 'Content-Type: application/json' -H 'Authorization: Bearer smoke-token' -H 'X-Forwarded-For: 198.51.100.77' -d "$draw" "http://127.0.0.1:$PORT/v1/draw")"

echo
echo "== restart persistence (the Space-sleep hole) =="
stop_relay
start_relay
check "window survives restart" "429" \
  "$(status -H 'Content-Type: application/json' -H 'Authorization: Bearer smoke-token' -d "$draw" "http://127.0.0.1:$PORT/v1/draw")"

echo
echo "== spam ladder + ban persistence =="
banned=false
for i in $(seq 1 25); do
  code=$(status -H 'Content-Type: application/json' -H 'Authorization: Bearer smoke-token' \
    -d "{\"categories\":[\"python\"],\"size\":1,\"device\":{\"machine_id\":\"$idB\",\"disk_uuid\":\"9f1b2c3d4e5f60718293a4b5c6d7e8f9\"}}" \
    "http://127.0.0.1:$PORT/v1/draw")
  if [[ "$code" == "429" ]]; then banned=true; fi
done
check "spamming is throttled" "true" "$banned"
stop_relay
start_relay
check "ban survives restart" "429" \
  "$(status -H 'Content-Type: application/json' -H 'Authorization: Bearer smoke-token' \
     -d "{\"categories\":[\"python\"],\"size\":1,\"device\":{\"machine_id\":\"$idB\",\"disk_uuid\":\"9f1b2c3d4e5f60718293a4b5c6d7e8f9\"}}" \
     "http://127.0.0.1:$PORT/v1/draw")"

echo
echo "== metrics =="
metrics=$(curl -s -H 'Authorization: Bearer smoke-metrics' "http://127.0.0.1:$PORT/metrics")
check "metrics gated endpoint works" "true"   "$(grep -q 'archx_bank_examples_total' <<<"$metrics" && echo true || echo false)"
check "metrics exposes usable stock" "true"   "$(grep -q 'archx_bank_examples_usable' <<<"$metrics" && echo true || echo false)"

echo
echo "== on-disk layout =="
find "$WORK/store" "$WORK/rate" -type f 2>/dev/null | sed "s|$WORK/||" | sort | head -24

echo
echo "== event log sample =="
cat "$WORK"/store/events/*.jsonl 2>/dev/null | head -2

echo
echo "result: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
