#!/usr/bin/env bash
#
# record-fixture.sh NAME records a real Admin API response from the dev store
# as a test fixture.
#
# It runs the GraphQL document in cmd/storewarden/testdata/NAME.graphql (with
# variables from NAME.variables.json, if that file exists) and writes the
# response to cmd/storewarden/testdata/NAME.json, next to the query that made
# it. Credentials come from .env and never appear in arguments or output.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
name="${1:?usage: scripts/record-fixture.sh NAME}"
dir=cmd/storewarden/testdata
query="$dir/$name.graphql"
vars="$dir/$name.variables.json"
out="$dir/$name.json"
[[ -f "$query" ]] || { echo "no query at $query" >&2; exit 1; }

# A worktree has no .env of its own, so use the main checkout's.
env_file=.env
if [[ ! -f "$env_file" ]]; then
  env_file="$(dirname "$(git rev-parse --path-format=absolute --git-common-dir)")/.env"
fi
set -a
# shellcheck disable=SC1090
. "$env_file"
set +a
: "${SHOPIFY_STORE:?SHOPIFY_STORE missing from .env}"
: "${SHOPIFY_CLIENT_ID:?SHOPIFY_CLIENT_ID missing from .env}"
: "${SHOPIFY_CLIENT_SECRET:?SHOPIFY_CLIENT_SECRET missing from .env}"
api_version=$(sed -n 's/^const APIVersion = "\(.*\)"$/\1/p' internal/shopify/client.go)

# The secret goes to curl on stdin, so it never shows in the process list.
token=$(printf '%s' "$SHOPIFY_CLIENT_SECRET" | curl -sS --fail-with-body --max-time 30 \
  -X POST "https://$SHOPIFY_STORE/admin/oauth/access_token" \
  --data-urlencode grant_type=client_credentials \
  --data-urlencode "client_id=$SHOPIFY_CLIENT_ID" \
  --data-urlencode client_secret@- |
  python3 -c 'import json, sys; print(json.load(sys.stdin)["access_token"])')

body=$(python3 -c '
import json, os, sys
query = open(sys.argv[1]).read()
variables = json.load(open(sys.argv[2])) if os.path.exists(sys.argv[2]) else {}
print(json.dumps({"query": query, "variables": variables}))
' "$query" "$vars")

# The token goes to curl as a header file on stdin, for the same reason.
printf 'X-Shopify-Access-Token: %s\n' "$token" | curl -sS --fail-with-body --max-time 30 \
  -X POST "https://$SHOPIFY_STORE/admin/api/$api_version/graphql.json" \
  -H @- -H 'Content-Type: application/json' --data-binary "$body" |
  python3 -m json.tool --indent 2 > "$out"

echo "recorded $out"
