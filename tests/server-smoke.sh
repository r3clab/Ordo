#!/usr/bin/env bash
set -euo pipefail
: "${ORDO_TEST_BINARY:?}" "${ORDO_TEST_INSTALLABLE:?}" "${ORDO_TEST_IMAGE:?}"
work=$(mktemp -d /tmp/ordo-server-evidence.XXXXXX)
root_a=$(ssh vegeta 'umask 077; mktemp -d /tmp/ordo-server-test.XXXXXX')
root_b=$(ssh goku 'umask 077; mktemp -d /tmp/ordo-server-test.XXXXXX')
passed=false
pid=
cleanup() {
  if [[ -n $pid ]]; then kill -TERM "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  { "$ORDO_TEST_BINARY" queue list --state "$work/server" | jq -r '.[].id' | while IFS= read -r id; do "$ORDO_TEST_BINARY" queue get --state "$work/server" --id "$id"; done; } 2>/dev/null | jq -s . > "$work/cleanup.json" || true
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
jq -n --arg a "$root_a" --arg b "$root_b" '{port:0,workers:[{host:"vegeta",state_dir:$a,cpus:"62,63,126,127",memory_mib:512},{host:"goku",state_dir:$b,cpus:"62,63,126,127",memory_mib:512}]}' > "$work/server.json"
start() { "$ORDO_TEST_BINARY" server --state "$work/server" --config "$work/server.json" >> "$work/server.log" 2>&1 & pid=$!; for ((i=0;i<50;i++)); do if "$ORDO_TEST_BINARY" queue list --state "$work/server" > "$work/list.json" 2>/dev/null; then return; fi; sleep 0.2; done; return 1; }
start
out=$work/out
for name in first second; do
 jq -nc --arg out "$out/$name" --arg installable "$ORDO_TEST_INSTALLABLE" '{out:$out,image:$installable,resources:{cores:2,memory_mib:256},args:["-c","printf start; sleep 30; printf done > /outputs/result"]}' > "$work/$name.json"
 "$ORDO_TEST_BINARY" submit --state "$work/server" --file "$work/$name.json" > "$work/$name-submitted.json"
done
if ! "$ORDO_TEST_BINARY" queue get --state "$work/server" --out "$out/first" | jq -e --arg image "$ORDO_TEST_IMAGE" '.steps[0].spec.image==$image' >/dev/null; then echo "ORDO_TEST_IMAGE does not match the image built from ORDO_TEST_INSTALLABLE" >&2; exit 1; fi
for ((i=0;i<90;i++)); do
 "$ORDO_TEST_BINARY" queue list --state "$work/server" > "$work/list.json"
 if jq -e 'length==2 and all(.state=="running") and ([.[].worker]|unique|length)==2' "$work/list.json" >/dev/null; then break; fi
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
jq -e 'length==2 and all(.state=="succeeded" and .attempts==1)' "$work/final.json" >/dev/null
for name in first second; do test "$(<"$out/$name/result")" = "done"; test -s "$out/$name/.ordo/job.json"; done
dag=$out/dag
jq -nc --arg dag "$dag" --arg installable "$ORDO_TEST_INSTALLABLE" '
 {out:"\($dag)/produce",resources:{cores:1,memory_mib:128},args:["-c","printf source > /outputs/value"]},
 {out:"\($dag)/consume",inputs:{upstream:"\($dag)/produce"},resources:{cores:1,memory_mib:128},args:["-c","cp /inputs/upstream/value /outputs/value"]},
 {out:"\($dag)/steps",steps:[
  {name:"one",image:$installable,resources:{cores:1,memory_mib:128},args:["-c","printf one > /outputs/value"]},
  {name:"two",image:$installable,resources:{cores:2,memory_mib:256},args:["-c","printf two >> /outputs/value"]}]},
 {out:"\($dag)/fail",retry:{task:1},resources:{cores:1,memory_mib:128},args:["-c","printf partial > /outputs/value; exit 9"]},
 {out:"\($dag)/skip",inputs:{upstream:"\($dag)/fail"},resources:{cores:1,memory_mib:128},args:["-c","exit 0"]},
 {out:"\($dag)/timeout",resources:{cores:1,memory_mib:128},timeout_seconds:2,args:["-c","sleep 30"]},
 {out:"\($dag)/cancel",resources:{cores:1,memory_mib:128},args:["-c","sleep 60"]}
 | if .steps then . else .image=$installable end' > "$work/dag.jsonl"
"$ORDO_TEST_BINARY" submit --state "$work/server" --file "$work/dag.jsonl" --wait > "$work/dag-waited.json" 2> "$work/dag-waited.err" &
waiter=$!
for ((i=0;i<60;i++)); do
  "$ORDO_TEST_BINARY" queue get --state "$work/server" --out "$dag/cancel" > "$work/cancel.json" 2>/dev/null || true
  if jq -e '.attempts[-1].container_id != null' "$work/cancel.json" >/dev/null 2>&1; then break; fi
  sleep 1
done
"$ORDO_TEST_BINARY" queue cancel --state "$work/server" --out "$dag/cancel" > "$work/cancel-request.json"
status=0; wait "$waiter" || status=$?
test "$status" = 2
grep -Fx '4 of 7 jobs did not succeed' "$work/dag-waited.err"
jq -se --arg dag "$dag" 'length==14 and (.[7:] | map({key:(.out|ltrimstr("\($dag)/")),value:.state}) | from_entries) ==
 {produce:"succeeded",consume:"succeeded",steps:"succeeded",fail:"failed",skip:"skipped",timeout:"failed",cancel:"cancelled"}' "$work/dag-waited.json" >/dev/null
"$ORDO_TEST_BINARY" queue list --state "$work/server" --under "$dag" > "$work/dag-final.json"
jq -e --arg dag "$dag" 'map({key:(.out|ltrimstr("\($dag)/")),value:.}) | from_entries |
 .fail.attempts==2 and (.skip.attempts//0)==0 and .steps.steps==2' "$work/dag-final.json" >/dev/null
"$ORDO_TEST_BINARY" queue get --state "$work/server" --out "$dag/timeout" | jq -e '.attempts[-1].stop_requested=="timeout"' >/dev/null
test "$(<"$dag/consume/value")" = source
test "$(<"$dag/steps/value")" = onetwo
test -f "$dag/steps/.ordo/one.stdout" && test -f "$dag/steps/.ordo/two.stdout"
test "$(<"$dag/fail/value")" = partial
jq -e '.state=="failed"' "$dag/fail/.ordo/job.json" >/dev/null
test ! -e "$dag/skip" && test ! -e "$dag/cancel"
if [[ -n ${ORDO_TEST_CHROMIUM:-} ]]; then
 url=$(awk '/Web UI:/{url=$3}END{print url}' "$work/server.log")
 node tests/web-smoke.mjs "$url" "$work/server" "$work"
fi
passed=true
printf 'PASS: queue recovery, delivery into out, path dependencies, steps, bounded retry, timeout, cancellation, --wait\n'
