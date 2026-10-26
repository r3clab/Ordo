#!/usr/bin/env bash
set -euo pipefail
: "${ORDO_TEST_BINARY:?}" "${ORDO_TEST_ARCHIVE:?}" "${ORDO_TEST_IMAGE:?}"
host=${1:?}; gpu=${2:?}
[[ $host =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ && $gpu =~ ^GPU-[a-fA-F0-9-]{36}$ ]]
work=$(mktemp -d /tmp/ordo-gpu-evidence.XXXXXX)
ssh "$host" 'nvidia-smi --query-gpu=index,uuid,name,memory.total --format=csv,noheader,nounits; nvidia-smi --query-compute-apps=gpu_uuid,pid,used_memory --format=csv,noheader,nounits; nvidia-smi pmon -c 1' > "$work/availability.txt"
root=$(ssh "$host" 'umask 077; mktemp -d /tmp/ordo-gpu-test.XXXXXX')
unit="ordo-agent-$(printf %s "$root" | sha256sum | cut -c1-16)"
passed=false
cleanup(){
 ssh "$host" "systemctl --user stop '$unit'" || return 1
 rsync -a --exclude=bin --exclude=blobs "$host:$root/" "$work/remote/"
 if $passed; then
  ssh "$host" "set -eu; test ! -s /run/user/\$(id -u)/ordo-worker.lock; if test -f '$root/image-introduced'; then podman image rm '$ORDO_TEST_IMAGE'; fi; rm -rf -- '$root'"
 else printf 'Retained %s:%s\n' "$host" "$root" >&2; fi
 printf 'Evidence: %s\n' "$work"
}
trap cleanup EXIT
base=(--host "$host" --state "$root" --cpus 62,63,126,127 --memory-mib 512 --gpus "$gpu")
ssh "$host" "if ! podman image exists '$ORDO_TEST_IMAGE'; then touch '$root/image-introduced'; fi"
"$ORDO_TEST_BINARY" worker start "${base[@]}" --binary "$ORDO_TEST_BINARY" > "$work/start.json"
"$ORDO_TEST_BINARY" worker put-image "${base[@]}" --archive "$ORDO_TEST_ARCHIVE" --image "$ORDO_TEST_IMAGE" > "$work/image.json"
"$ORDO_TEST_BINARY" worker submit "${base[@]}" --id gpu --image "$ORDO_TEST_IMAGE" --cores 1 --task-memory-mib 256 --task-gpus 1 --timeout-seconds 30 -- -c 'timeout --kill-after=2 10 ordo-gpu-probe || exit $?; sleep 12' > "$work/submitted.json"
status=0
"$ORDO_TEST_BINARY" worker submit "${base[@]}" --id overlap --image "$ORDO_TEST_IMAGE" --cores 1 --task-memory-mib 128 --task-gpus 1 > "$work/overlap.stdout" 2> "$work/overlap.stderr" || status=$?
test "$status" = 2
grep -F 'insufficient unreserved resources' "$work/overlap.stderr"
for ((i=0;i<30;i++)); do
 if ssh "$host" "jq -e '.container_id != null' '$root/runs/gpu/run.json'" >/dev/null 2>&1; then break; fi
 sleep 0.2
done
ssh "$host" "jq -e '.container_id != null' '$root/runs/gpu/run.json'" >/dev/null
ssh "$host" "pid=\$(systemctl --user show '$unit' -p MainPID --value); test \"\$pid\" -gt 1; kill -KILL \"\$pid\""
"$ORDO_TEST_BINARY" worker info "${base[@]}" > "$work/restored.json"
jq -e --arg gpu "$gpu" '(.held|length)==1 and .held[0].gpus==[$gpu]' "$work/restored.json" >/dev/null
for ((i=0;i<60;i++)); do
 "$ORDO_TEST_BINARY" worker get "${base[@]}" --id gpu > "$work/final.json"
 if jq -e '.state=="finished"' "$work/final.json" >/dev/null; then break; fi
 sleep 1
done
jq -e --arg gpu "$gpu" '.state=="finished" and .result.exit_code==0 and .result.container_removed and .allocation.gpus==[$gpu]' "$work/final.json" >/dev/null
"$ORDO_TEST_BINARY" worker fetch "${base[@]}" --id gpu --output "$work/result" > "$work/fetch.json"
grep -Fx "uuid=$gpu" "$work/result/stdout.log"
grep -Fx count=1 "$work/result/stdout.log"
grep -Fx value=42 "$work/result/stdout.log"
passed=true
printf 'PASS: one visible UUID, CUDA allocation/copy, exclusive reservation, restart recovery\n'
