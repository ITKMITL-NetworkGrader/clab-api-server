#!/usr/bin/env bash
# NTG-233: from v0.5.5-ntg233 every IOL node binds <node>/vlan.dat (its VLANs and VTP state).
# A rollback needs nothing: older builds ignore the file. But while an older build runs, a node it
# recreates keeps its VLANs in the container only, and the host file goes stale. Re-installing the
# NTG-233 build would mount that stale file and bring back VLANs deleted since.
#
# So, before re-installing the NTG-233 build after a rollback (while the older build is still
# installed, so no new vlan.dat mounts appear), delete only the vlan.dat files that no container,
# running or stopped, mounts. A mounted file is what its node has been writing all along.
#   sudo ./ntg233-vlandat-sweep.sh            list them
#   sudo ./ntg233-vlandat-sweep.sh --delete   delete them
# CLAB_ROOT defaults to /home/ngclab/.clab. Any docker error stops it before anything is deleted.
set -euo pipefail
root=$(realpath -e "${CLAB_ROOT:-/home/ngclab/.clab}")
declare -A mounted=()
ids=$(docker ps -aq) # a plain assignment, so set -e stops the script when docker fails
for id in $ids; do
  srcs=$(docker inspect -f '{{range .Mounts}}{{println .Source}}{{end}}' "$id")
  while IFS= read -r src; do
    if [[ $src == */vlan.dat ]]; then mounted[$(realpath -m -- "$src")]=1; fi
  done <<< "$srcs"
done
# A find error can only skip files, never delete a mounted one.
while IFS= read -r -d '' f; do
  f=$(realpath -e -- "$f")
  if [[ -n ${mounted[$f]:-} ]]; then continue; fi
  echo "stale: $f"
  if [[ ${1:-} == --delete ]]; then rm -- "$f"; fi
done < <(find -L "$root" -path '*/clab-*/*/vlan.dat' -type f -print0)
