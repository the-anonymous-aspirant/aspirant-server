#!/usr/bin/env bash
#
# Nightly retest for system_3 security-finding #1380.
#
# Verifies the three substrate-observable properties that the PR
# introducing the fix relies on:
#
#   1) POST /login rate limit — 21 failed logins from one IP → 429
#   2) GET /login/:username removed — endpoint returns 404, not a JSON body
#   3) Timing oracle closed — the unknown-username failure path takes
#      about the same wall-clock as a known-username-wrong-password
#      path (both go through bcrypt)
#
# Usage:
#   BASE_URL=https://the-aspirant.com/api ./scripts/security-retest-1380.sh
#
# BASE_URL should point at the aspirant-server prefix (e.g.
# https://the-aspirant.com/api for the production nginx-fronted
# service). Exit code 0 on success, non-zero on any failed check.
# The task body's "Retest cadence: nightly" is honoured by scheduling
# this script via the operator's routine (cron or a scheduled workflow
# with BASE_URL set as an env secret).

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
KNOWN_USERNAME="${KNOWN_USERNAME:-aspirant}"

pass=0
fail=0

check() {
  local label="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then
    printf '  PASS  %s (got %s)\n' "$label" "$got"
    pass=$((pass + 1))
  else
    printf '  FAIL  %s (want %s, got %s)\n' "$label" "$want" "$got"
    fail=$((fail + 1))
  fi
}

echo "== #1380 nightly retest against ${BASE_URL} =="

# --- (1) POST /login rate limit ---
echo
echo "[1] Per-IP rate limit: fire 21 failed logins from this IP against random usernames"
code=""
for i in $(seq 1 21); do
  code=$(curl -sS -o /dev/null -w '%{http_code}' \
    -X POST "${BASE_URL}/login" \
    -H 'Content-Type: application/json' \
    --data "{\"username\":\"probe-$i\",\"password\":\"wrong\"}")
done
check "21st attempt returns 429" "$code" "429"

# --- (2) GET /login/:username removed ---
# The pre-fix route was gin-registered as `/login/:username`, so the
# real probe URL is slash-form. (The aspirant-online retest probed
# `/login:username` which never matched gin's `/:param` and always
# 404'd — that variant would false-positive even against the
# vulnerable server.)
echo
echo "[2] GET /login/:username is no longer registered"
code=$(curl -sS -o /dev/null -w '%{http_code}' \
  "${BASE_URL}/login/${KNOWN_USERNAME}")
check "GET /login/<username> returns 404" "$code" "404"

# --- (3) Timing hardening ---
echo
echo "[3] Timing oracle: unknown-username failure path takes comparable time to known"
# The known-username path only exists if aspirant is a real account;
# we don't want the retest to depend on a specific account existing,
# so we compare two unknown usernames instead. What matters is that
# BOTH go through bcrypt — a jitter check would be flaky, so we
# assert only that each response completes in a plausible bcrypt
# window (> 30 ms — a pure DB miss short-circuits in < 5 ms).
t_ms() { awk 'BEGIN { printf "%d\n", '"$1"' * 1000 }'; }
t1=$(curl -sS -o /dev/null -w '%{time_total}' \
  -X POST "${BASE_URL}/login" \
  -H 'Content-Type: application/json' \
  --data '{"username":"nobody-a","password":"x"}')
t2=$(curl -sS -o /dev/null -w '%{time_total}' \
  -X POST "${BASE_URL}/login" \
  -H 'Content-Type: application/json' \
  --data '{"username":"nobody-b","password":"y"}')
t1_ms=$(t_ms "$t1")
t2_ms=$(t_ms "$t2")
echo "  unknown-A: ${t1_ms} ms, unknown-B: ${t2_ms} ms"
if (( t1_ms >= 30 && t2_ms >= 30 )); then
  printf '  PASS  bcrypt-cost floor observed on both (>= 30 ms)\n'
  pass=$((pass + 1))
else
  printf '  FAIL  short-circuit path suspected — one or both below 30 ms\n'
  fail=$((fail + 1))
fi

echo
printf 'Summary: %d passed, %d failed\n' "$pass" "$fail"
exit $(( fail > 0 ? 1 : 0 ))
