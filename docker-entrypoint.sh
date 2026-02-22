#!/bin/sh
set -eu

CONFIG_PATH="${WORKER_CONFIG_PATH:-/app/config.json}"

# If config file does not exist, allow passing it inline via env var (useful on Railway).
if [ ! -f "${CONFIG_PATH}" ] && [ -n "${WORKER_CONFIG_JSON:-}" ]; then
  printf '%s' "${WORKER_CONFIG_JSON}" > "${CONFIG_PATH}"
fi

exec /usr/local/bin/codenite-worker "$@"
