"""The keploy scope protocol v1 client of the mock lanes' test harnesses.

What a test framework's keploy integration does in the proven model's first
release (release1-cut §2.1, §2.2), for the lanes' pytest suites and Python
drivers: claim the run once per test process before any test, then begin and
end each test.

  claim()  POST /agent/instance {key, dir}                 -> {v:1, instance:"run", timeFile}
  begin()  POST /agent/scope/begin {name, file, canSkip}   -> {v:1, status:ok[, action:skip, reason]}
  end()    POST /agent/scope/end {name, file, outcome}     -> {v:1, status:ok[, action:fail, reason]}

No pid, at, suite or dir on a begin or an end: the agent reads the caller
from the connection. A non-2xx answer, an unreadable body, or a missing or
unknown v is an error (AgentError, or Refused for an answer the agent gave a
code), never retried. There is no timeout: the agent answers once the work it
owes is done, and a deadline would let the machine's load decide a test.

An agent without the proven switch has no /agent/instance and answers 404:
claim() then returns LEGACY, and begin and end send the same bodies to the
scope API as it was, whose answers carry no v. LEGACY alone may send a pid
(the per-worker scopes of today's parallel workers, which v1 has no word
for). The flip deletes LEGACY with the switch.

Inert (every call returns at once and sends nothing) when KEPLOY_MOCK_AGENT is
unset or "off", or when KEPLOY_TEST_PID names a process this one runs under:
a process of a test process that claimed the run is a member, and claims
nothing (§2.2 item 2).
"""
import json
import os
import urllib.error
import urllib.request

V = 1
V1 = "v1"
LEGACY = "legacy"
INERT = "inert"

AGENT = (os.environ.get("KEPLOY_MOCK_AGENT") or "").strip().rstrip("/")
TOKEN = os.environ.get("KEPLOY_MOCK_AGENT_TOKEN") or ""


class AgentError(Exception):
    """An answer of the agent the harness cannot trust."""


class Refused(AgentError):
    """A non-2xx answer: its HTTP status, and the wire's error code, message
    and remedy when its body names them."""

    def __init__(self, path, status, code, message, remedy, raw):
        self.path, self.status, self.code = path, status, code
        self.message, self.remedy, self.raw = message, remedy, raw
        text = message or raw
        if remedy:
            text += " (" + remedy + ")"
        super().__init__("keploy: the keploy agent answered %s to %s: %s%s" % (
            status, path, (code + ": ") if code else "", text))


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    # A redirect is an answer like any other non-2xx: following it would send
    # the call again, or somewhere else.
    def redirect_request(self, *a, **k):
        return None


# No proxy from the environment, and no redirect.
_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), _NoRedirect())

_mode = None


def _parent(pid):
    try:
        with open("/proc/%d/stat" % pid, "rb") as f:
            stat = f.read()
    except OSError:
        return 0
    return int(stat[stat.rindex(b")") + 2:].split()[1])


def _member():
    """This process runs under the test process that claimed the run."""
    tp = (os.environ.get("KEPLOY_TEST_PID") or "").strip()
    if not tp or tp == str(os.getpid()):
        return False
    try:
        claimer = int(tp)
    except ValueError:
        return False
    p = os.getppid()
    for _ in range(128):
        if p == claimer:
            return True
        if p <= 1:
            return False
        p = _parent(p)
    return False


def joined():
    return AGENT not in ("", "off") and not _member()


