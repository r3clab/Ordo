#!/usr/bin/env bash
set -euo pipefail
: "${ORDO_TEST_BINARY:?}" "${ORDO_TEST_INSTALLABLE:?}" "${ORDO_TEST_IMAGE:?}"
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
jq -n --arg host "$host" --arg root "$root" --arg gpu "$gpu" '{listen:"127.0.0.1:0",workers:[{host:$host,state_dir:$root,cpus:"62,63,126,127",memory_mib:512,gpus:[$gpu],tags:["gpu-smoke"]}]}' > "$work/server.json"
start() { "$ORDO_TEST_BINARY" server --state "$work/server" --config "$work/server.json" >> "$work/server.log" 2>&1 & pid=$!; for ((i=0;i<50;i++)); do if "$ORDO_TEST_BINARY" queue list --state "$work/server" > /dev/null 2>&1; then return; fi; sleep 0.2; done; return 1; }
submit() { jq -nc --arg installable "$ORDO_TEST_INSTALLABLE" --arg out "$work/out" "$1" | "$ORDO_TEST_BINARY" submit --state "$work/server" >> "$work/submitted.json"; }
job() { "$ORDO_TEST_BINARY" queue get --state "$work/server" --out "$work/out/$1"; }
start
if submit '{out:"\($out)/mismatch",image:$installable,resources:{cores:1,memory_mib:128},tags:["absent"]}' 2> "$work/mismatch.txt"; then echo "accepted a tag no worker carries" >&2; exit 1; fi
jq -se --arg out "$work/out/mismatch" 'any(.[]; .out==$out and (.error|contains("no configured worker has all tags")))' "$work/submitted.json" >/dev/null
submit '{out:"\($out)/gpu",image:$installable,resources:{cores:1,memory_mib:256,gpus:1},tags:["gpu-smoke"],timeout_seconds:30,args:["-c","timeout --kill-after=2 10 ordo-gpu-probe || exit $?; sleep 12"]}'
if ! job gpu | jq -e --arg image "$ORDO_TEST_IMAGE" '.steps[0].spec.image==$image' >/dev/null; then echo "ORDO_TEST_IMAGE does not match the image built from ORDO_TEST_INSTALLABLE" >&2; exit 1; fi
for ((i=0;i<60;i++)); do
 job gpu > "$work/gpu.json"
 if jq -e '.state=="running" and .attempts[-1].container_id != null' "$work/gpu.json" >/dev/null; then break; fi
 sleep 1
done
jq -e --arg gpu "$gpu" '.state=="running" and .attempts[-1].allocation.gpus==[$gpu]' "$work/gpu.json" >/dev/null
submit '{out:"\($out)/overlap",image:$installable,resources:{cores:1,memory_mib:128,gpus:1}}'
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
"$ORDO_TEST_BINARY" queue cancel --state "$work/server" --out "$work/out/overlap" > /dev/null
for ((i=0;i<60;i++)); do
 job overlap > "$work/overlap.json"
 if jq -e '.state=="cancelled"' "$work/overlap.json" >/dev/null; then break; fi
 sleep 1
done
jq -e '.state=="cancelled"' "$work/overlap.json" >/dev/null
grep -Fx "uuid=$gpu" "$work/out/gpu/.ordo/1.stdout"
grep -Fx count=1 "$work/out/gpu/.ordo/1.stdout"
grep -Fx value=42 "$work/out/gpu/.ordo/1.stdout"
passed=true
printf 'PASS: one visible UUID, CUDA allocation/copy, exclusive reservation, tag matching, server restart recovery\n'
