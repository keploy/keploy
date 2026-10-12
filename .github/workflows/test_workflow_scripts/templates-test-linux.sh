#!/usr/bin/env bash
# E2E: keploy record → keploy templatize → keploy test, for values an app
# makes on every run.
#
# The app mints a user id and an order number of seven digits (at least
# 1000000) on every create, answered 201 with a Location naming the user, and
# a CSRF token and a session id on every login, set as two cookies (the CSRF
# one with an Expires date). A read-back by the id, an order lookup by the
# number, and a request carrying both cookies follow, then a logout that
# redirects (302) to /bye, setting a cookie that names the same page, and a
# call to /bye that sends the cookie. Replayed as recorded,
# the create and the login answer new values and the later requests send the
# old ones. `keploy templatize` writes the create's id and number and each
# cookie as captures ({{capture string .id}}, "{{capture int .orderNo}}",
# csrftoken={{capture string .csrftoken}}), the create's own Location as a use
# of the id it captures (/users/{{string .id}}), each later request that
# sends them as uses, and config.yaml's template gets the recorded values.
#
#   1. record the seven test cases.
#   2. keploy test before templatize: the set fails.
#   3. keploy templatize: captures at the create and the login, uses after and
#      in the create's Location; it prints what each capture no longer
#      compares. The logout's redirect target stays as recorded (only a 201's
#      Location names a resource the request created), and templatize says
#      so; the page cookie beside it is captured, and the redirect holds no
#      use of that capture (it would accept any target the cookie names).
#   4. keploy test: every test case passes; the read-back, the lookup and the
#      cookie request were sent this run's values (the order number as its
#      digits, both cookies); the report says what was captured; config.yaml
#      is byte for byte what templatize wrote.
#   5. keploy templatize again changes nothing: the set holds placeholders
#      now, and templatize writes only into a plain recording; it says so in
#      one line.
#   6. the create answers the recorded id (as a database served from mocks
#      does) and the read-back answers another id: the read-back fails. A
#      keploy that matched an expected value against the template VALUES
#      passes it (main did: PASSED, body_result.normal false).
#  6b. the logout redirects to /oops instead, and its cookie names /oops: the
#      logout fails, as it does without templates. A capture of the redirect
#      target would pass it, and so would a use there of the cookie's capture.
#   7. a variable wins over noise: with globalNoise on body.id, the read-back
#      answering another id still fails (its {{string .id}} is asserted), and
#      the noise entry over it is named. Noise alone would let it pass.
#      keploy report, keploy report --full, keploy report --format junit and
#      keploy report --format json each name the place (body.id) and the
#      reason; the logged next step says where the run's value came from and
#      what to do if the value changed on purpose, never keploy normalize, and
#      (the read-back's own request sent the id) never offers a capture there;
#      the warning over the create's capture says to keep it.
#   8. --update-template is ignored: the run passes and config.yaml keeps the
#      recorded values.
#   9. the read-back answers another name: it fails; keploy normalize accepts
#      the new answer and keeps the placeholders; the next run passes.
#  9b. templatize refuses an older templatized set and changes nothing. The
#      recording as an older keploy templatized it (one use per value, at a
#      place of the producing response picked by map order, here the logout's
#      redirect /{{string .page}}, the page cookie beside it written out, the
#      call to the page sending {{string .page}}): keploy templatize leaves
#      every file byte for byte as it is (a capture of the cookie would turn
#      the redirect's use into an echo of it) and names the set, its first
#      placeholder (with the file and line it is on) and what to do in one
#      line; keploy test replays the set as written, so the logout redirecting
#      to /oops with its cookie naming it fails.
#  9c. templatize writes all of a set or none of it: with a config.yaml that
#      does not parse it changes no file and says so, on a second run as on
#      the first (it never takes the set for one it finished), leaves no file
#      of its own behind, and exits non-zero.
#  9d. keploy test refuses a set a killed keploy templatize left half-written:
#      the files a templatize stopped between two renames leaves (some test
#      cases templatized, the files as they were beside them, the rest's new
#      bytes waiting) make keploy test run none of the set's test cases, fail
#      the set with the fix-up steps templatize prints, and exit 1; keploy
#      report, JUnit and --format json show each of its test cases failed and
#      refused, with the steps. Done as
#      written, the steps give the recording back, and templatize then writes
#      what it wrote at step 3. (The state is built file by file: a real kill
#      would be a race to time.)
#  9e. a variable in a test case's assertions is refused when the set loads:
#      the test case fails unsent, with the edit, and keploy test exits 1.
#
# Every keploy test's exit code is asserted: 0 when the set passes, 1 when a
# test case fails (a template variable's place included) or is refused, or the
# set is refused. The script waits for events, never for a length of time:
# for the app's port to take a connection, and for keploy to have written the
# seven test cases.
#  10. no ERROR in the record, templatize, normalize and passing test logs, and
#      no data race in any log.
#
# Uses port 18921 on 127.0.0.1. Env: RECORD_BIN (the binary that records) and
# REPLAY_BIN (the one that templatizes, tests and normalizes). mock_linux.yml
# runs it with the PR `build` binary for both, and again recording with the
# released `latest`: the recording format did not change, and a set recorded
# by a released keploy is what an upgrading user templatizes.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
APP=18921
FAIL=0
fail() {
  echo "FAIL: $*"
  FAIL=$((FAIL + 1))
}

