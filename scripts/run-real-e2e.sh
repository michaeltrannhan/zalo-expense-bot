#!/bin/sh
# Supervised real-provider E2E. It first matches a one-time challenge without
# sending anything, then pins pilot access to that sender and runs the normal
# API + worker binaries against an isolated database.
set -eu

: "${ZALO_BOT_TOKEN:?ZALO_BOT_TOKEN is required}"
: "${GEMINI_API_KEY:?GEMINI_API_KEY is required}"
: "${E2E_DATABASE_URL:?E2E_DATABASE_URL is required}"

# Resolve the connection string through pgx and validate its effective target;
# query parameters can override a URL's apparent path or host.
./bin/e2e validate-db

e2e_timeout=${E2E_TIMEOUT:-10m}
e2e_model=${GEMINI_MODEL:-gemini-3.6-flash}
e2e_api_base=${ZALO_API_BASE:-https://bot-api.zaloplatforms.com}
e2e_gemini_base=${GEMINI_API_BASE:-https://generativelanguage.googleapis.com}
e2e_data_dir=./data/e2e
identity_file=$(mktemp /tmp/zl-expense-e2e-identity.XXXXXX)
random_suffix=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
challenge="ZLE2E-${random_suffix}"
# Pilot config remains fail-closed even though this E2E uses long polling and
# never exposes a webhook listener. Supply a strong per-run secret rather than
# weakening validation or relying on a developer's placeholder .env value.
e2e_webhook_secret=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
stack_pid=
watch_pid=

cleanup() {
	if [ -n "$watch_pid" ]; then
		kill "$watch_pid" 2>/dev/null || true
		wait "$watch_pid" 2>/dev/null || true
	fi
	if [ -n "$stack_pid" ]; then
		kill "$stack_pid" 2>/dev/null || true
		wait "$stack_pid" 2>/dev/null || true
	fi
	rm -f -- "$identity_file"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf '%s\n' 'Checking the supplied credentials with read-only Zalo metadata and a synthetic Gemini OCR call...'
APP_ENV=development DATABASE_URL="$E2E_DATABASE_URL" \
	ZALO_API_BASE="$e2e_api_base" EXTRACTOR=gemini GEMINI_MODEL="$e2e_model" \
	GEMINI_API_BASE="$e2e_gemini_base" OBJECTSTORE=local \
	EXTRACTION_ENABLED=true OUTBOUND_ENABLED=true \
	./bin/e2e preflight

printf '\nSend this exact one-time text to your Zalo bot:\n\n  %s\n\n' "$challenge"
printf '%s\n' 'No reply is sent during discovery. Other senders and messages are ignored.'
ZALO_API_BASE="$e2e_api_base" ./bin/e2e discover \
	-challenge "$challenge" -identity-file "$identity_file" -timeout "$e2e_timeout"

provider_user_id=$(sed -n '1p' "$identity_file")
run_since=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

printf '%s\n' 'Preparing the isolated E2E schema and starting the normal three-process stack...'
APP_ENV=pilot DATABASE_URL="$E2E_DATABASE_URL" DATA_DIR="$e2e_data_dir" \
	ZALO_WEBHOOK_SECRET="$e2e_webhook_secret" \
	PILOT_ALLOWLIST="$provider_user_id" ZALO_API_BASE="$e2e_api_base" \
	OBJECTSTORE=local EXTRACTOR=gemini GEMINI_MODEL="$e2e_model" GEMINI_API_BASE="$e2e_gemini_base" \
	EXTRACTION_ENABLED=true OUTBOUND_ENABLED=true \
	./bin/migrate

APP_ENV=pilot DATABASE_URL="$E2E_DATABASE_URL" DATA_DIR="$e2e_data_dir" \
	ZALO_WEBHOOK_SECRET="$e2e_webhook_secret" \
	PILOT_ALLOWLIST="$provider_user_id" ZALO_API_BASE="$e2e_api_base" \
	OBJECTSTORE=local EXTRACTOR=gemini GEMINI_MODEL="$e2e_model" GEMINI_API_BASE="$e2e_gemini_base" \
	EXTRACTION_ENABLED=true OUTBOUND_ENABLED=true \
	./scripts/run-local.sh -poll &
stack_pid=$!

printf '\n%s\n\n' 'Stack ready. Send /batdau to the bot. The monitor will tell you when to send the test receipt and confirmation.'
DATABASE_URL="$E2E_DATABASE_URL" ./bin/e2e watch \
	-identity-file "$identity_file" -since "$run_since" -timeout "$e2e_timeout" -model "$e2e_model" &
watch_pid=$!

while kill -0 "$watch_pid" 2>/dev/null; do
	if ! kill -0 "$stack_pid" 2>/dev/null; then
		wait "$stack_pid" || true
		stack_pid=
		printf '%s\n' 'E2E stack stopped before verification completed.' >&2
		exit 1
	fi
	sleep 1
done

if wait "$watch_pid"; then
	watch_pid=
	printf '%s\n' 'Real-provider E2E completed; the isolated zl_expense_e2e database is retained for inspection.'
	exit 0
fi
watch_pid=
exit 1
