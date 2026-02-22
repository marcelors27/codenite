#!/bin/sh
set -eu

CONFIG_PATH="${WORKER_CONFIG_PATH:-/tmp/config.json}"
CONFIG_DIR="$(dirname "${CONFIG_PATH}")"
mkdir -p "${CONFIG_DIR}"

# If config file does not exist, allow passing it inline via env var (useful on Railway).
if [ ! -f "${CONFIG_PATH}" ] && [ -n "${WORKER_CONFIG_JSON:-}" ]; then
  printf '%s' "${WORKER_CONFIG_JSON}" > "${CONFIG_PATH}"
fi

if ! command -v codex >/dev/null 2>&1; then
  echo "codex CLI not found in PATH" >&2
  exit 127
fi

exec /usr/local/bin/codenite-worker "$@"
