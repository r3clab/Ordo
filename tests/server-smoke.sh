#!/usr/bin/env bash
set -euo pipefail
: "${ORDO_TEST_BINARY:?}" "${ORDO_TEST_ARCHIVE:?}" "${ORDO_TEST_IMAGE:?}"
work=$(mktemp -d /tmp/ordo-server-evidence.XXXXXX)
root_a=$(ssh vegeta 'umask 077; mktemp -d /tmp/ordo-server-test.XXXXXX')
root_b=$(ssh goku 'umask 077; mktemp -d /tmp/ordo-server-test.XXXXXX')
passed=false
pid=
cleanup() {
  if [[ -n $pid ]]; then kill -TERM "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  for pair in "vegeta:$root_a" "goku:$root_b"; do
    host=${pair%%:*}; root=${pair#*:}; unit="ordo-agent-$(printf %s "$root" | sha256sum | cut -c1-16)"
    ssh "$host" "systemctl --user stop '$unit'" || { printf 'Retained %s\n' "$pair" >&2; continue; }
    rsync -a --exclude=bin "$host:$root/" "$work/$host/"
    if $passed; then
      ssh "$host" "bash -s -- '$root' '$ORDO_TEST_IMAGE'" <<'EOF'
set -euo pipefail
[[ $1 =~ ^/tmp/ordo-server-test\.[a-zA-Z0-9]+$ ]]
for record in "$1"/runs/*/run.json; do
 jq -e '.container_removed' "$record" >/dev/null
 status=0; podman container exists "$(jq -r .container_id "$record")" || status=$?
 test "$status" = 1
done
if test -f "$1/image-introduced"; then podman image rm "$2"; fi
rm -rf -- "$1"
EOF
    else printf 'Retained %s\n' "$pair" >&2; fi
  done
  printf 'Evidence: %s\n' "$work"
}
trap cleanup EXIT
for pair in "vegeta:$root_a" "goku:$root_b"; do host=${pair%%:*}; root=${pair#*:}; ssh "$host" "if ! podman image exists '$ORDO_TEST_IMAGE'; then touch '$root/image-introduced'; fi"; done
jq -n --arg a "$root_a" --arg b "$root_b" --arg binary "$ORDO_TEST_BINARY" '[{host:"vegeta",state_dir:$a,cpus:"62,63,126,127",memory_mib:512,binary:$binary},{host:"goku",state_dir:$b,cpus:"62,63,126,127",memory_mib:512,binary:$binary}]' > "$work/workers.json"
start() { "$ORDO_TEST_BINARY" server --state "$work/server" --workers "$work/workers.json" >> "$work/server.log" 2>&1 & pid=$!; for ((i=0;i<50;i++)); do if "$ORDO_TEST_BINARY" queue list --state "$work/server" > "$work/list.json" 2>/dev/null; then return; fi; sleep 0.2; done; return 1; }
start
for id in first second; do
 jq -n --arg id "$id" --arg image "$ORDO_TEST_IMAGE" --arg archive "$ORDO_TEST_ARCHIVE" '{id:$id,image:$image,archive:$archive,resources:{cores:2,memory_mib:256},args:["-c","printf start; sleep 30; printf done > /outputs/result"]}' > "$work/$id.json"
 "$ORDO_TEST_BINARY" queue submit --state "$work/server" --file "$work/$id.json" > "$work/$id-submitted.json"
done
for ((i=0;i<90;i++)); do
 "$ORDO_TEST_BINARY" queue list --state "$work/server" > "$work/list.json"
 if jq -e 'length==2 and all(.state=="running") and ([.[].attempts[-1].worker]|unique|length)==2' "$work/list.json" >/dev/null; then break; fi
 sleep 1
done
jq -e 'all(.state=="running")' "$work/list.json" >/dev/null
pgrep -P "$pid" > "$work/tunnel-pids.txt" || true
kill -KILL "$pid"; wait "$pid" 2>/dev/null || true; pid=
sleep 3
while IFS= read -r child; do if kill -0 "$child" 2>/dev/null; then printf 'Orphan transport %s\n' "$child" >&2; exit 1; fi; done < "$work/tunnel-pids.txt"
start
for ((i=0;i<90;i++)); do
 "$ORDO_TEST_BINARY" queue list --state "$work/server" > "$work/final.json"
 if jq -e 'all(.state=="succeeded")' "$work/final.json" >/dev/null; then break; fi
 sleep 1
done
jq -e 'length==2 and all(.state=="succeeded" and (.attempts|length)==1)' "$work/final.json" >/dev/null
while IFS= read -r result; do test "$(<"$result/outputs/result")" = done; done < <(jq -r '.[].attempts[-1].result_dir' "$work/final.json")
passed=true
printf 'PASS: two-worker queue, server SIGKILL, tunnel cleanup, same attempts, collected artifacts\n'