cat > app.py <<'PY'
import email.utils, http.server, json, random, socketserver, sys, time, uuid
PORT = int(sys.argv[1])
# MODE "wrong-id": the read-back answers an id the app did not make, and the
# create answers the id and number given after it (as a database served from
# mocks answers what it recorded). MODE "v2": the read-back answers another
# name (a new field alone would pass: keploy accepts an additive change).
# MODE "redirect": the logout redirects to /oops, which answers as /bye does.
MODE = sys.argv[2] if len(sys.argv) > 2 else ""
FIXED = sys.argv[3:5] if len(sys.argv) > 4 else None
users, orders, session = {}, {}, {}
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def _send(self, code, obj, cookies=(), headers=()):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        for c in cookies: self.send_header("Set-Cookie", c)
        for k, v in headers: self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers(); self.wfile.write(body)
    def _json(self):
        return json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
    def do_POST(self):
        body = self._json()
        if self.path == "/users":
            uid = "usr_" + uuid.uuid4().hex[:12]           # a new id on every run
            order_no = random.randint(1000000, 9999999)     # >= 1e6: 7 digits
            if FIXED: uid, order_no = FIXED[0], int(FIXED[1])
            users[uid] = {"id": uid, "name": body.get("name", "alice")}
            orders[order_no] = uid
            return self._send(201, {"id": uid, "name": users[uid]["name"], "order_no": order_no},
                              headers=[("Location", "/users/" + uid)])
        if self.path == "/orders/lookup":
            n, u = body.get("order_no"), body.get("user")
            if isinstance(n, int) and orders.get(n) == u:
                return self._send(200, {"order_no": n, "user": u, "status": "open"})
            return self._send(404, {"error": "no such order", "asked": body})
        if self.path == "/logout":
            target = "/oops" if MODE == "redirect" else "/bye"
            return self._send(302, {"ok": True}, ["page=%s; Path=/" % target[1:]], headers=[("Location", target)])
        if self.path == "/login":
            session["csrf"] = "csrf_" + uuid.uuid4().hex[:12]
            session["sid"] = "sess_" + uuid.uuid4().hex[:16]
            expires = email.utils.formatdate(time.time() + 3600, usegmt=True)
            return self._send(200, {"ok": True}, [
                "csrftoken=%s; Path=/; Expires=%s" % (session["csrf"], expires),
                "sessionid=%s; Path=/; HttpOnly" % session["sid"],
            ])
        self._send(404, {"error": "not found", "path": self.path})
    def do_GET(self):
        parts = self.path.strip("/").split("/")
        if len(parts) == 2 and parts[0] == "users" and parts[1] in users:
            u = dict(users[parts[1]])
            if MODE == "wrong-id": u["id"] = "usr_" + uuid.uuid4().hex[:12]
            if MODE == "v2": u["name"] = "alice-v2"
            return self._send(200, u)
        if self.path in ("/bye", "/oops"):
            return self._send(200, {"bye": True})
        if self.path == "/me":
            jar = dict(p.strip().split("=", 1) for p in (self.headers.get("Cookie") or "").split(";") if "=" in p)
            if session and jar.get("csrftoken") == session["csrf"] and jar.get("sessionid") == session["sid"] \
                    and self.headers.get("X-CSRFToken") == session["csrf"]:
                return self._send(200, {"user": "alice"})
            return self._send(403, {"error": "csrf or session", "cookie": self.headers.get("Cookie")})
        self._send(404, {"error": "not found", "path": self.path})
    def log_message(self, *a): pass
socketserver.ThreadingTCPServer.allow_reuse_address = True
with socketserver.ThreadingTCPServer(("127.0.0.1", PORT), H) as s: s.serve_forever()
PY

