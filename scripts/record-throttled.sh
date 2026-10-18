#!/usr/bin/env bash
#
# record-throttled.sh records a real THROTTLED response from the dev store as
# cmd/storewarden/testdata/throttled.json.
#
# Shopify refunds the unused part of a query's requested cost as soon as the
# query runs, and the dev store holds little data, so large queries barely
# drain the cost bucket. throttled.graphql asks for 990 aliases of
# shop { id }, which costs the full 990 whatever the Store holds. The script
# sends dozens of copies of it at once through record-fixture.sh, records
# throttled itself once enough of them have answered, and tries again until
# Shopify throttles it.
#
# TestAThrottledResponseIsRetriedOnceAfterTheWait in internal/shopify uses the
# recording's figures; update them after a new recording.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
dir=cmd/storewarden/testdata
copies=60   # sent at once; the dev store's bucket holds about 20
answered=20 # copies that must answer before throttled is sent

cleanup() { rm -f "$dir"/drain_*; }
trap cleanup EXIT

# report prints the recording's error codes and cost, and fails unless
# Shopify throttled it.
report() {
  python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
codes = [e.get("extensions", {}).get("code") for e in d.get("errors", [])]
print("errors:", codes or "none", "cost:", json.dumps(d["extensions"]["cost"]))
sys.exit(0 if "THROTTLED" in codes else 1)
' "$dir/throttled.json"
}

for attempt in 1 2 3; do
  for i in $(seq -w 1 "$copies"); do
    cp "$dir/throttled.graphql" "$dir/drain_$i.graphql"
    scripts/record-fixture.sh "drain_$i" >/dev/null 2>&1 &
  done
  # record-fixture.sh writes its output only once the response is in.
  start=$SECONDS
  until (($(find "$dir" -name 'drain_*.json' -size +0 | wc -l) >= answered)) || ((SECONDS - start > 30)); do
    sleep 0.05
  done
  scripts/record-fixture.sh throttled
  wait
  cleanup
  if report; then
    exit 0
  fi
  echo "attempt $attempt: not throttled; waiting for the bucket to refill" >&2
  sleep 25
done

git checkout -- "$dir/throttled.json"
echo "Shopify didn't throttle in 3 attempts; throttled.json is unchanged" >&2
exit 1
