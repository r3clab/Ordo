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
  "$ORDO_TEST_BINARY" queue list --state "$work/server" > "$work/cleanup.json" 2>/dev/null || true
  for pair in "vegeta:$root_a" "goku:$root_b"; do
    host=${pair%%:*}; root=${pair#*:}
    if $passed; then
      attempts=$(jq -r --arg host "$host" '[.[].attempts[]? | select(.worker==$host) | .id] | join(" ")' "$work/cleanup.json")
      ssh "$host" "bash -s -- '$root' '$ORDO_TEST_IMAGE' $attempts" <<'EOF'
set -euo pipefail
root=$1 image=$2; shift 2
[[ $root =~ ^/tmp/ordo-server-test\.[a-zA-Z0-9]+$ ]]
test -z "$(ls -A "$root/runs")"
for attempt; do
 status=0; podman container exists "ordo-$attempt" || status=$?
 test "$status" = 1
done
if test -f "$root/image-introduced"; then podman image rm "$image"; fi
rm -rf -- "$root"
EOF
    else printf 'Retained %s\n' "$pair" >&2; fi
  done
  printf 'Evidence: %s\n' "$work"
}
trap cleanup EXIT
for pair in "vegeta:$root_a" "goku:$root_b"; do host=${pair%%:*}; root=${pair#*:}; ssh "$host" "if ! podman image exists '$ORDO_TEST_IMAGE'; then touch '$root/image-introduced'; fi"; done
jq -n --arg a "$root_a" --arg b "$root_b" '[{host:"vegeta",state_dir:$a,cpus:"62,63,126,127",memory_mib:512},{host:"goku",state_dir:$b,cpus:"62,63,126,127",memory_mib:512}]' > "$work/workers.json"
start() { "$ORDO_TEST_BINARY" server --state "$work/server" --workers "$work/workers.json" --listen 127.0.0.1:0 >> "$work/server.log" 2>&1 & pid=$!; for ((i=0;i<50;i++)); do if "$ORDO_TEST_BINARY" queue list --state "$work/server" > "$work/list.json" 2>/dev/null; then return; fi; sleep 0.2; done; return 1; }
start
for id in first second; do
 jq -nc --arg id "$id" --arg image "$ORDO_TEST_IMAGE" --arg archive "$ORDO_TEST_ARCHIVE" '{id:$id,image:{archive:$archive,id:$image},resources:{cores:2,memory_mib:256},args:["-c","printf start; sleep 30; printf done > /outputs/result"]}' > "$work/$id.json"
 "$ORDO_TEST_BINARY" submit --state "$work/server" --file "$work/$id.json" > "$work/$id-submitted.json"
done
for ((i=0;i<90;i++)); do
 "$ORDO_TEST_BINARY" queue list --state "$work/server" > "$work/list.json"
 if jq -e 'length==2 and all(.state=="running") and ([.[].attempts[-1].worker]|unique|length)==2' "$work/list.json" >/dev/null; then break; fi
 sleep 1
done
jq -e 'all(.state=="running")' "$work/list.json" >/dev/null
pgrep -P "$pid" > "$work/ssh-pids.txt" || true
kill -KILL "$pid"; wait "$pid" 2>/dev/null || true; pid=
sleep 3
while IFS= read -r child; do if kill -0 "$child" 2>/dev/null; then printf 'Orphan transport %s\n' "$child" >&2; exit 1; fi; done < "$work/ssh-pids.txt"
start
for ((i=0;i<90;i++)); do
 "$ORDO_TEST_BINARY" queue list --state "$work/server" > "$work/final.json"
 if jq -e 'all(.state=="succeeded")' "$work/final.json" >/dev/null; then break; fi
 sleep 1
done
jq -e 'length==2 and all(.state=="succeeded" and (.attempts|length)==1)' "$work/final.json" >/dev/null
while IFS= read -r result; do test "$(<"$result/outputs/result")" = done; done < <(jq -r '.[].attempts[-1].result_dir' "$work/final.json")
jq -n --arg image "$ORDO_TEST_IMAGE" --arg archive "$ORDO_TEST_ARCHIVE" '{id:"dag",tasks:([
 {id:"produce",resources:{cores:1,memory_mib:128},args:["-c","printf source > /outputs/value"]},
 {id:"consume",resources:{cores:1,memory_mib:128},from:{upstream:"produce"},args:["-c","cp /inputs/upstream/value /outputs/value"]},
 {id:"fail",resources:{cores:1,memory_mib:128},retry:{task:1},args:["-c","exit 9"]},
 {id:"skip",resources:{cores:1,memory_mib:128},depends_on:["fail"],args:["-c","exit 0"]},
 {id:"timeout",resources:{cores:1,memory_mib:128},timeout_seconds:2,args:["-c","sleep 30"]},
 {id:"cancel",resources:{cores:1,memory_mib:128},args:["-c","sleep 60"]}
] | map(.image={archive:$archive,id:$image}))}' > "$work/workflow.json"
"$ORDO_TEST_BINARY" queue workflow --state "$work/server" --file "$work/workflow.json" > "$work/workflow-submitted.json"
for ((i=0;i<60;i++)); do
  "$ORDO_TEST_BINARY" queue get --state "$work/server" --id dag--cancel > "$work/cancel.json"
  if jq -e '.attempts[-1].container_id != null' "$work/cancel.json" >/dev/null; then break; fi
  sleep 1
done
"$ORDO_TEST_BINARY" queue cancel --state "$work/server" --id dag--cancel > "$work/cancel-request.json"
for ((i=0;i<90;i++)); do
  "$ORDO_TEST_BINARY" queue list --state "$work/server" > "$work/workflow-final.json"
  if jq -e 'map(select(.workflow=="dag")) | length==6 and all(.state=="succeeded" or .state=="failed" or .state=="cancelled" or .state=="skipped")' "$work/workflow-final.json" >/dev/null; then break; fi
  sleep 1
done
jq -e 'map(select(.workflow=="dag")) | map({key:.id,value:.}) | from_entries |
 .["dag--produce"].state=="succeeded" and .["dag--consume"].state=="succeeded" and
 .["dag--fail"].state=="failed" and (.["dag--fail"].attempts|length)==2 and
 .["dag--skip"].state=="skipped" and (.["dag--skip"].attempts//[]|length)==0 and
 .["dag--timeout"].state=="failed" and .["dag--timeout"].attempts[-1].stop_requested=="timeout" and
 .["dag--cancel"].state=="cancelled"' "$work/workflow-final.json" >/dev/null
result=$(jq -r '.[] | select(.id=="dag--consume") | .attempts[-1].result_dir' "$work/workflow-final.json")
test "$(<"$result/outputs/value")" = source
if [[ -n ${ORDO_TEST_CHROMIUM:-} ]]; then
 url=$(awk '/Web UI:/{url=$3}END{print url}' "$work/server.log")
 node tests/web-smoke.mjs "$url" "$work/server" "$work"
fi
passed=true
printf 'PASS: queue recovery, DAG outputs, bounded retry, timeout, cancellation\n'
