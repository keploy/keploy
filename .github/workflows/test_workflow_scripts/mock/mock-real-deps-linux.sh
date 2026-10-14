#!/usr/bin/env bash
# e2e for mock.real (the proven model, KEPLOY_PROVEN=1, design v14 §4):
# `keploy mock record` reaches the destinations keploy.yml lists for real,
# never recording them, and writes into each test's recording which ones it
# used; everything else is recorded as before.
#
# Three local HTTP servers play the destinations:
#   - REAL, at 127.0.0.1:<port>, listed as an address entry (localhost:<port>);
#   - NAMED, at 127.0.0.2:<port>, listed as a name entry (real-lane.test) that
#     only the hosts file maps, and that shares its address with an unlisted
#     name (other-lane.test): keploy's DNS never names the address, so each
#     connection is named by its request's Host;
#   - DEP, an ordinary dependency.
# The recording must:
#   - have no mock of a call to REAL or to real-lane.test, and list them in
#     test_real's mapping (real:), and in no other test's;
#   - record the calls to DEP and to other-lane.test (same address and port as
#     real-lane.test) as mocks;
#   - close, as capture_incomplete, a connection to real-lane.test's address
#     whose first bytes name nothing, and say so.
# Without the switch the list is not read: every call is recorded and no test
# lists a REAL destination. (A replay under the switch needs a build that
# judges each test, keploy enterprise; pkg/agent/proxy's tests cover a
# replay's REAL connections, which the agent decides as a record's.)
#
# Env: RECORD_BIN (the keploy binary under test).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

if ! python3 -c "import pytest, yaml" 2>/dev/null; then
  python3 -m ensurepip --default-pip >/dev/null 2>&1 || true
  # A distribution's python refuses a system install (PEP 668) unless told.
  python3 -m pip install --quiet --disable-pip-version-check pytest pyyaml ||
    python3 -m pip install --quiet --disable-pip-version-check --break-system-packages pytest pyyaml
fi
python3 -c "import pytest, yaml" || { echo "FAIL: pytest and pyyaml could not be installed"; exit 1; }

# keploy runs the test command as root (sudo), whose python does not read
# this user's site-packages, where pip may have put them.
PYPATH="$(python3 -c 'import site; print(site.getusersitepackages())')"

RECORD_BIN="${RECORD_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK"
FAIL=0
PIDS=()
NAME=real-lane.test
OTHER=other-lane.test
HOSTS_MARK="# keploy mock-real-deps lane $$"

# The servers are this shell's own children, stopped by their pids; the hosts
# lines are this run's, removed by their mark.
finish() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; wait "$p" 2>/dev/null; done
  # Rewritten in place, not by sed -i's rename: in a container /etc/hosts is
  # a bind mount, which a rename cannot replace.
  local hosts
  hosts=$(sed "/$HOSTS_MARK\$/d" /etc/hosts) && printf '%s\n' "$hosts" | sudo tee /etc/hosts >/dev/null
  cd / && sudo rm -rf "$WORK"
}
trap finish EXIT

printf '127.0.0.2 %s %s %s\n' "$NAME" "$OTHER" "$HOSTS_MARK" | sudo tee -a /etc/hosts >/dev/null

# server.py WHO ADDR: answers every GET with who it is and the path, notes
# each in hits.<WHO>, and prints "ready <port>" once it listens.
cat > server.py <<'PY'
import http.server, json, sys
who, addr = sys.argv[1], sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open("hits." + who, "a") as f:
            f.write(self.path + "\n")
        b = json.dumps({"who": who, "path": self.path}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a): pass
s = http.server.HTTPServer((addr, 0), H)
print("ready", s.server_address[1], flush=True)
s.serve_forever()
PY

