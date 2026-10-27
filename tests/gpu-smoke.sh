#!/usr/bin/env bash
set -euo pipefail
: "${ORDO_TEST_BINARY:?}" "${ORDO_TEST_ARCHIVE:?}" "${ORDO_TEST_IMAGE:?}"
host=${1:?}; gpu=${2:?}
[[ $host =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ && $gpu =~ ^GPU-[a-fA-F0-9-]{36}$ ]]
work=$(mktemp -d /tmp/ordo-gpu-evidence.XXXXXX)
ssh "$host" 'nvidia-smi --query-gpu=index,uuid,name,memory.total --format=csv,noheader,nounits; nvidia-smi --query-compute-apps=gpu_uuid,pid,used_memory --format=csv,noheader,nounits; nvidia-smi pmon -c 1' > "$work/availability.txt"
root=$(ssh "$host" 'umask 077; mktemp -d /tmp/ordo-gpu-test.XXXXXX')
passed=false
pid=
cleanup(){
 if [[ -n $pid ]]; then kill -TERM "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
 if $passed; then
  ssh "$host" "set -eu; test -z \"\$(ls -A '$root/runs')\"; if test -f '$root/image-introduced'; then podman image rm '$ORDO_TEST_IMAGE'; fi; rm -rf -- '$root'"
 else printf 'Retained %s:%s\n' "$host" "$root" >&2; fi
 printf 'Evidence: %s\n' "$work"
}
trap cleanup EXIT
ssh "$host" "if ! podman image exists '$ORDO_TEST_IMAGE'; then touch '$root/image-introduced'; fi"
ssh "$host" "nvidia-smi --id='$gpu' --query-gpu=name,driver_version --format=csv,noheader; grep -m1 '^model name' /proc/cpuinfo | cut -d: -f2-" > "$work/hardware.txt"
gpu_model=$(sed -n 1p "$work/hardware.txt" | cut -d, -f1 | xargs)
gpu_driver=$(sed -n 1p "$work/hardware.txt" | cut -d, -f2 | xargs)
cpu_model=$(sed -n 2p "$work/hardware.txt" | xargs)
jq -n --arg host "$host" --arg root "$root" --arg gpu "$gpu" '[{host:$host,state_dir:$root,cpus:"62,63,126,127",memory_mib:512,gpus:[$gpu]}]' > "$work/workers.json"
start() { "$ORDO_TEST_BINARY" server --state "$work/server" --workers "$work/workers.json" --listen 127.0.0.1:0 >> "$work/server.log" 2>&1 & pid=$!; for ((i=0;i<50;i++)); do if "$ORDO_TEST_BINARY" queue list --state "$work/server" > /dev/null 2>&1; then return; fi; sleep 0.2; done; return 1; }
submit() { jq -nc --arg image "$ORDO_TEST_IMAGE" --arg archive "$ORDO_TEST_ARCHIVE" "$1" | "$ORDO_TEST_BINARY" submit --state "$work/server" >> "$work/submitted.json"; }
job() { "$ORDO_TEST_BINARY" queue get --state "$work/server" --id "$1"; }
start
submit '{id:"mismatch",image:{archive:$archive,id:$image},resources:{cores:1,memory_mib:128},match:{cpu_model:"not-this-hardware"}}'
jq -nc --arg image "$ORDO_TEST_IMAGE" --arg archive "$ORDO_TEST_ARCHIVE" --arg cpu "$cpu_model" --arg model "$gpu_model" --arg driver "$gpu_driver" \
 '{id:"gpu",image:{archive:$archive,id:$image},resources:{cores:1,memory_mib:256,gpus:1},match:{arch:"amd64",cpu_model:$cpu,gpu_model:$model,gpu_driver:$driver,min_gpu_memory_mib:40000},timeout_seconds:30,args:["-c","timeout --kill-after=2 10 ordo-gpu-probe || exit $?; sleep 12"]}' |
 "$ORDO_TEST_BINARY" submit --state "$work/server" >> "$work/submitted.json"
for ((i=0;i<60;i++)); do
 job gpu > "$work/gpu.json"
 if jq -e '.state=="running" and .attempts[-1].container_id != null' "$work/gpu.json" >/dev/null; then break; fi
 sleep 1
done
jq -e --arg gpu "$gpu" '.state=="running" and .attempts[-1].allocation.gpus==[$gpu]' "$work/gpu.json" >/dev/null
submit '{id:"overlap",image:{archive:$archive,id:$image},resources:{cores:1,memory_mib:128,gpus:1}}'
kill -KILL "$pid"; wait "$pid" 2>/dev/null || true; pid=
start
sleep 3
job overlap > "$work/overlap.json"
jq -e '.state=="queued" and (.attempts//[]|length)==0' "$work/overlap.json" >/dev/null
for ((i=0;i<60;i++)); do
 job gpu > "$work/final.json"
 if jq -e '.state=="succeeded" or .state=="failed"' "$work/final.json" >/dev/null; then break; fi
 sleep 1
done
jq -e --arg gpu "$gpu" '.state=="succeeded" and (.attempts|length)==1 and .attempts[0].result.exit_code==0 and .attempts[0].allocation.gpus==[$gpu]' "$work/final.json" >/dev/null
job mismatch > "$work/mismatch.json"
jq -e '.state=="queued" and (.attempts//[]|length)==0' "$work/mismatch.json" >/dev/null
"$ORDO_TEST_BINARY" queue cancel --state "$work/server" --id mismatch > /dev/null
"$ORDO_TEST_BINARY" queue cancel --state "$work/server" --id overlap > /dev/null
for ((i=0;i<60;i++)); do
 job overlap > "$work/overlap.json"
 if jq -e '.state=="cancelled"' "$work/overlap.json" >/dev/null; then break; fi
 sleep 1
done
jq -e '.state=="cancelled"' "$work/overlap.json" >/dev/null
"$ORDO_TEST_BINARY" fetch --state "$work/server" --into "$work/results" --id gpu > "$work/fetch.txt"
grep -Fx "uuid=$gpu" "$work/results/gpu/stdout.log"
grep -Fx count=1 "$work/results/gpu/stdout.log"
grep -Fx value=42 "$work/results/gpu/stdout.log"
passed=true
printf 'PASS: one visible UUID, CUDA allocation/copy, exclusive reservation, hardware matching, server restart recovery\n'
