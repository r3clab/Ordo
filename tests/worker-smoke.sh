#!/usr/bin/env bash
# Bounded, opt-in worker lifecycle test. No GPU work or host configuration changes.
set -euo pipefail
: "${ORDO_TEST_BINARY:?static Linux ordo binary required}"
: "${ORDO_TEST_ARCHIVE:?nix2container OCI archive required}"
: "${ORDO_TEST_IMAGE:?immutable sha256 image ID required}"
host=${1:?SSH alias required}
[[ $host =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]]
[[ $ORDO_TEST_IMAGE =~ ^sha256:[a-f0-9]{64}$ ]]
root=$(ssh -o BatchMode=yes "$host" 'umask 077; mktemp -d /tmp/ordo-worker-test.XXXXXX')
[[ $root =~ ^/tmp/ordo-worker-test\.[a-zA-Z0-9]+$ ]]
evidence=$(mktemp -d /tmp/ordo-worker-evidence.XXXXXX)
unit="ordo-agent-$(printf %s "$root" | sha256sum | cut -c1-16)"
passed=false
cleanup() {
  if ! ssh -o BatchMode=yes "$host" "systemctl --user stop '$unit'" > "$evidence/stop.log" 2>&1; then
    if $passed; then printf 'Cleanup failed: service may still be running; retained %s:%s\n' "$host" "$root" >&2; return 1; fi
  fi
  rsync -a --exclude=bin --exclude=image.tar "$host:$root/" "$evidence/remote/" || return 1
  if $passed; then
    ssh -o BatchMode=yes "$host" "bash -s -- '$root' '$ORDO_TEST_IMAGE'" <<'EOF'
set -euo pipefail
root=$1; image=$2
[[ $root =~ ^/tmp/ordo-worker-test\.[a-zA-Z0-9]+$ ]]
for record in "$root"/runs/*/run.json; do
  jq -e '.container_removed == true' "$record" >/dev/null
  cid=$(jq -r .container_id "$record")
  status=0; podman container exists "$cid" || status=$?
  test "$status" = 1
done
if test -f "$root/image-introduced"; then podman image rm "$image"; fi
rm -rf -- "$root"
EOF
  else
    printf 'FAILED: retained worker state at %s:%s; bounded test containers may still be running\n' "$host" "$root" >&2
  fi
  printf 'Evidence: %s\n' "$evidence"
}
trap cleanup EXIT
printf 'HOST=%s STATE=%s UNIT=%s\n' "$host" "$root" "$unit"
sha256sum "$ORDO_TEST_BINARY" > "$evidence/binary.sha256"
digest=$(cut -d ' ' -f 1 "$evidence/binary.sha256")
expect_other_agent_rejected() {
  ssh -o BatchMode=yes "$host" "timeout 5 '$root/bin/$digest' agent --state '$root/other' --cpus 62,63,126,127 --memory-mib 512; status=\$?; test \"\$status\" = 2" > "$evidence/other-$1.stdout" 2> "$evidence/other-$1.stderr"
  grep -E 'already has an active agent|is bound to' "$evidence/other-$1.stderr"
}
scp -q "$ORDO_TEST_ARCHIVE" "$host:$root/image.tar"
ssh -o BatchMode=yes "$host" "if ! podman image exists '$ORDO_TEST_IMAGE'; then podman load --input '$root/image.tar' && touch '$root/image-introduced'; fi"
base=(--host "$host" --state "$root" --cpus 62,63,126,127 --memory-mib 512)
"$ORDO_TEST_BINARY" worker start "${base[@]}" --binary "$ORDO_TEST_BINARY" > "$evidence/start.json"
jq -e '.protocol == 1 and .hardware.memory_mib == 512 and (.hardware.cores|length) == 2 and ((.error // "") == "")' "$evidence/start.json" >/dev/null
worker_id=$(jq -r .id "$evidence/start.json")
expect_other_agent_rejected active
scopes() { ssh -o BatchMode=yes "$host" "systemctl --user list-units --type=scope --plain --no-legend 'ordo-runtime-*'" | awk '{print $1}' | sort; }
scopes > "$evidence/scopes-before.txt"
ssh -o BatchMode=yes "$host" "test \"\$(systemctl --user show '$unit' -p KillMode --value)\" = control-group"
payload='printf "start\n"; printf "cpuset=%s\n" "$(</sys/fs/cgroup/cpuset.cpus.effective)"; printf "memory=%s\n" "$(</sys/fs/cgroup/memory.max)"; printf "swap=%s\n" "$(</sys/fs/cgroup/memory.swap.max)"; sleep 60; printf "end\n"; printf "stderr\n" >&2; exit 7'
submit=("${base[@]}" --id survive --image "$ORDO_TEST_IMAGE" --cores 2 --task-memory-mib 256 -- -c "$payload")
"$ORDO_TEST_BINARY" worker submit "${submit[@]}" > "$evidence/submit.json"
"$ORDO_TEST_BINARY" worker submit "${submit[@]}" > "$evidence/repeat.json"
test "$(jq -r .created_at "$evidence/submit.json")" = "$(jq -r .created_at "$evidence/repeat.json")"
expect_full() {
  status=0
  "$ORDO_TEST_BINARY" worker submit "${base[@]}" --id overlap --image "$ORDO_TEST_IMAGE" --cores 1 --task-memory-mib 128 > "$evidence/overlap.stdout" 2> "$evidence/overlap.stderr" || status=$?
  test "$status" = 2
  grep -F 'insufficient unreserved resources' "$evidence/overlap.stderr"
}
expect_full
for ((i=0; i<20; i++)); do
  cid=$(ssh -o BatchMode=yes "$host" "jq -r '.container_id // empty' '$root/runs/survive/run.json' 2>/dev/null" || true)
  [[ $cid =~ ^[a-f0-9]{64}$ ]] && break
  sleep 0.2
done
[[ $cid =~ ^[a-f0-9]{64}$ ]]
ssh -o BatchMode=yes "$host" "bash -s -- '$cid'" > "$evidence/monitor-cgroup.txt" <<'EOF'
set -euo pipefail
pidfile=$(podman inspect --format '{{.ConmonPidFile}}' "$1")
monitor=$(<"$pidfile")
grep -E '/(ordo-runtime-[a-z0-9]+|libpod-conmon-[a-f0-9]+)\.scope(/|$)' "/proc/$monitor/cgroup"
EOF
scopes > "$evidence/scopes-running.txt"
comm -13 "$evidence/scopes-before.txt" "$evidence/scopes-running.txt" > "$evidence/test-scopes.txt"
test -s "$evidence/test-scopes.txt"
ssh -o BatchMode=yes "$host" "test \"\$(podman inspect --format '{{.State.Running}}' '$cid')\" = true; pid=\$(systemctl --user show '$unit' -p MainPID --value); test \"\$pid\" -gt 1; kill -KILL \"\$pid\""
"$ORDO_TEST_BINARY" worker info "${base[@]}" > "$evidence/after-crash.json"
test "$(jq -r .id "$evidence/after-crash.json")" = "$worker_id"
jq -e '(.held|length) == 1' "$evidence/after-crash.json" >/dev/null
expect_full
ssh -o BatchMode=yes "$host" "systemctl --user stop '$unit'; test \"\$(podman inspect --format '{{.State.Running}}' '$cid')\" = true"
expect_other_agent_rejected stopped
"$ORDO_TEST_BINARY" worker start "${base[@]}" --binary "$ORDO_TEST_BINARY" > "$evidence/after-stop.json"
test "$(jq -r .id "$evidence/after-stop.json")" = "$worker_id"
expect_full
for ((i=0; i<90; i++)); do
  "$ORDO_TEST_BINARY" worker get "${base[@]}" --id survive > "$evidence/final.json"
  state=$(jq -r .state "$evidence/final.json")
  [[ $state == finished ]] && break
  [[ $state == infrastructure_failed ]] && { printf 'Unexpected infrastructure failure\n' >&2; exit 1; }
  sleep 1
done
jq -e --arg id "$cid" '.state == "finished" and .result.exit_code == 7 and .result.container_id == $id and .result.container_removed and .result.allocation_mode == "agent"' "$evidence/final.json" >/dev/null
ssh -o BatchMode=yes "$host" "grep -Fx cpuset=62-63,126-127 '$root/runs/survive/stdout.log'; grep -Fx memory=268435456 '$root/runs/survive/stdout.log'; grep -Fx swap=0 '$root/runs/survive/stdout.log'; test \"\$(grep -c '^start$' '$root/runs/survive/stdout.log')\" = 1; test \"\$(grep -c '^end$' '$root/runs/survive/stdout.log')\" = 1; grep -Fx stderr '$root/runs/survive/stderr.log'"
# Leave one physical core free: rejection here must be due to aggregate memory.
"$ORDO_TEST_BINARY" worker submit "${base[@]}" --id memory --image "$ORDO_TEST_IMAGE" --cores 1 --task-memory-mib 384 -- -c 'sleep 8' > "$evidence/memory-submit.json"
status=0
"$ORDO_TEST_BINARY" worker submit "${base[@]}" --id memory-overlap --image "$ORDO_TEST_IMAGE" --cores 1 --task-memory-mib 256 > "$evidence/memory-overlap.stdout" 2> "$evidence/memory-overlap.stderr" || status=$?
test "$status" = 2
grep -F 'insufficient unreserved resources' "$evidence/memory-overlap.stderr"
for ((i=0; i<30; i++)); do
  "$ORDO_TEST_BINARY" worker get "${base[@]}" --id memory > "$evidence/memory-final.json"
  if jq -e '.state == "finished"' "$evidence/memory-final.json" >/dev/null; then break; fi
  sleep 1
done
jq -e '.state == "finished" and .result.exit_code == 0 and .result.container_removed' "$evidence/memory-final.json" >/dev/null
"$ORDO_TEST_BINARY" worker info "${base[@]}" > "$evidence/released.json"
jq -e '(.held // [] | length) == 0' "$evidence/released.json" >/dev/null
for ((i=0; i<10; i++)); do
  scopes > "$evidence/scopes-after.txt"
  comm -13 "$evidence/scopes-before.txt" "$evidence/scopes-after.txt" > "$evidence/leftover-scopes.txt"
  if ! test -s "$evidence/leftover-scopes.txt"; then break; fi
  sleep 0.2
done
test ! -s "$evidence/leftover-scopes.txt"
passed=true
printf 'PASS: idempotent dispatch, full-core allocation, reconnect, agent crash/restart, graceful service stop, durable reservation, original execution/result, resource release\n'