def post(path, body, auth=True):
    """Sends one call. Returns (status, raw body bytes); never raises for a
    non-2xx answer. A transport error raises."""
    if not AGENT.startswith(("http://", "https://")):
        raise AgentError("keploy: KEPLOY_MOCK_AGENT=%r is neither off nor the http URL of a keploy agent" % AGENT)
    h = {"Content-Type": "application/json"}
    if auth and TOKEN:
        h["Authorization"] = "Bearer " + TOKEN
    req = urllib.request.Request(AGENT + path, data=json.dumps(body).encode(), headers=h, method="POST")
    try:
        with _opener.open(req) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def _call(path, body, versioned):
    status, raw = post(path, body)
    text = raw.decode("utf-8", "replace").strip()
    if not 200 <= status <= 299:
        code = message = remedy = ""
        try:
            e = json.loads(raw)
            if isinstance(e, dict):
                code, message, remedy = e.get("error") or "", e.get("message") or "", e.get("remedy") or ""
        except ValueError:
            pass
        raise Refused(path, status, code, message, remedy, text[:512])
    try:
        a = json.loads(raw)
    except ValueError as e:
        raise AgentError("keploy: the keploy agent's answer to %s cannot be read (%s): %r" % (path, e, text[:512]))
    if not isinstance(a, dict):
        raise AgentError("keploy: the keploy agent's answer to %s is not an object: %r" % (path, text[:512]))
    if versioned:
        if "v" not in a:
            raise AgentError("keploy: the keploy agent answered %s with %r, which names no scope protocol version" % (path, text[:512]))
        if a["v"] != V:
            raise AgentError("keploy: the keploy agent answered %s in scope protocol v%s; this harness speaks v%d" % (path, a["v"], V))
    return a


def mode():
    return _mode


def claim(key=".", dir=None):
    """Claims the run for this test process, before any test. Returns V1,
    LEGACY (an agent without the switch) or INERT."""
    global _mode
    if _mode is not None:
        raise AgentError("keploy: this test process claimed keploy's run already; claim once, before the first test")
    if not joined():
        _mode = INERT
        return _mode
    try:
        a = _call("/agent/instance", {"key": key, "dir": dir or os.getcwd()}, versioned=True)
    except Refused as e:
        if e.status != 404:
            raise
        _mode = LEGACY
        return _mode
    if a.get("instance") != "run":
        raise AgentError("keploy: the keploy agent answered the claim with the instance %r, not the run" % a.get("instance"))
    # What this process starts runs on the run's clock, and is a member.
    if a.get("timeFile"):
        os.environ["KEPLOY_TIME_FILE"] = a["timeFile"]
    os.environ["KEPLOY_TEST_PID"] = str(os.getpid())
    _mode = V1
    return _mode


def _body(name, file, project, pid):
    b = {"name": name}
    if file:
        b["file"] = file
    if project:
        b["project"] = project
    if pid is not None:
        if _mode != LEGACY:
            raise AgentError("keploy: a pid is not sent on scope protocol v1; only the scope API without the proven switch takes one")
        b["pid"] = pid
    return b


def _ready():
    if _mode is None:
        raise AgentError("keploy: claim the run (keploy_v1.claim()) before the first test")
    return _mode != INERT


def begin(name, file=None, project=None, can_skip=True, pid=None):
    """Opens a test. Returns the reason to skip it with, or None to run it."""
    if not _ready():
        return None
    b = _body(name, file, project, pid)
    if can_skip:
        b["canSkip"] = True
    a = _call("/agent/scope/begin", b, versioned=_mode == V1)
    if a.get("status") != "ok":
        raise AgentError("keploy: the keploy agent answered the begin of %s with status %r" % (name, a.get("status")))
    action = a.get("action") or ""
    if action == "skip":
        return a.get("reason") or "keploy: not run in this replay"
    if action:
        raise AgentError("keploy: the keploy agent answered the begin of %s with the unknown action %r" % (name, action))
    return None


def end(name, outcome, file=None, project=None, pid=None):
    """Closes a test with its outcome. Returns keploy's reason to fail it,
    or None."""
    if not _ready():
        return None
    b = _body(name, file, project, pid)
    b["outcome"] = outcome
    a = _call("/agent/scope/end", b, versioned=_mode == V1)
    if a.get("status") != "ok":
        raise AgentError("keploy: the keploy agent answered the end of %s with status %r" % (name, a.get("status")))
    action = a.get("action") or ""
    if action == "fail":
        return a.get("reason") or "keploy: failed by keploy"
    if action:
        raise AgentError("keploy: the keploy agent answered the end of %s with the unknown action %r" % (name, action))
    return None
