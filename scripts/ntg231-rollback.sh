#!/usr/bin/env bash
# NTG-231 rollback to a clab-api-server built before NTG-231.
#
# A pre-NTG-231 containerlab opens <node>/nvram_<Index+1>; NTG-231 keeps the saved config in
# <node>/nvram. Rolling back without moving it back boots every IOL node without its config.
# Run the phases in order, in a maintenance window:
#   0. Stop whatever drives the API from sending lab operations, and let it settle.
#   1. ./ntg231-rollback.sh capture-destroy   CLAB_USERNAME must be the user that owns the labs:
#      the API puts a lab in the logged-in user's directory.
#   2. sudo -E ./ntg231-rollback.sh rename     every lab under CLAB_ROOT, running or not
#   3. install the previous build
#   4. ./ntg231-rollback.sh redeploy          the captured labs, one at a time, stopped nodes
#      stopped again
#   5. let the controller send operations again
# Env: STATE_DIR (required), CLAB_API (default http://127.0.0.1:8090), CLAB_USERNAME,
#      CLAB_PASSWORD, CLAB_ROOT (default ~CLAB_USERNAME/.clab), LAB_FILTER (regex, default all).
set -euo pipefail
API=${CLAB_API:-http://127.0.0.1:8090}
DIR=${STATE_DIR:?set STATE_DIR}
FILTER=${LAB_FILTER:-.}
ROOT=${CLAB_ROOT:-$(getent passwd "${CLAB_USERNAME:?}" | cut -d: -f6)/.clab}
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$DIR"

login() {
  T=$(curl -fsS -X POST "$API/login" -H 'content-type: application/json' \
    -d "$(jq -n --arg u "$CLAB_USERNAME" --arg p "${CLAB_PASSWORD:?}" '{username:$u,password:$p}')" | jq -r .token)
}
call() { # method path [outfile] -> prints the HTTP status
  curl -sS -o "${3:-/dev/null}" -w '%{http_code}' -X "$1" "$API$2" -H "Authorization: Bearer $T"
}
fail() { echo "$*" >&2; exit 1; }

capture_destroy() {
  login
  [ "$(call GET /api/v1/labs "$DIR/labs.json")" = 200 ] || fail "list labs failed"
  jq -r --arg u "$CLAB_USERNAME" 'to_entries[] | select(any(.value[]; .kind == "cisco_iol"))
      | select(all(.value[]; .owner == $u)) | .key' "$DIR/labs.json" | grep -E "$FILTER" > "$DIR/labs.txt" || true
  jq -r --arg u "$CLAB_USERNAME" 'to_entries[] | select(any(.value[]; .kind == "cisco_iol"))
      | select(any(.value[]; .owner != $u)) | "skipped \(.key): not owned by \($u)"' "$DIR/labs.json"
  while read -r lab; do
    [ "$(call GET "/api/v1/labs/$lab" "$DIR/$lab.inspect.json")" = 200 ] || fail "inspect $lab failed"
    path=$(jq -r '.[0].absLabPath' "$DIR/$lab.inspect.json")
    [ "$path" = "$ROOT/$lab/$lab.clab.yml" ] || { echo "skipped $lab: lab file $path is not $ROOT/$lab/$lab.clab.yml"; continue; }
    [ "$(call GET "/api/v1/labs/$lab/topology/yaml" "$DIR/$lab.yml")" = 200 ] || fail "topology of $lab failed"
    for try in $(seq 1 30); do
      code=$(call DELETE "/api/v1/labs/$lab?cleanup=false" "$DIR/$lab.destroy.json")
      [ "$code" = 409 ] || break
      echo "$lab busy, waiting"; sleep 10
    done
    [ "$code" = 200 ] || fail "destroy $lab failed ($code): $(cat "$DIR/$lab.destroy.json")"
    echo "$lab destroyed, directory kept"
    echo "$lab" >> "$DIR/done.txt"
  done < "$DIR/labs.txt"
}

rename() {
  [ "$(id -u)" = 0 ] || fail "run as root"
  touch "$DIR/done.txt"
  while read -r lab; do # labs we destroyed: the file on disk must be what was running
    cmp -s "$ROOT/$lab/$lab.clab.yml" "$DIR/$lab.yml" || fail "$lab: the file on disk differs from the captured topology"
  done < "$DIR/done.txt"
  for topo in "$ROOT"/*/*.clab.yml; do # every lab, also those with no containers now
    lab=$(basename "$(dirname "$topo")")
    [ "$topo" = "$ROOT/$lab/$lab.clab.yml" ] || continue
    echo "$lab" | grep -qE "$FILTER" || continue
    if [ -n "$(docker ps -q --filter "label=containerlab=$lab")" ]; then echo "skipped $lab: still has containers"; continue; fi
    python3 "$HERE/ntg231-rename.py" "$topo"
  done
}

redeploy() {
  login
  while read -r lab; do
    code=$(call POST "/api/v1/labs/$lab/deploy" "$DIR/$lab.deploy.json")
    [ "$code" = 200 ] || fail "deploy $lab failed ($code): $(head -c 300 "$DIR/$lab.deploy.json")"
    for node in $(jq -r '.[] | select(.state != "running") | .nodeName' "$DIR/$lab.inspect.json"); do
      call POST "/api/v1/labs/$lab/nodes/$node/stop" >/dev/null
    done
    call GET "/api/v1/labs/$lab" "$DIR/$lab.after.json" >/dev/null
    want=$(jq -c '[.[] | {nodeName, state}] | sort_by(.nodeName)' "$DIR/$lab.inspect.json")
    got=$(jq -c '[.[] | {nodeName, state}] | sort_by(.nodeName)' "$DIR/$lab.after.json")
    now=$(jq -r '.[0].absLabPath' "$DIR/$lab.after.json")
    [ "$want" = "$got" ] && [ "$now" = "$ROOT/$lab/$lab.clab.yml" ] || fail "$lab MISMATCH: want $want got $got path $now"
    echo "$lab back: $got"
  done < "$DIR/done.txt"
}

case "${1:-}" in
  capture-destroy) capture_destroy ;;
  rename) rename ;;
  redeploy) redeploy ;;
  *) sed -n '2,19p' "$0"; exit 2 ;;
esac
