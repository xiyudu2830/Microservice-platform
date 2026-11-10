#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

base=${BASE_URL:-http://127.0.0.1:8080}
mailpit=${MAILPIT_URL:-http://127.0.0.1:8025}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

env_value() {
  sed -n "s/^$1=//p" .env
}

call() {
  curl --silent --show-error --max-time 12 --output "$tmp/body" --write-out '%{http_code}' "$@"
}

expect() {
  local expected=$1 actual
  shift
  actual=$(call "$@")
  if [[ "$actual" != "$expected" ]]; then
    printf 'Expected HTTP %s, got %s for %s\n' "$expected" "$actual" "${*: -1}" >&2
    jq -c '{error}' "$tmp/body" >&2 || true
    exit 1
  fi
}

login() {
  jq -nc --arg email "$1" --arg password "$2" '{email:$email,password:$password}' |
    expect 200 -H 'Content-Type: application/json' --data-binary @- "$base/v1/login"
  jq -er '.token' "$tmp/body"
}

write_route() {
  expect 200 -X PUT -H "$admin" -H 'Content-Type: application/json' \
    --data "$1" "$base/v1/routes/demo-echo"
}

invoke() {
  expect 200 -H "$viewer" -H "X-Region: $1" -H 'Content-Type: application/json' \
    --data '{"message":"Hello from the governance platform"}' "$base/v1/invoke/demo-echo"
  jq -e --arg backend "$2" '.backend == $backend and (.request_id | length == 32)' "$tmp/body" >/dev/null
  jq -c . "$tmp/body"
}

admin="Authorization: Bearer $(login "${BOOTSTRAP_EMAIL:-$(env_value BOOTSTRAP_EMAIL)}" "${BOOTSTRAP_PASSWORD:-$(env_value BOOTSTRAP_PASSWORD)}")"
suffix=$(openssl rand -hex 5)
viewer_email="viewer-$suffix@example.test"
viewer_password=$(openssl rand -hex 16)
jq -nc --arg email "$viewer_email" --arg password "$viewer_password" \
  '{email:$email,password:$password,role:"viewer"}' |
  expect 201 -H "$admin" -H 'Content-Type: application/json' --data-binary @- "$base/v1/users"
viewer="Authorization: Bearer $(login "$viewer_email" "$viewer_password")"

expect 401 "$base/v1/routes"
expect 403 -X PUT -H "$viewer" "$base/v1/routes/demo-echo"
expect 404 -H "$viewer" --data '{"message":"missing"}' "$base/v1/invoke/missing-$suffix"
echo "Verified HTTP 401, 403 and 404"

write_route '{"default_backend":"standard","rules":[{"role":"viewer","region":"us-east","backend":"priority"}]}'
expect 200 -H "$viewer" "$base/v1/routes"
jq -e '.routes[] | select(.service == "demo-echo")' "$tmp/body" >/dev/null
invoke us-east priority
invoke eu-west standard
write_route '{"default_backend":"standard","rules":[{"region":"eu-west","backend":"priority"}]}'
invoke eu-west priority
request_id=$(jq -er '.request_id' "$tmp/body")

subject="Governance demo $suffix"
jq -nc --arg subject "$subject" '{to:"ops@example.test",subject:$subject,body:"The routing policy was updated."}' |
  expect 204 -H "$admin" -H 'Content-Type: application/json' --data-binary @- "$base/v1/notifications"

if [[ "${CHECK_MAILPIT:-1}" == 1 ]]; then
  found=0
  for _ in {1..10}; do
    if curl --silent --show-error --fail --max-time 3 "$mailpit/api/v1/messages" |
      jq -e --arg subject "$subject" 'any(.messages[]; .Subject == $subject)' >/dev/null; then
      found=1
      break
    fi
    sleep 1
  done
  [[ $found == 1 ]] || { echo "Email did not appear in Mailpit" >&2; exit 1; }
fi

expect 403 -H "$viewer" "$base/v1/audit"
expect 200 -H "$admin" "$base/v1/audit"
jq -e --arg id "$request_id" \
  '.events[] | select(.request_id == $id and .status == 200 and .path == "/v1/invoke/{service}")' "$tmp/body"
echo "Demo passed"
