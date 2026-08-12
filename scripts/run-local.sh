#!/bin/sh
# Run the full local stack in one terminal: api + receipt-worker +
# notification-worker. Ctrl-C (or any process exiting) stops all three.
# Usage: scripts/run-local.sh [-poll]
set -e

./bin/api "$@" &
API=$!
./bin/receipt-worker &
RC=$!
./bin/notification-worker &
NF=$!

cleanup() {
	# Stop surviving siblings and reap every child. Individual kill failures
	# only mean that process was already the one that exited.
	kill "$API" "$RC" "$NF" 2>/dev/null || true
	wait "$API" "$RC" "$NF" 2>/dev/null || true
}
trap cleanup INT TERM EXIT

# POSIX sh has no portable `wait -n`. Poll liveness so one crashed process
# tears down the other two instead of leaving a misleading partial stack.
while kill -0 "$API" 2>/dev/null &&
	kill -0 "$RC" 2>/dev/null &&
	kill -0 "$NF" 2>/dev/null; do
	sleep 1
done

exit 1
