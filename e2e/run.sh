#!/usr/bin/env bash
# Spin up the full local logai stack, run E2E assertions, then tear it down.
#
#   ./e2e/run.sh           # run and clean up
#   ./e2e/run.sh --keep    # leave the stack running afterwards
set -euo pipefail

cd "$(dirname "$0")"
COMPOSE="docker compose -f docker-compose.yml"
KEEP=0
[[ "${1:-}" == "--keep" ]] && KEEP=1

cleanup() {
  if [[ $KEEP -eq 0 ]]; then
    echo "=== Tearing down stack ==="
    $COMPOSE down -v --remove-orphans || true
  else
    echo "=== Leaving stack running (--keep). Stop with: $COMPOSE down -v ==="
  fi
}
trap cleanup EXIT

echo "=== Building & starting stack ==="
$COMPOSE up -d --build

echo "=== Waiting for logai /health ==="
for i in $(seq 1 60); do
  if curl -sf http://localhost:3000/health >/dev/null 2>&1; then
    echo "logai is up."
    break
  fi
  if [[ $i -eq 60 ]]; then
    echo "logai did not become healthy; recent logs:"
    $COMPOSE logs --tail=80 logai
    exit 1
  fi
  sleep 2
done

curl -s http://localhost:3000/health; echo

echo "=== Running E2E assertions ==="
set +e
python "$(pwd)/assert.py"
rc=$?
set -e

if [[ $rc -ne 0 ]]; then
  echo "=== logai logs (tail) ==="
  $COMPOSE logs --tail=120 logai
fi
exit $rc
