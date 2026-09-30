#!/usr/bin/env bash
# Notify Ol1nBot about a deploy.
#   DEPLOY_HOOK_SECRET=… notify-deploy.sh <service> <version> <success|failed> [notes]
# Optional env: DEPLOY_ENV (prod), DEPLOY_COMMIT, DEPLOY_URL, DEPLOY_REPO (owner/name),
#               DEPLOY_DURATION (seconds), OL1NBOT_HOOK_URL.
set -euo pipefail

service=${1:?service}
version=${2:?version}
status=${3:?status success|failed}
notes=${4:-}
url=${OL1NBOT_HOOK_URL:-https://hooks.ol1n.com/discordbot/deploy}

body=$(jq -cn \
  --arg service "$service" --arg version "$version" --arg status "$status" --arg notes "$notes" \
  --arg env "${DEPLOY_ENV:-prod}" --arg commit "${DEPLOY_COMMIT:-}" --arg url "${DEPLOY_URL:-}" \
  --arg repo "${DEPLOY_REPO:-}" --argjson duration "${DEPLOY_DURATION:-0}" \
  '{service:$service, env:$env, version:$version, commit:$commit, url:$url, status:$status, notes:$notes, repo:$repo, duration:$duration}
   | with_entries(select(.value != "" and .value != 0))')

sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$DEPLOY_HOOK_SECRET" -hex | sed 's/^.* //')

curl -fsS -X POST "$url" \
  -H 'Content-Type: application/json' \
  -H "X-Signature: sha256=$sig" \
  --data "$body"
echo
