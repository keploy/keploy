#!/usr/bin/env bash
# go-test-shard.tests.sh — unit tests for go-test-shard.sh and structural
# invariants for .github/workflows/go-test.yaml.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$here/../../.." && pwd)"
shard_sh="$here/go-test-shard.sh"
wf="$repo_root/.github/workflows/go-test.yaml"
action="$repo_root/.github/actions/go-test-setup/action.yml"

cases=0
fails=0

pass() { cases=$((cases + 1)); }
fail() {
  cases=$((cases + 1))
  fails=$((fails + 1))
  echo "FAIL: $*" >&2
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# 1. Exact disjoint partition over a realistic package list (heavy + non-heavy + future package).
cat > "$tmp/pkgs.txt" << 'EOF'
go.keploy.io/server/v3/pkg/client/app
go.keploy.io/server/v3/pkg/service/record
go.keploy.io/server/v3/pkg/matcher/learn
go.keploy.io/server/v3/pkg/agent/proxy
go.keploy.io/server/v3/pkg/service/replay
go.keploy.io/server/v3/pkg/agent/proxy/integrations/http
go.keploy.io/server/v3/pkg/platform/http
go.keploy.io/server/v3/utils
go.keploy.io/server/v3/pkg
go.keploy.io/server/v3/pkg/service/mock
go.keploy.io/server/v3/pkg/platform/yaml/mockdb
go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer
go.keploy.io/server/v3/pkg/platform/yaml/mapdb
go.keploy.io/server/v3/pkg/brandnew/futurepkg
go.keploy.io/server/v3/cli
go.keploy.io/server/v3/config
go.keploy.io/server/v3/internal/starvetest
go.keploy.io/server/v3/pkg/agent/netnames
go.keploy.io/server/v3/pkg/agent/routes
go.keploy.io/server/v3/pkg/agent/token
go.keploy.io/server/v3/pkg/models
go.keploy.io/server/v3/pkg/service/tools
go.keploy.io/server/v3/pkg/service/utgen
go.keploy.io/server/v3/utils/log
go.keploy.io/server/v3/utils/stall
EOF

for total in 1 2 3 4 5; do
  : > "$tmp/union.$total"
  for ((s = 0; s < total; s++)); do
    bash "$shard_sh" --stdin "$s" "$total" < "$tmp/pkgs.txt" > "$tmp/s.$total.$s"
    cat "$tmp/s.$total.$s" >> "$tmp/union.$total"
  done
  want="$(LC_ALL=C sort -u "$tmp/pkgs.txt")"
  got_sorted="$(LC_ALL=C sort "$tmp/union.$total")"
  got_uniq="$(LC_ALL=C sort -u "$tmp/union.$total")"
  if [ "$got_sorted" != "$got_uniq" ]; then
    fail "partition total=$total has duplicate package assignments"
  elif [ "$got_sorted" != "$want" ]; then
    fail "partition total=$total union differs from input package list"
  else
    pass
  fi
done

# 2. Top-3 heaviest packages land on 3 distinct shards for total=3.
s0="$(cat "$tmp/s.3.0")"
s1="$(cat "$tmp/s.3.1")"
s2="$(cat "$tmp/s.3.2")"
if grep -qx 'go.keploy.io/server/v3/pkg/client/app' <<< "$s0" &&
   grep -qx 'go.keploy.io/server/v3/pkg/service/record' <<< "$s1" &&
   grep -qx 'go.keploy.io/server/v3/pkg/matcher/learn' <<< "$s2"; then
  pass
else
  fail "top-3 heavy packages were not spread across shards 0, 1, 2"
fi

# 3. Serpentine balance: 4th heaviest (agent/proxy) lands on shard 2, 5th (service/replay) on shard 1, 6th (http) on shard 0.
if grep -qx 'go.keploy.io/server/v3/pkg/agent/proxy' <<< "$s2" &&
   grep -qx 'go.keploy.io/server/v3/pkg/service/replay' <<< "$s1" &&
   grep -qx 'go.keploy.io/server/v3/pkg/agent/proxy/integrations/http' <<< "$s0"; then
  pass
else
  fail "serpentine heavy-package spread broken for ranks 4-6"
fi

# 4. Unsorted / duplicate stdin produces identical deterministic output.
{ cat "$tmp/pkgs.txt"; tac "$tmp/pkgs.txt"; } > "$tmp/dup.txt"
for s in 0 1 2; do
  got_dup="$(bash "$shard_sh" --stdin "$s" 3 < "$tmp/dup.txt")"
  want_s="$(cat "$tmp/s.3.$s")"
  if [ "$got_dup" = "$want_s" ]; then
    pass
  else
    fail "unsorted/duplicate input changed shard $s output"
  fi
done

# 5. Fail-closed argument & empty-input validation.
for bad in \
  "" \
  "0" \
  "0 0" \
  "3 3" \
  "4 3" \
  "-1 3" \
  "a 3" \
  "0 b"; do
  # shellcheck disable=SC2086
  if bash "$shard_sh" --stdin $bad < "$tmp/pkgs.txt" >/dev/null 2>&1; then
    fail "invalid args '$bad' unexpectedly succeeded"
  else
    pass
  fi
done

if printf '\n  \n' | bash "$shard_sh" --stdin 0 3 >/dev/null 2>&1; then
  fail "empty package list unexpectedly succeeded"
else
  pass
fi

# 6. Structural invariants on go-test.yaml and go-test-setup/action.yml.
load_yaml_json() {
  if python3 -c "import yaml" 2>/dev/null; then
    python3 -c 'import json, sys, yaml; print(json.dumps(yaml.safe_load(open(sys.argv[1]))))' "$1"
  elif command -v ruby >/dev/null 2>&1; then
    ruby -ryaml -rjson -e 'puts JSON.generate(YAML.safe_load(File.read(ARGV[0]), aliases: true))' "$1"
  else
    echo "error: neither python3 PyYAML nor ruby YAML parser is available" >&2
    return 1
  fi
}
wf_json="$(load_yaml_json "$wf")"
act_json="$(load_yaml_json "$action")"
python3 - "$wf" "$wf_json" "$act_json" << 'PYEOF'
import json, sys

wf_path, wf_json, act_json = sys.argv[1], sys.argv[2], sys.argv[3]
wf_raw = open(wf_path).read()
wf = json.loads(wf_json)
act = json.loads(act_json)

assert act["runs"]["using"] == "composite", "go-test-setup must be a composite action"
assert any(s.get("uses") == "actions/setup-go@v6" for s in act["runs"]["steps"]), "go-test-setup must use actions/setup-go@v6"
assert "inputs" not in act or "allow-userns" not in act.get("inputs", {}), "userns lift must be unconditional in go-test-setup"
act_runs = "\n".join(s.get("run", "") for s in act["runs"]["steps"])
assert "kernel.apparmor_restrict_unprivileged_userns=0" in act_runs, "go-test-setup must lift apparmor userns restriction"
for s in act["runs"]["steps"]:
    if "apparmor_restrict_unprivileged_userns=0" in s.get("run", ""):
        assert "if" not in s, "apparmor userns step must not be conditional"
assert wf["name"] == "Go-Test", f"workflow name changed: {wf['name']}"
wf_env = wf.get("env", {})
for req_env in (
    "KEPLOY_INTERNAL",
    "CGO_ENABLED",
    "KEPLOY_TEST_IPV6_REQUIRED",
    "KEPLOY_TEST_USERNS_REQUIRED",
    "KEPLOY_TEST_CPU_HOLD",
    "KEPLOY_TEST_STORAGE_IO_REQUIRED",
):
    assert wf_env.get(req_env) == "1", f"go-test.yaml must preserve workflow env {req_env}: 1"
assert " -short" not in wf_raw, "go-test.yaml must never pass -short"

jobs = wf["jobs"]
assert "test" in jobs, "aggregator job 'test' missing"
agg = jobs["test"]
assert agg["name"] == "go-test", f"aggregator display name must stay 'go-test', got {agg.get('name')}"
assert agg.get("if") == "always()", "aggregator must run with if: always()"

siblings = sorted(j for j in jobs if j != "test")
needs = sorted(agg["needs"])
assert needs == siblings, f"aggregator needs {needs} != sibling jobs {siblings}"

# Verify aggregator derives all job results dynamically from toJSON(needs) and fails closed
assert agg.get("env", {}).get("NEEDS_JSON") == "${{ toJSON(needs) }}", "aggregator must pass NEEDS_JSON: ${{ toJSON(needs) }}"
agg_run = "\n".join(s.get("run", "") for s in agg["steps"])
assert 'if [ "$res" != "success" ]' in agg_run, "aggregator must fail closed on any non-success result"

import subprocess
def run_agg(payload):
    return subprocess.run(
        ["bash", "-c", agg_run],
        env={"PATH": "/usr/local/bin:/usr/bin:/bin", "NEEDS_JSON": json.dumps(payload)},
        capture_output=True, text=True,
    )

ok_payload = {j: {"result": "success"} for j in siblings}
r = run_agg(ok_payload)
assert r.returncode == 0, f"aggregator failed on all-success payload: {r.stdout} {r.stderr}"
r = run_agg({})
assert r.returncode != 0, "aggregator passed on empty needs payload"
for malformed in ({"a": 1}, {"a": {"result": "success"}, "b": 5}, []):
    r = run_agg(malformed)
    assert r.returncode != 0, f"aggregator passed on malformed payload {malformed!r}"
for job_name in siblings:
    for bad_state in ("failure", "cancelled", "skipped", ""):
        bad_payload = dict(ok_payload)
        bad_payload[job_name] = {"result": bad_state}
        r = run_agg(bad_payload)
        assert r.returncode != 0, f"aggregator passed when {job_name} had result={bad_state!r}"

# Every job must have timeout-minutes <= 45 (well under the 60m ceiling)
for jname, jdef in jobs.items():
    tm = jdef.get("timeout-minutes")
    assert isinstance(tm, int) and 1 <= tm <= 45, f"job {jname} timeout-minutes={tm} out of [1, 45]"

# Unit matrix shards must be contiguous 0..total-1 and pass matrix shard/total + required env
unit = jobs["unit"]
matrix = unit["strategy"]["matrix"]
totals = matrix["total"]
assert len(totals) == 1 and totals[0] >= 2, f"unexpected unit matrix total: {totals}"
total = totals[0]
assert matrix["shard"] == list(range(total)), f"unit shards {matrix['shard']} != 0..{total-1}"
assert unit["strategy"].get("fail-fast") is False, "unit matrix must set fail-fast: false"
unit_test_steps = [s for s in unit.get("steps", []) if "go-test-shard.sh" in s.get("run", "")]
assert len(unit_test_steps) == 1, "expected exactly one unit step calling go-test-shard.sh"
unit_step = unit_test_steps[0]
assert unit_step.get("env", {}).get("KEPLOY_TEST_REAL_DOCKER") == "1", "unit step missing KEPLOY_TEST_REAL_DOCKER: 1"
assert unit_step.get("env", {}).get("GOTMPDIR", "").startswith("/tmp/keploy-go-test-"), "unit step missing long GOTMPDIR"
assert 'go-test-shard.sh" "${{ matrix.shard }}" "${{ matrix.total }}"' in unit_step["run"], "unit step must pass matrix shard and total to go-test-shard.sh"

# All required suites must be present across jobs
all_runs = "\n".join(s.get("run", "") for j in jobs.values() for s in j.get("steps", []))
required_snippets = [
    "go-test-shard.sh",
    "KEPLOY_PROVEN=1 KEPLOY_LEARN_SET_EVERY_CACHE=1 go test -count=1 ./pkg/matcher/learn/",
    "KEPLOY_PROVEN=1 KEPLOY_LEARN_LAZY_EVERY_RAW=1 go test -count=1 ./pkg/matcher/learn/",
    "go test -race -count=1 -run '^Test(Tracker|Order)",
    "KEPLOY_LARGE_STREAM_MIB=256 go test -count=1 -timeout 20m -v",
    "KEPLOY_TEST_KCMP_REQUIRED=1 /tmp/utils.test",
    "KEPLOY_TEST_KCMP_REQUIRED=1 /tmp/http.test",
    "KEPLOY_TEST_CPU_HOLD=1 KEPLOY_TEST_POLICY_RESTORE_REQUIRED=1 /tmp/starvetest.test",
    "sudo /tmp/hooks-linux.test",
    "go test -race -count=1 -timeout 15m ./pkg/agent/proxy/... ./pkg/agent/netnames/... ./pkg/agent/routes/...",
    "go test -race -count=1 ./pkg/agent/token/...",
    "go test -race -count=1 -timeout 15m ./pkg/platform/yaml/mapdb/",
    "go test -race -count=1 -timeout 20m ./pkg/service/replay/",
]
for snip in required_snippets:
    assert snip in all_runs, f"missing required test command in go-test.yaml: {snip}"

# MySQL service must be present on both unit and race-proxy
for jname in ("unit", "race-proxy"):
    assert "mysql" in jobs[jname].get("services", {}), f"{jname} missing mysql service"
    assert jobs[jname].get("env", {}).get("KEPLOY_TEST_MYSQL_REQUIRED") == "1", f"{jname} missing KEPLOY_TEST_MYSQL_REQUIRED"
PYEOF
pass

if [ "$fails" -ne 0 ]; then
  echo "FAILED: $fails/$cases cases" >&2
  exit 1
fi
echo "PASS: all $cases go-test-shard and go-test.yaml checks succeeded"