# start WHO ADDR: starts a server and sets PORT to its port once it said
# ready (its first line: a read that ends with the server, never a clock).
start() {
  local fifo="$WORK/$1.fifo" line
  mkfifo "$fifo"
  python3 server.py "$1" "$2" >"$fifo" 2>&1 &
  PIDS+=($!)
  exec 3<"$fifo"
  IFS= read -r line <&3 || { echo "FAIL: server $1 exited before it listened"; exit 1; }
  exec 3<&-
  case "$line" in ready\ *) PORT=${line#ready } ;; *) echo "FAIL: server $1 said $line"; exit 1 ;; esac
}
start real 127.0.0.1; REAL_PORT=$PORT
start named 127.0.0.2; NAMED_PORT=$PORT
start dep 127.0.0.1; DEP_PORT=$PORT
export REAL_PORT NAMED_PORT DEP_PORT NAME OTHER

cat > conftest.py <<'PY'
import os, json, urllib.request, pytest
AGENT=os.environ.get("KEPLOY_MOCK_AGENT")
TOKEN=os.environ.get("KEPLOY_MOCK_AGENT_TOKEN")
def _post(p,b):
    if not AGENT: return None
    h={"Content-Type":"application/json"}
    if TOKEN: h["Authorization"]="Bearer "+TOKEN
    r=urllib.request.Request(AGENT+p,data=json.dumps(b).encode(),headers=h,method="POST")
    return json.loads(urllib.request.urlopen(r).read() or b"{}")
@pytest.fixture(autouse=True, scope="session")
def keploy_claim():
    if os.environ.get("KEPLOY_PROVEN")=="1":
        _post("/agent/instance",{"key":".","dir":os.getcwd()})
    yield
@pytest.fixture(autouse=True)
def keploy_scope(request, keploy_claim):
    test={"name":request.node.name,"file":request.node.path.name}
    resp=_post("/agent/scope/begin",dict(test,canSkip=True))
    if resp and resp.get("action")=="skip":
        pytest.skip(resp.get("reason") or "keploy: not run this replay")
    yield
    _post("/agent/scope/end",dict(test,outcome="passed"))
PY

cat > test_real_deps.py <<'PY'
import http.client, json, os, socket
REAL, NAMED, DEP = int(os.environ["REAL_PORT"]), int(os.environ["NAMED_PORT"]), int(os.environ["DEP_PORT"])
NAME, OTHER = os.environ["NAME"], os.environ["OTHER"]
def get(host, port, path):
    # One connection per call, so each is decided on its own.
    c = http.client.HTTPConnection(host, port)
    c.request("GET", path)
    b = c.getresponse().read()
    c.close()
    return json.loads(b)
def test_real():
    assert get("127.0.0.1", REAL, "/r1")["who"] == "real"
    assert get(NAME, NAMED, "/r2")["who"] == "named"
def test_dep():
    assert get("127.0.0.1", DEP, "/d1")["who"] == "dep"
def test_other_name():
    # Same address and port as NAME, another name: a dependency.
    assert get(OTHER, NAMED, "/o1")["who"] == "named"
def test_unnamed():
    # Bytes that name nothing, to an address a mock.real name resolves to:
    # under the switch keploy cannot tell which name it is for, and closes it
    # unanswered; without it they reach the server, which refuses them.
    s = socket.create_connection(("127.0.0.2", NAMED))
    s.sendall(b"\x00\x01\x02\x03\r\n\r\n")
    try:
        got = s.recv(16)
    except ConnectionResetError:
        got = b""
    s.close()
    if os.environ.get("KEPLOY_PROVEN") == "1":
        assert got == b""
    else:
        # The server answers garbage (how depends on its python: a 400, or
        # an HTTP/0.9-style bare body).
        assert got != b""
PY

CMD="python3 -m pytest -q -p no:cacheprovider test_real_deps.py"

# record NAME LOG PROVEN: records the set, the agent's debug lines included.
record() {
  local name="$1" log="$2" proven="$3"
  sudo -E env PATH="$PATH" PYTHONPATH="$PYPATH" KEPLOY_PROVEN="$proven" REAL_PORT="$REAL_PORT" NAMED_PORT="$NAMED_PORT" DEP_PORT="$DEP_PORT" NAME="$NAME" OTHER="$OTHER" \
    "$RECORD_BIN" mock record -c "$CMD" --name "$name" --disable-tele --debug > "$log" 2>&1
  local rc=$?
  grep -E "[0-9]+ (passed|failed|error)" "$log" | tail -3
  if [ "$rc" -ne 0 ]; then
    echo "FAIL: record of $name ($log) exited $rc"
    tail -60 "$log"
    FAIL=1
  fi
}

