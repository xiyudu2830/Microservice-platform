#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ -f .env ]]; then
  echo "Using existing .env"
  exit 0
fi
umask 077
tmp=$(mktemp .env.XXXXXX)
trap 'rm -f "$tmp"' EXIT
{
  printf 'BOOTSTRAP_EMAIL=admin@example.test\n'
  for name in BOOTSTRAP_PASSWORD INTERNAL_RPC_KEY POSTGRES_PASSWORD MONGO_PASSWORD; do
    printf '%s=%s\n' "$name" "$(openssl rand -hex 24)"
  done
} > "$tmp"
if ! ln "$tmp" .env; then
  echo "Could not create .env" >&2
  exit 1
fi
echo "Created .env with local demo credentials"
