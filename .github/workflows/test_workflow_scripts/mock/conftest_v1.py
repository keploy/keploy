"""The keploy pytest harness of the mock lanes: copied into a lane's work
directory as conftest.py, beside keploy_v1.py, which speaks the wire.

The test process claims the run before pytest collects anything: module code
a collection runs is the test process's code too (release1-cut §2.2 item 2).
Each test is begun by the outermost autouse fixture, and ended once its
teardown has reported, with the worst of its setup, call and teardown (an
expected failure proves nothing). A test the replay says not to run is
skipped with keploy's reason and sends no end; a test keploy fails at its end
fails at its teardown, with keploy's text. Any answer the harness cannot
trust fails the test, or, for the claim, the run.

KEPLOY_E2E_NO_SKIP plays a harness that predates skipping: its begin does not
say it can skip, which only the scope API without the proven switch takes.
KEPLOY_E2E_NO_PID plays a harness that sends no pid on that scope API (the
optional single shared pool): its begin and end narrow the one pid-less
scope instead of this worker's. Under the switch no call carries a pid either
way.
"""
import os

import pytest

import keploy_v1 as k

# The pid begin and end send: this worker's by default, none with
# KEPLOY_E2E_NO_PID.
_PID = None if os.environ.get("KEPLOY_E2E_NO_PID") else k.WORKER


def pytest_sessionstart(session):
    try:
        k.claim(".", os.getcwd())
    except k.AgentError as e:
        pytest.exit(str(e), returncode=1)


@pytest.fixture(autouse=True)
def keploy_scope(request):
    reason = k.begin(request.node.name, request.node.path.name,
                     can_skip=not os.environ.get("KEPLOY_E2E_NO_SKIP"), pid=_PID)
    if reason is not None:
        # Told not to run this test: skip, and send no end for it.
        request.node._keploy_gated = True
        pytest.skip(reason)
    yield


_RANK = {"passed": 0, "skipped": 1, "failed": 2}


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item, call):
    rep = (yield).get_result()
    if getattr(item, "_keploy_gated", False):
        return
    o = "skipped" if hasattr(rep, "wasxfail") else rep.outcome
    worst = getattr(item, "_keploy_worst", None)
    if worst is None or _RANK[o] > _RANK[worst]:
        item._keploy_worst = o
    if rep.when != "teardown":
        return
    try:
        reason = k.end(item.name, item._keploy_worst, file=item.path.name, pid=_PID)
    except k.AgentError as e:
        reason = str(e)
    if reason is not None:
        rep.outcome = "failed"
        rep.longrepr = reason