# paths_of NAME: the path of each HTTP mock of the set, sorted.
paths_of() {
  python3 - "keploy/$1/mocks.yaml" <<'PY'
import sys, yaml
try:
    docs = list(yaml.safe_load_all(open(sys.argv[1])))
except FileNotFoundError:
    docs = []
out = []
for d in docs:
    if d and d.get("kind") == "Http":
        url = d["spec"]["req"]["url"]
        out.append(url.split("://", 1)[-1].split("/", 1)[-1])
print("\n".join("/" + p for p in sorted(out)))
PY
}

# real_of NAME: "<test> <dest,dest>" for each test of the set's mappings that
# lists a REAL destination, sorted.
real_of() {
  python3 - "keploy/$1/mappings.yaml" <<'PY'
import sys, yaml
try:
    doc = yaml.safe_load(open(sys.argv[1])) or {}
except FileNotFoundError:
    doc = {}
rows = []
for t in doc.get("tests") or []:
    if t.get("real"):
        rows.append("%s %s" % (t["id"], ",".join(t["real"])))
print("\n".join(sorted(rows)))
PY
}

expect() {
  local file="$1" what="$2"
  shift 2
  printf '%s\n' "$@" > "$file.want"
  if ! diff -u "$file.want" "$file"; then
    echo "FAIL: $what"
    FAIL=1
  fi
}

cat > keploy.yml <<EOF
mock:
  real:
    - localhost:${REAL_PORT}
    - ${NAME}:${NAMED_PORT}
EOF

echo "== 1. record under the proven switch =="
record rd rec.log 1
grep -q "4 passed" rec.log || { echo "FAIL: pytest did not pass every test while recording"; FAIL=1; }
paths_of rd > paths.txt
cat paths.txt
expect paths.txt "a REAL call was recorded, or a dependency call was not" "/d1" "/o1"
real_of rd > real.txt
cat real.txt
expect real.txt "the recording does not list test_real's REAL destinations, and only its" \
  "test_real_deps.py::test_real localhost:${REAL_PORT},${NAME}:${NAMED_PORT}"
grep -q "keploy closed a connection it could not name: capture_incomplete" rec.log ||
  { echo "FAIL: the connection keploy could not name was not reported as capture_incomplete"; FAIL=1; }
grep -q "keploy relays these destinations to the real thing" rec.log ||
  { echo "FAIL: the agent did not say which destinations it relays"; FAIL=1; }
for who in real named dep; do
  [ -s "hits.$who" ] || { echo "FAIL: the $who server was never reached while recording"; FAIL=1; }
done

echo "== 2. without the switch the list is not read =="
rm -f hits.*
record rd-off rec-off.log ""
grep -q "4 passed" rec-off.log || { echo "FAIL: pytest did not pass every test recording without the switch"; FAIL=1; }
paths_of rd-off > paths-off.txt
cat paths-off.txt
expect paths-off.txt "without the switch a call was not recorded" "/d1" "/o1" "/r1" "/r2"
real_of rd-off > real-off.txt
expect real-off.txt "a recording made without the switch lists a REAL destination" ""
# keploy's own lines only: a failing test's traceback echoes its source.
if grep -aE "Keploy.*(mock\.real|capture_incomplete)" rec-off.log; then
  echo "FAIL: an agent without the switch acted on mock.real"
  FAIL=1
fi

# A race build reports a data race and carries on: fail on one in any leg.
if grep -l "WARNING: DATA RACE" ./*.log; then
  echo "FAIL: a data race, in the logs above"
  FAIL=1
fi

if [ "$FAIL" -ne 0 ]; then
  echo "mock.real e2e FAILED"
  exit 1
fi
echo "mock.real e2e passed"
