#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the demo cluster context}"
: "${IMAGE_TAG:?Set IMAGE_TAG to a new tag already loaded on the demo cluster}"
namespace=${KUBE_NAMESPACE:-governance}
results=${ROLLOUT_RESULTS:-rollout-results.csv}
kube=(kubectl --context "$KUBE_CONTEXT" --namespace "$namespace")
pod="governance-probe-$(openssl rand -hex 4)"
base=http://governance-broker:8080
worker=
cleanup() {
  if [[ -n "$worker" ]]; then
    kill "$worker" 2>/dev/null || true
    wait "$worker" 2>/dev/null || true
  fi
  "${kube[@]}" delete pod "$pod" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

current_image=$("${kube[@]}" get deployment governance-broker -o jsonpath='{.spec.template.spec.containers[0].image}')
if [[ "$current_image" == "governance:$IMAGE_TAG" ]]; then
  echo "IMAGE_TAG must differ from the deployed tag" >&2
  exit 1
fi
"${kube[@]}" run "$pod" --restart=Never --image=curlimages/curl:8.14.1 --command -- sleep 900
"${kube[@]}" wait --for=condition=Ready "pod/$pod" --timeout=120s
email=$(sed -n 's/^BOOTSTRAP_EMAIL=//p' .env)
password=$(sed -n 's/^BOOTSTRAP_PASSWORD=//p' .env)
token=$(jq -nc --arg email "$email" --arg password "$password" '{email:$email,password:$password}' |
  "${kube[@]}" exec -i "$pod" -- curl -fsS --max-time 12 -H 'Content-Type: application/json' \
    --data-binary @- "$base/v1/login" | jq -er '.token')

printf 'unix_seconds,http_status\n' > "$results"
printf '%s\n' "$token" | "${kube[@]}" exec -i "$pod" -- sh -ec '
  read -r token
  while [ ! -f /tmp/stop-probe ]; do
    code=$(curl -s --max-time 10 -o /dev/null -w "%{http_code}" \
      -H "Authorization: Bearer $token" -H "Content-Type: application/json" \
      --data "{\"message\":\"rollout probe\"}" "$1/v1/invoke/demo-echo") || true
    printf "%s,%s\n" "$(date +%s)" "${code:-000}"
    sleep 1
  done
' probe "$base" >> "$results" &
worker=$!

for _ in {1..20}; do
  kill -0 "$worker"
  if [[ $(wc -l < "$results") -ge 2 ]]; then break; fi
  sleep 1
done
[[ $(wc -l < "$results") -ge 2 ]] || { echo "No probe samples" >&2; exit 1; }
[[ $(sed -n '2s/.*,//p' "$results") == 200 ]] ||
  { echo "Baseline failed. Run scripts/demo.sh against this cluster first." >&2; exit 1; }

helm upgrade governance deploy/helm/governance --reuse-values \
  --kube-context "$KUBE_CONTEXT" --namespace "$namespace" \
  --set-string image.tag="$IMAGE_TAG" --atomic --wait --timeout 5m

sleep 5
"${kube[@]}" exec "$pod" -- touch /tmp/stop-probe
wait "$worker"
worker=

awk -F, 'NR > 1 { total++; if ($2 == "200") good++ }
  END {
    if (total < 5) { print "Insufficient probe samples"; exit 1 }
    printf "Successful requests during the observed release window: %d/%d (%.2f%%)\n", good, total, 100 * good / total
    exit (good != total)
  }' "$results"