# listening PORT: something accepts a connection there (base-path-linux.sh's
# probe). A connection and no request: under keploy record a request is a
# test case, and the recording is to hold the seven below and no probe.
listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# wait_for_port PORT SECONDS: until something listens there. The bound is a
# safety net for a run that hangs, as keploy gives its agent up to 330s.
wait_for_port() {
  local i
  for ((i = 0; i < $2 * 5; i++)); do
    listening "$1" && return 0
    sleep 0.2
  done
  echo "nothing listened on 127.0.0.1:$1 within $2s"
  return 1
}
# test_files: how many test case files keploy has written whole. keploy
# record reserves a name with an empty file and writes a .tmp beside it while
# a test case is in flight, so only a non-empty .yaml counts.
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
# wait_for_tests N: until keploy has written N test case files, with the
# same safety net.
wait_for_tests() {
  local i
  for ((i = 0; i < 360 * 5; i++)); do
    [ "$(test_files)" -ge "$1" ] && return 0
    sleep 0.2
  done
  return 1
}
stop_keploy() {
  local name; name="$(basename "$1")"
  sudo pkill -INT -x "$name" 2>/dev/null
  for _ in $(seq 1 30); do pgrep -x "$name" >/dev/null || break; sleep 1; done
  sudo pkill -9 -x "$name" 2>/dev/null
  sudo pkill -f "app.py $APP" 2>/dev/null
  for _ in $(seq 1 10); do listening "$APP" || break; sleep 1; done
}
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }
set_status() { grep -m1 '^status:' "$(REPORT)" 2>/dev/null | awk '{print $2}'; }
# replay runs keploy test; RC is its exit code.
replay() { # $1: log name, $2: arguments for the app, $3: extra keploy flag
  sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "python3 app.py $APP${2:+ $2}" --delay 6 \
    --generateGithubActions=false --disable-tele ${3:-} >"$1.log" 2>&1 </dev/null
  RC=$?
  stop_keploy "$REPLAY_BIN"; own
}
# expect_rc WANT WHAT: the last keploy test (or tool) exited WANT.
expect_rc() { [ "$RC" = "$1" ] || fail "$2: keploy exited $RC, want $1"; }
# tool runs a keploy subcommand; RC is its exit code.
tool() { # $1: log name, then the keploy subcommand and its flags
  local log="$1"; shift
  sudo -E env PATH="$PATH" "$REPLAY_BIN" "$@" --disable-tele >"$log.log" 2>&1 </dev/null
  RC=$?
  own
}
# test_status prints a test case's status in the newest report.
test_status() {
  awk -v id="$1" '/^ *- kind:/{st=""} /^ *status:/{ if (st=="") st=$2 } /test_case_id:/{ if ($2==id) {print st; exit} }' "$(REPORT)"
}
# case_of prints the test case file whose request URL matches $1.
case_of() { grep -l "url: .*$1" keploy/test-set-0/tests/*.yaml | head -n1; }
name_of() { basename "$1" .yaml; }
digest() { (cd keploy/test-set-0 && find tests config.yaml -type f -print0 | sort -z | xargs -0 sha256sum) | sha256sum | cut -d' ' -f1; }
# report_block prints a test case's block in the newest report.
report_block() {
  awk -v id="$1" '/^ *- kind:/{if (keep) exit; buf=""} {buf=buf $0 "\n"} /test_case_id:/{ if ($2==id) keep=1 } END{if (keep) printf "%s", buf}' "$(REPORT)"
}

if listening "$APP"; then echo "port $APP is in use"; exit 1; fi
trap 'stop_keploy "$RECORD_BIN" >/dev/null 2>&1' EXIT

echo "== 1. record: create, read back, look the order up, log in, call with both cookies =="
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "python3 app.py $APP" --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
wait_for_port "$APP" 360 || { echo "FAIL: the app did not start under keploy record"; tail -30 record.log; exit 1; }
# The first request goes as soon as the app listens: a request keploy does
# not record then is a keploy bug to report, not a wait to add.
CREATED="$(curl -s -XPOST "http://127.0.0.1:$APP/users" -H 'Content-Type: application/json' -d '{"name":"alice"}')"
REC_ID="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["id"])' "$CREATED" 2>/dev/null)"
REC_NO="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["order_no"])' "$CREATED" 2>/dev/null)"
[ -n "$REC_ID" ] && [ -n "$REC_NO" ] || fail "the create answered '$CREATED'"
curl -s "http://127.0.0.1:$APP/users/$REC_ID" | grep -q "\"$REC_ID\"" || fail "the read-back did not answer the id"
curl -s -XPOST "http://127.0.0.1:$APP/orders/lookup" -H 'Content-Type: application/json' \
  -d "{\"order_no\": $REC_NO, \"user\": \"$REC_ID\"}" | grep -q '"open"' || fail "the order lookup did not find the order"
curl -s -c cookies.txt -XPOST "http://127.0.0.1:$APP/login" -H 'Content-Type: application/json' -d '{"user":"alice"}' >/dev/null
REC_CSRF="$(awk '$6=="csrftoken"{print $7}' cookies.txt)"
REC_SID="$(awk '$6=="sessionid"{print $7}' cookies.txt)"
[ -n "$REC_CSRF" ] && [ -n "$REC_SID" ] || fail "the login set no csrftoken/sessionid cookies: $(cat cookies.txt)"
curl -s -b cookies.txt -H "X-CSRFToken: $REC_CSRF" "http://127.0.0.1:$APP/me" | grep -q '"alice"' || fail "the call with both cookies was refused"
[ "$(curl -s -o /dev/null -w '%{redirect_url}' -XPOST "http://127.0.0.1:$APP/logout")" = "http://127.0.0.1:$APP/bye" ] || fail "the logout did not redirect to /bye"
curl -s -H 'Cookie: page=bye' "http://127.0.0.1:$APP/bye" | grep -q '"bye"' || fail "/bye did not answer"
wait_for_tests 7 || fail "keploy record wrote $(test_files) test cases, want 7"
stop_keploy "$RECORD_BIN"; own
cp -a keploy keploy.plain # the recording as it is, for step 9b
N=$(ls keploy/test-set-0/tests 2>/dev/null | wc -l)
[ "$N" -eq 7 ] || { fail "recorded $N test cases, want 7"; tail -30 record.log; exit 1; }
T1=$(grep -l "url: .*/users$" keploy/test-set-0/tests/*.yaml | head -n1)
T2=$(case_of "/users/usr_")
T3=$(case_of "/orders/lookup")
T4=$(case_of "/login")
T5=$(case_of "/me")
T6=$(case_of "/logout")
T7=$(case_of "/bye")
for t in "$T1" "$T2" "$T3" "$T4" "$T5" "$T6" "$T7"; do [ -n "$t" ] || { fail "a recorded test case is missing: $(ls keploy/test-set-0/tests)"; exit 1; }; done

echo "== 2. keploy test before templatize: the set fails =="
replay plain
expect_rc 1 "step 2, a set that fails"
[ "$(set_status)" = FAILED ] || fail "without templates the set must fail: the create answers a new id (status: $(set_status))"

echo "== 3. keploy templatize: captures at the create and the login, uses after =="
tool templatize templatize
expect_rc 0 "step 3, keploy templatize"
grep -q '{{capture string .id}}' "$T1" || fail "the create's expected id is not a capture: $(grep -m1 'body:' "$T1")"
grep -q '{{capture int .orderNo}}' "$T1" || fail "the create's expected order number is not a capture"
grep -Eq "Location: '?/users/\{\{string \.id\}\}'?$" "$T1" || fail "the create's own Location is not a use of the id it captures: $(grep -m1 'Location:' "$T1")"
grep -q 'url: .*/users/{{string .id}}' "$T2" || fail "the read-back's URL does not use the id: $(grep -m1 'url:' "$T2")"
[ "$(grep -c '{{string .id}}' "$T2")" -ge 2 ] || fail "the read-back's expected id is not asserted with a placeholder: $(grep 'body:' "$T2")"
grep -q '"order_no": *"{{int .orderNo}}"' "$T3" || fail "the lookup's body does not send the order number: $(grep -m1 'body:' "$T3")"
grep -q '"user": *"{{string .id}}"' "$T3" || fail "the lookup's body does not send the id"
grep -q 'csrftoken={{capture string .csrftoken}}' "$T4" || fail "the login's csrftoken cookie is not a capture: $(grep -i -m1 'set-cookie' "$T4")"
grep -q 'sessionid={{capture string .sessionid}}' "$T4" || fail "the login's sessionid cookie is not a capture: $(grep -i -m1 'set-cookie' "$T4")"
grep -q 'Expires=' "$T4" || fail "the csrftoken cookie's Expires is gone from the expected Set-Cookie"
grep -q 'csrftoken={{string .csrftoken}}; *sessionid={{string .sessionid}}' "$T5" || fail "the call does not send both cookies: $(grep -i -m1 'cookie:' "$T5")"
grep -Eqi "X-Csrftoken: '?\{\{string \.csrftoken\}\}'?$" "$T5" || fail "the call does not send the CSRF header: $(grep -i -m1 'x-csrftoken' "$T5")"
grep -q "id: $REC_ID" keploy/test-set-0/config.yaml || fail "config.yaml does not hold the recorded id: $(cat keploy/test-set-0/config.yaml)"
grep -Eq "orderNo: \"?$REC_NO\"?$" keploy/test-set-0/config.yaml || fail "config.yaml does not hold the recorded order number as its digits: $(cat keploy/test-set-0/config.yaml)"
grep -q "csrftoken: $REC_CSRF" keploy/test-set-0/config.yaml || fail "config.yaml does not hold the recorded csrftoken"
for key in id orderNo csrftoken sessionid; do
  grep -q "now captures $key: its value is no longer compared" templatize.log || fail "templatize did not say what the $key capture no longer compares"
done
# The logout's redirect target is no resource the request created: it stays
# as recorded. The page cookie the logout sets is captured, and the redirect
# holds no use of that capture; the call to the page sends the captured value
# where it sent the recorded one, its cookie and its path (value flow).
grep -Eqi "location: '?/bye'?$" "$T6" || fail "templatize wrote in the logout's redirect target: $(grep -m1 -i 'location:' "$T6")"
grep -q 'page={{capture string .page}}' "$T6" || fail "the logout's page cookie is not a capture: $(grep -i -m1 'set-cookie' "$T6")"
grep -m1 'url:' "$T7" | grep -q 'url: http://127.0.0.1:[0-9]*/{{string .page}}$' || fail "the call to the page does not send the cookie's value in its path: $(grep -m1 'url:' "$T7")"
grep -Eqi "cookie: '?page=\{\{string \.page\}\}" "$T7" || fail "the call to the page does not send the page cookie: $(grep -i -m1 'cookie:' "$T7")"
grep -q 'of a 302 response holds Location.path.0 .*bye.*, which a later request sends: templatize captures nothing there' templatize.log \
  || fail "templatize did not say why it left the logout's redirect target as recorded: $(grep -m1 '302' templatize.log)"
TEMPLATED="$(digest)"
CONFIG_BEFORE="$(sha256sum keploy/test-set-0/config.yaml | cut -d' ' -f1)"
cp -a keploy keploy.templated # the set as templatize wrote it, for step 9d

echo "== 4. keploy test: every test case passes with this run's values =="
replay templated
expect_rc 0 "step 4, a set that passes"
if [ "$(set_status)" != PASSED ]; then
  fail "the templatized set did not pass (status: $(set_status))"
  grep -hE "^status:|test_case_id:|expected:|actual:|where:|why:" "$(REPORT)" | head -40
fi
SENT_URL=$(report_block "$(name_of "$T2")" | grep -m1 "url: .*/users/")
[ -n "$SENT_URL" ] || fail "the report has no request URL for the read-back"
echo "$SENT_URL" | grep -q "/users/usr_" || fail "the read-back was not sent to a user id: $SENT_URL"
echo "$SENT_URL" | grep -q "$REC_ID" && fail "the read-back was sent with the recorded id: $SENT_URL"
LOOKUP=$(report_block "$(name_of "$T3")" | grep -m1 'body: .*order_no')
[ -n "$LOOKUP" ] || fail "the report has no request body for the lookup"
echo "$LOOKUP" | grep -Eq '"order_no": *[1-9][0-9]{6}[,}]' || fail "the lookup was not sent a seven-digit JSON number: $LOOKUP"
echo "$LOOKUP" | grep -q "$REC_NO" && fail "the lookup was sent the recorded number: $LOOKUP"
COOKIE=$(report_block "$(name_of "$T5")" | grep -i -m1 'cookie: ')
[ -n "$COOKIE" ] || fail "the report has no Cookie for the call"
echo "$COOKIE" | grep -q "$REC_CSRF\|$REC_SID" && fail "the call was sent a recorded cookie: $COOKIE"
for key in id orderNo csrftoken sessionid; do
  grep -q "key: $key" "$(REPORT)" || fail "the report does not say what was captured for $key"
done
[ "$(sha256sum keploy/test-set-0/config.yaml | cut -d' ' -f1)" = "$CONFIG_BEFORE" ] || fail "keploy test changed config.yaml"

echo "== 5. keploy templatize again changes nothing =="
tool templatize2 templatize
expect_rc 0 "step 5, keploy templatize over a set it leaves as it is"
[ "$(digest)" = "$TEMPLATED" ] || fail "a second keploy templatize changed the test set"
grep -q "test-set-0 holds template placeholders already (.*): keploy templatize writes only into a plain recording, so it changes nothing in this set" templatize2.log \
  || fail "the second keploy templatize did not say it leaves the set as it is: $(tail -5 templatize2.log)"

echo "== 6. the create answers the recorded id, the read-back another id: it fails =="
replay wrong-id "wrong-id $REC_ID $REC_NO"
expect_rc 1 "step 6, a template variable's place that fails"
[ "$(set_status)" = FAILED ] || fail "the app answered an id it did not make and the set still passed (forgiven by value)"
[ "$(test_status "$(name_of "$T2")")" = FAILED ] || fail "the read-back is not the test that failed: $(test_status "$(name_of "$T2")")"
[ "$(test_status "$(name_of "$T1")")" = PASSED ] || fail "the create should still pass"

echo "== 6b. the logout redirects to /oops, its cookie naming it: it fails, as it does without templates =="
replay redirect redirect
expect_rc 1 "step 6b, a redirect target that fails"
[ "$(test_status "$(name_of "$T6")")" = FAILED ] || fail "the logout redirecting to /oops passed: its target must stay asserted"
[ "$(test_status "$(name_of "$T7")")" = PASSED ] || fail "the call to the page the cookie names should still pass"

echo "== 7. a variable wins over noise: globalNoise on body.id, the read-back answers another id: it still fails =="
cat > keploy.yml <<'KEPLOY_CFG'
test:
  globalNoise:
    global: {"body": {"id": []}}
KEPLOY_CFG
replay noise "wrong-id $REC_ID $REC_NO"
expect_rc 1 "step 7, a template variable's place under noise that fails"
rm -f keploy.yml
[ "$(test_status "$(name_of "$T2")")" = FAILED ] || fail "under noise on body.id the read-back answering another id passed: the variable must be asserted anyway"
report_block "$(name_of "$T2")" | grep -q "where: body.id" || fail "the read-back's report does not name the variable place it failed on"
grep -q "a noise entry covers a template variable's place" noise.log || fail "the noise entry over the read-back's {{string .id}} was not named"
[ "$(test_status "$(name_of "$T1")")" = PASSED ] || fail "the create should pass under the noise (its capture holds)"
# Every report format says which variable place failed and why: the
# comparison's rows under the noise are all normal, so nothing else does.
WHY="{{string .id}} must equal id's value"
tool report_compact report
grep -q "TEMPLATE VARIABLES THE ANSWER DID NOT SATISFY" report_compact.log || fail "keploy report does not show the template failures: $(tail -20 report_compact.log)"
grep -qF "body.id: $WHY" report_compact.log || fail "keploy report does not name body.id and why"
grep -q "TEMPLATE_VARIABLE" report_compact.log || fail "keploy report does not show the TEMPLATE_VARIABLE category"
tool report_full report --full
grep -qF "body.id: $WHY" report_full.log || fail "keploy report --full does not name body.id and why: $(tail -20 report_full.log)"
tool report_junit report --format junit
# XML escapes the apostrophe of "id's": the reason is read up to it.
grep -qF "template variable body.id: {{string .id}} must equal id" report_junit.log || fail "keploy report --format junit does not name body.id and why: $(tail -20 report_junit.log)"
tool report_json report --format json
grep -q '"template_failures":\[{"where":"body.id","placeholder":"{{string .id}}"' report_json.log || fail "keploy report --format json does not carry the template failure: $(tail -5 report_json.log)"
# The next step says both things the failure can mean, and never sends the
# user to keploy normalize (which leaves a HIGH-risk failure as it is).
NEXT=$(grep "a template variable's place does not hold what it must" noise.log)
echo "$NEXT" | grep -q "this run's value of id is what" || fail "the template failure's next step does not say where the run's value came from: $NEXT"
echo "$NEXT" | grep -q "re-record" || fail "the template failure's next step does not say what to do when the value changed on purpose: $NEXT"
echo "$NEXT" | grep -q "keploy normalize" && fail "the template failure's next step points at keploy normalize: $NEXT"
# The read-back's own request sent the id: a capture there would accept any
# id and send the wrong one on, so its next step never offers one.
echo "$NEXT" | grep -qF "{{capture" && fail "the read-back's next step offers to make it a capture: $NEXT"
echo "$NEXT" | grep -q "own request sent it" || fail "the read-back's next step does not say its own request sent the id: $NEXT"
# The noise entry also covers the create's capture: its warning says to keep
# the capture, never to replace it with the recorded value.
WARN_CAP=$(grep "a noise entry covers a template variable's place" noise.log | grep -F "{{capture string .id}}" || true)
[ -n "$WARN_CAP" ] || fail "the noise entry over the create's {{capture string .id}} was not named"
echo "$WARN_CAP" | grep -q "keep the capture" || fail "the noise warning over the create's capture does not say to keep it: $WARN_CAP"

echo "== 8. --update-template is ignored: config.yaml keeps the recorded values =="
replay update "" --update-template
expect_rc 0 "step 8, --update-template"
[ "$(set_status)" = PASSED ] || fail "the run with --update-template did not pass (status: $(set_status))"
[ "$(sha256sum keploy/test-set-0/config.yaml | cut -d' ' -f1)" = "$CONFIG_BEFORE" ] || fail "--update-template wrote config.yaml: $(cat keploy/test-set-0/config.yaml)"

echo "== 9. the read-back answers another name: keploy normalize accepts it and keeps the placeholders =="
replay v2 v2
expect_rc 1 "step 9, a changed answer"
[ "$(test_status "$(name_of "$T2")")" = FAILED ] || fail "the read-back's new name did not fail it"
tool normalize normalize
grep -q '"name": *"alice-v2"' "$T2" || fail "keploy normalize did not accept the read-back's new answer: $(grep 'body:' "$T2")"
grep -q 'url: .*/users/{{string .id}}' "$T2" || fail "keploy normalize lost the read-back URL's placeholder"
[ "$(grep -c '{{string .id}}' "$T2")" -ge 2 ] || fail "keploy normalize dropped the read-back body's placeholder: $(grep 'body:' "$T2")"
grep -q '{{capture string .id}}' "$T1" || fail "keploy normalize touched the create's capture"
replay normalized v2
expect_rc 0 "step 9, the run after keploy normalize"
[ "$(set_status)" = PASSED ] || fail "the run after keploy normalize did not pass (status: $(set_status))"

echo "== 9b. templatize refuses an older templatized set and changes nothing =="
cp -a keploy keploy.before9b
rm -rf keploy && cp -a keploy.plain keploy
# The recording as an older keploy templatize wrote it: the logout's redirect
# holds the use, the page cookie beside it is written out, and the call to the
# page sends the key; config.yaml holds the recorded value.
python3 - "$T6" "$T7" keploy/test-set-0/config.yaml <<'OLDER_SET'
import os, re, sys
t6, t7, conf = sys.argv[1:4]
s = open(t6).read()
s = re.sub(r"(?im)^(\s*location:\s*)'?/bye'?\s*$", r"\1/{{string .page}}", s)
open(t6, "w").write(s)
s = open(t7).read()
s = re.sub(r"(?m)^(\s*url: http://127\.0\.0\.1:[0-9]+)/bye\s*$", r"\1/{{string .page}}", s)
s = re.sub(r"(?im)^(\s*cookie:\s*)'?page=bye'?\s*$", r"\1page={{string .page}}", s)
open(t7, "w").write(s)
c = open(conf).read() if os.path.exists(conf) else "" # a recording has none
c = re.sub(r"(?m)^template:.*\n(?:[ \t].*\n)*", "", c)
open(conf, "w").write(c.rstrip("\n") + "\ntemplate:\n    page: bye\n")
OLDER_SET
grep -Eqi "location: '?/\{\{string \.page\}\}'?$" "$T6" || fail "could not write the older set's redirect use: $(grep -i -m1 'location:' "$T6")"
grep -q 'url: .*/{{string .page}}$' "$T7" || fail "could not write the older set's use in the call to the page: $(grep -m1 'url:' "$T7")"
grep -q 'page: bye' keploy/test-set-0/config.yaml || fail "could not write the older set's config.yaml: $(cat keploy/test-set-0/config.yaml)"
OLDER="$(digest)"
tool templatize3 templatize
expect_rc 0 "step 9b, keploy templatize over an older templatized set"
[ "$(digest)" = "$OLDER" ] || fail "keploy templatize changed a set an older keploy templatized"
grep -q 'page=bye' "$T6" || fail "templatize captured the cookie beside the redirect's use: $(grep -i -m1 'set-cookie' "$T6")"
# The line names the set's first placeholder by file and line (the files in
# name order, whichever of the two test cases that is).
HELD=$(grep -o "test-set-0 holds template placeholders already ({{string .page}} at [^ ]* line [0-9]*): keploy templatize writes only into a plain recording, so it changes nothing in this set" templatize3.log | head -n1)
[ -n "$HELD" ] || fail "templatize did not name the older set and its first placeholder: $(tail -5 templatize3.log)"
HELD_FILE=$(echo "$HELD" | sed -E 's/.* at ([^ ]*) line ([0-9]*)\).*/\1/')
HELD_LINE=$(echo "$HELD" | sed -E 's/.* at ([^ ]*) line ([0-9]*)\).*/\2/')
sed -n "${HELD_LINE:-0}p" "keploy/test-set-0/tests/$HELD_FILE" 2>/dev/null | grep -qF '{{string .page}}' \
  || fail "the line templatize names does not hold the placeholder: $HELD"
grep -q "re-record the set and run keploy templatize on the new recording" templatize3.log || fail "templatize did not say what to do with the older set: $(tail -5 templatize3.log)"
replay redirect9b redirect
expect_rc 1 "step 9b, the older set's redirect that fails"
[ "$(test_status "$(name_of "$T6")")" = FAILED ] || fail "the older set's logout redirecting to /oops, its cookie naming it, passed"
rm -rf keploy && mv keploy.before9b keploy

echo "== 9c. a config.yaml that does not parse: templatize changes nothing, on every run =="
cp -a keploy keploy.before9c
rm -rf keploy && cp -a keploy.plain keploy
printf 'template: [unclosed\n' > keploy/test-set-0/config.yaml
BROKEN="$(digest)"
for n in 1 2; do
  tool "templatize4_$n" templatize
  [ "$RC" != 0 ] || fail "run $n of keploy templatize exited 0 though it changed nothing in a set it could not read"
  [ "$(digest)" = "$BROKEN" ] || fail "run $n of keploy templatize changed a set whose config.yaml does not parse"
  grep -q "keploy templatize changed nothing in this set" "templatize4_$n.log" \
    || fail "run $n did not say it changed nothing: $(tail -5 "templatize4_$n.log")"
  grep -q "holds template placeholders already" "templatize4_$n.log" \
    && fail "run $n took the set for one templatize had finished"
  [ -z "$(find keploy/test-set-0 -name '*.keploy-new' -o -name '*.keploy-old' -o -name '*.keploy-part' -o -name '*.keploy-absent')" ] || fail "run $n left files beside the set's own"
done
rm -rf keploy && mv keploy.before9c keploy

echo "== 9d. keploy test refuses a set a killed keploy templatize left half-written =="
cp -a keploy keploy.before9d
rm -rf keploy && cp -a keploy.plain keploy
# The files a templatize stopped right after its first rename leaves: the
# create templatized, the recording of it beside it, every other file's new
# bytes and its file as it was beside it, and config.yaml (which the recording
# has none of) waiting with the marker that says it did not exist.
python3 - keploy.plain/test-set-0 keploy.templated/test-set-0 keploy/test-set-0 "$(basename "$T1")" <<'KILLED_SET'
import os, shutil, sys
plain, done, out, first = sys.argv[1:5]
for name in sorted(os.listdir(os.path.join(done, "tests"))):
    rec, new, at = (os.path.join(d, "tests", name) for d in (plain, done, out))
    shutil.copyfile(rec, at + ".keploy-old")
    shutil.copyfile(new, at if name == first else at + ".keploy-new")
shutil.copyfile(os.path.join(done, "config.yaml"), os.path.join(out, "config.yaml.keploy-new"))
open(os.path.join(out, "config.yaml.keploy-absent"), "w").close()
KILLED_SET
replay killed
expect_rc 1 "step 9d, a set an interrupted templatize left"
[ "$(set_status)" = FAILED ] || fail "the half-written set was not refused (status: $(set_status))"
grep -q "keploy test refused this test set, and ran none of its test cases" killed.log || fail "keploy test did not say it refused the set: $(grep -m3 -i refus killed.log)"
grep -q '^failure_reason:' "$(REPORT)" || fail "the refused set's report gives no failure reason"
# The steps as keploy test logged them (the error of its refusal line).
REASON=$(python3 - killed.log <<'REASON_OF'
import json, re, sys
for line in open(sys.argv[1], errors="replace"):
    if "keploy test refused this test set" in line:
        m = re.search(r'"error": "((?:[^"\\]|\\.)*)"', line)
        if m:
            print(json.loads('"' + m.group(1) + '"'))
            break
REASON_OF
)
echo "$REASON" | grep -q "rename tests/$(basename "$T1").keploy-old to tests/$(basename "$T1")" || fail "the set's failure reason does not give the step for the templatized create: $REASON"
echo "$REASON" | grep -q "config.yaml.keploy-absent" || fail "the set's failure reason does not name config.yaml's marker: $REASON"
grep -q "found no test results" killed.log && fail "keploy test said it found no test results for the refused set"
# Every report format shows the refused set's test cases as failed and
# refused, with the steps: a CI that reads JUnit sees the set fail.
tool report_killed report
grep -q "test set test-set-0: refused when the test set loaded: an interrupted write left its files behind" report_killed.log \
  || fail "keploy report does not show the refused set's reason: $(tail -20 report_killed.log)"
grep -q "No failed tests found" report_killed.log && fail "keploy report says no test failed in the refused set"
tool report_killed_junit report --format junit
grep -q "test case refused when its test set loaded, not sent" report_killed_junit.log || fail "keploy report --format junit does not fail the refused set's test cases: $(grep -m3 testsuite report_killed_junit.log)"
grep -q 'failures="0"' report_killed_junit.log && fail "keploy report --format junit counts no failure in the refused set: $(grep -m3 testsuite report_killed_junit.log)"
tool report_killed_json report --format json
grep -q '"where":"test set test-set-0"' report_killed_json.log || fail "keploy report --format json does not carry the refused set's reason: $(tail -3 report_killed_json.log)"
grep -q '"refused":true' report_killed_json.log || fail "keploy report --format json does not say the refused set's test cases were refused"
# Done as written: each "rename A to B" and each "delete A, B", in order.
python3 - keploy/test-set-0 "$REASON" <<'FOLLOW'
import os, re, sys
root, reason = sys.argv[1], sys.argv[2]
steps = reason.split("(or restore them from version control): ", 1)[1].split("; then run the command again", 1)[0]
for step in steps.split("; "):
    if step.startswith("rename "):
        a, b = step[len("rename "):].split(" to ")
        os.rename(os.path.join(root, a), os.path.join(root, b))
    elif step.startswith("delete "):
        for f in re.sub(r", which that run created$", "", step[len("delete "):]).split(", "):
            os.remove(os.path.join(root, f))
    else:
        sys.exit("a step that is neither a rename nor a deletion: " + step)
FOLLOW
[ "$(cd keploy/test-set-0 && find . -type f | sort | xargs sha256sum)" = "$(cd keploy.plain/test-set-0 && find . -type f | sort | xargs sha256sum)" ] \
  || fail "the steps did not give the recording back: $(cd keploy/test-set-0 && find . -type f | sort)"
tool templatize5 templatize
expect_rc 0 "step 9d, keploy templatize after the steps"
[ "$(digest)" = "$TEMPLATED" ] || fail "keploy templatize after the steps did not write what it wrote at step 3"
rm -rf keploy && mv keploy.before9d keploy

echo "== 9e. a variable in a test case's assertions is refused when the set loads =="
cp -a keploy keploy.before9e
python3 - "$T2" <<'ASSERTION'
import re, sys
t = sys.argv[1]
s = open(t).read()
s = re.sub(r"(?m)^([ \t]*)assertions:[ \t]*\n([ \t]+)noise:", r"\1assertions:\n\2header_equal:\n\2    X-User: '{{string .id}}'\n\2noise:", s, count=1)
open(t, "w").write(s)
ASSERTION
grep -q "X-User: '{{string .id}}'" "$T2" || fail "could not write the variable into the read-back's assertions: $(grep -A4 'assertions:' "$T2")"
replay refused
expect_rc 1 "step 9e, a refused test case"
[ "$(test_status "$(name_of "$T2")")" = FAILED ] || fail "the read-back with a variable in its assertions was not failed"
report_block "$(name_of "$T2")" | grep -q "refused: true" || fail "the read-back's report does not say it was refused: $(report_block "$(name_of "$T2")" | grep -m3 -i 'why\|refused')"
report_block "$(name_of "$T2")" | grep -q "an assertion is read as written" || fail "the read-back's report does not give the refusal's reason"
rm -rf keploy && mv keploy.before9e keploy

echo "== 10. logs: no ERROR where nothing failed, no data race anywhere =="
for log in record templatize templatize2 templatize3 templated update normalize normalized; do
  if grep -q "ERROR" "$log.log"; then fail "ERROR in $log.log: $(grep -m3 ERROR "$log.log")"; fi
done
for log in ./*.log; do
  if grep -q "WARNING: DATA RACE" "$log"; then fail "a data race in $log"; fi
done

if [ "$FAIL" = 0 ]; then
  echo "TEMPLATES E2E: PASSED"
  cd / && sudo rm -rf "$WORK"
else
  echo "TEMPLATES E2E: FAILED ($FAIL) — work directory kept: $WORK"
fi
exit "$FAIL"
