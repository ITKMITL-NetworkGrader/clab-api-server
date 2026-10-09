#!/usr/bin/env bash
# NTG-231 rollback to a clab-api-server built before NTG-231 (dev box only).
#
# A pre-NTG-231 containerlab opens <node>/nvram_<Index+1>; NTG-231 keeps the saved config in
# <node>/nvram. Rolling back without moving it back boots every IOL node without its config.
# Run the phases in order, inside a maintenance window:
#   0. Point the NTG-229 active host elsewhere (PUT /v0/admin/clab-host {"active":"maintenance"})
#      and wait 15 s, so Elysia sends nothing.
#   1. ./ntg231-rollback.sh capture-destroy   (as any user; CLAB_USERNAME must be the user Elysia
#      uses, ngclab, because the API puts a lab in the logged-in user's directory)
#   2. sudo ./ntg231-rollback.sh rename        (root: the lab directories belong to ngclab)
#   3. sudo /usr/local/sbin/ng-install-clab-api <previous build in dist/>
#   4. ./ntg231-rollback.sh redeploy          (each lab from its own file, one at a time,
#      stopped nodes stopped again)
#   5. Clear the active host (PUT /v0/admin/clab-host {"active":null}).
# Env: STATE_DIR (required), CLAB_API (default http://127.0.0.1:8090), CLAB_USERNAME,
#      CLAB_PASSWORD, LAB_FILTER (regex on lab names, default all).
set -euo pipefail
API=${CLAB_API:-http://127.0.0.1:8090}
DIR=${STATE_DIR:?set STATE_DIR}
FILTER=${LAB_FILTER:-.}
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$DIR"

login() {
  T=$(curl -fsS -X POST "$API/login" -H 'content-type: application/json' \
    -d "$(jq -n --arg u "${CLAB_USERNAME:?}" --arg p "${CLAB_PASSWORD:?}" '{username:$u,password:$p}')" | jq -r .token)
}
call() { # method path [outfile] -> prints HTTP status
  curl -sS -o "${3:-/dev/null}" -w '%{http_code}' -X "$1" "$API$2" -H "Authorization: Bearer $T"
}

capture_destroy() {
  login
  call GET /api/v1/labs "$DIR/labs.json" >/dev/null
  jq -r --arg u "$CLAB_USERNAME" 'to_entries[]
      | select(any(.value[]; .kind == "cisco_iol"))
      | select(all(.value[]; .owner == $u))
      | .key' "$DIR/labs.json" | grep -E "$FILTER" > "$DIR/labs.txt" || true
  jq -r --arg u "$CLAB_USERNAME" 'to_entries[] | select(any(.value[]; .kind == "cisco_iol"))
      | select(any(.value[]; .owner != $u)) | "skipped \(.key): owner is not \($u)"' "$DIR/labs.json"
  while read -r lab; do
    call GET "/api/v1/labs/$lab" "$DIR/$lab.inspect.json" >/dev/null
    path=$(jq -r '.[0].absLabPath' "$DIR/$lab.inspect.json")
    case "$path" in */.clab/"$lab"/*) ;; *) echo "skipped $lab: lab file $path is not under .clab/$lab"; continue ;; esac
    call GET "/api/v1/labs/$lab/topology/yaml" "$DIR/$lab.yml" >/dev/null
    for try in $(seq 1 30); do
      code=$(call DELETE "/api/v1/labs/$lab?cleanup=false" "$DIR/$lab.destroy.json")
      [ "$code" = 409 ] || break
      echo "$lab busy, waiting"; sleep 10
    done
    [ "$code" = 200 ] || { echo "destroy $lab failed ($code): $(cat "$DIR/$lab.destroy.json")"; exit 1; }
    echo "$lab destroyed (dir kept): $path"
    echo "$lab $path" >> "$DIR/done.txt"
  done < "$DIR/labs.txt"
}

rename() {
  [ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
  while read -r lab path; do
    if ! cmp -s "$path" "$DIR/$lab.yml"; then echo "$lab: $path differs from the captured topology; not renamed"; continue; fi
    echo "== $lab"; python3 "$HERE/ntg231-rename.py" "$path"
  done < "$DIR/done.txt"
}

redeploy() {
  login
  while read -r lab path; do
    code=$(call POST "/api/v1/labs/$lab/deploy?path=$(basename "$path")" "$DIR/$lab.deploy.json")
    [ "$code" = 200 ] || { echo "deploy $lab failed ($code): $(head -c 300 "$DIR/$lab.deploy.json")"; exit 1; }
    for node in $(jq -r '.[] | select(.state != "running") | .nodeName' "$DIR/$lab.inspect.json"); do
      call POST "/api/v1/labs/$lab/nodes/$node/stop" >/dev/null
    done
    call GET "/api/v1/labs/$lab" "$DIR/$lab.after.json" >/dev/null
    want=$(jq -c '[.[] | {nodeName, state}] | sort_by(.nodeName)' "$DIR/$lab.inspect.json")
    got=$(jq -c '[.[] | {nodeName, state}] | sort_by(.nodeName)' "$DIR/$lab.after.json")
    now=$(jq -r '.[0].absLabPath' "$DIR/$lab.after.json")
    if [ "$want" = "$got" ] && [ "$now" = "$path" ]; then echo "$lab back: $got"; else echo "$lab MISMATCH: want $want got $got path $now"; exit 1; fi
  done < "$DIR/done.txt"
}

case "${1:-}" in
  capture-destroy) capture_destroy ;;
  rename) rename ;;
  redeploy) redeploy ;;
  *) sed -n '2,20p' "$0"; exit 2 ;;
esac
