#!/usr/bin/env python3
"""Enforce that inlined copies of a shared script region stay byte-identical.

Some workflow steps have to run BEFORE actions/checkout, so they cannot call a
script out of the repository - the repository is not on disk yet. Those steps
inline the body instead, which means the same code exists in several places at
once and can drift apart silently. Drift between such copies is exactly the
class of bug this check exists to prevent, so the invariant is enforced here
rather than left to a comment asking people to keep the copies in sync.

The check parses every YAML file outside SKIP_DIRS once (workflows under
.github/workflows, composite actions under .github/actions/*/action.yml, and
anything else that happens to describe steps) and walks both step containers
GitHub understands: `jobs.<id>.steps` in a workflow and `runs.steps` in a
composite action. Everything below is driven by that one structural walk.

WHAT THIS CHECK IS AND IS NOT. It catches ACCIDENTAL divergence - the failure
that actually happened here, where one copy was hardened and the others were
silently left behind. It is not a sandbox and does not pretend to stop someone
who sets out to defeat it: a body assembled from `env:` or `strategy.matrix`
and run via `Invoke-Expression`, a payload in a committed helper script, a
copy under a SKIP_DIR, `continue-on-error` on the checking step, or narrowing
the host workflow's triggers all pass. Those are visible in review, which is
where they belong; adding regexes until the check "cannot" be evaded would
only buy a comment claiming a guarantee the code does not provide. See the
"fingerprints" entry in REGIONS for the exact detection boundary.

For each region described in REGIONS below the check:

  * extracts the region between the BEGIN/END markers of the source script;
  * NAME NET - collects every step named `step_name`, compares each inlined
    body (the YAML parser hands back the block scalar already dedented)
    byte-for-byte with the region, and prints a unified diff naming the file,
    job and step when it differs;
  * requires that the inlined body is indented by exactly INDENT spaces in the
    file, so the source script's description of the layout stays true;
  * requires that the set of call sites found equals the registered set, so a
    NEW copy cannot be added silently: an unknown copy fails until it is made
    identical AND registered here, and a registered copy that disappears fails
    too;
  * CONTENT NET - flags any OTHER `run:` body in the repository that carries
    the region's fingerprint (see the "fingerprints" entry in REGIONS below,
    which documents the choice and its false-positive/false-negative profile).
    The name net alone is defeated by the obvious dodge of copying the body
    under a different step name; the content net does not care what the step
    is called;
  * for a YAML file that fails to parse, falls back to a line-based scan for
    both nets and reports the parse failure as a loud warning, so a broken
    file is never a silent hole in the check.

It also checks its own wiring: some job under .github/workflows must actually
run this script in verify mode, otherwise deleting that job would disable
every guarantee above while this script still reported success locally.

Usage:
    python3 .github/scripts/check-inline-regions.py           # verify
    python3 .github/scripts/check-inline-regions.py --fix     # re-sync, then verify

--fix rewrites each REGISTERED call site's body from the source script, using
the same structural walk as the verify path, so it touches exactly the (file,
job, step) triples that are registered and nothing else. CI runs the read-only
form.
"""

from __future__ import annotations

import argparse
import difflib
import re
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - the CI job installs PyYAML
    sys.exit("check-inline-regions: PyYAML is required (pip install pyyaml)")

REPO_ROOT = Path(__file__).resolve().parents[2]
WORKFLOW_DIR = REPO_ROOT / ".github" / "workflows"
SELF = Path(__file__).resolve().relative_to(REPO_ROOT)
SELF_POSIX = SELF.as_posix()

# Indentation of an inlined `run: |` body: the `run:` key sits at 8 spaces in
# these workflows, so its block scalar content sits at 10.
INDENT = 10

REGIONS = [
    {
        "id": "recover-stale-workspace",
        "source": ".github/scripts/windows/recover-stale-workspace.ps1",
        "begin": "# ===== BEGIN INLINE REGION",
        "end": "# ===== END INLINE REGION",
        # The step name that identifies a call site.
        "step_name": "Recover stale runner state",
        # Every known call site, as (repo-relative YAML path, container id).
        # The container id is the job id for a workflow step and the literal
        # "runs" for a composite action's `runs.steps`. A site found in the
        # repository but missing here is reported as an unknown copy; a site
        # listed here but absent from the repository is reported missing.
        "sites": [
            (".github/workflows/golang_docker_windows.yml", "golang_docker_windows"),
            (".github/workflows/golang_native_windows.yml", "golang_native_windows"),
            (".github/workflows/prepare_and_run.yml", "build-windows-amd64"),
            (".github/workflows/prepare_and_run.yml", "precheck-windows"),
            (".github/workflows/prepare_and_run.yml", "cleanup_windows"),
        ],
        # CONTENT NET. The name net only sees steps called `step_name`, so on
        # its own it is defeated by copying the body under any other name -
        # which is precisely how the original machine-wide form would come
        # back. These fingerprints are matched against EVERY `run:` body in
        # the repository that is not one of the registered call sites above.
        #
        # Choosing the fingerprint is the whole game, so the reasoning is
        # written down rather than left implicit:
        #
        #  * "machine-wide-hazard-pair" is the semantic core of the region and
        #    the primary net. The two hazards this region exists to contain
        #    are (a) killing a keploy process and (b) touching the SINGLE
        #    machine-wide WinDivert kernel-driver service; a copy that drops
        #    either one is no longer the thing being guarded. It is also the
        #    only fingerprint that matches the PRE-repair machine-wide body
        #        Get-Process -Name 'keploy' | Stop-Process -Force
        #        foreach ($n in 'WinDivert','WinDivert64') { Stop-Service $n -Force }
        #    which carries none of this region's log strings - i.e. it catches
        #    a regression to the exact code this change removed.
        #  * "region-log-string" is a cheap independent net for a copy that
        #    was derived from the CURRENT region and then edited: these
        #    strings are load-bearing job output, so an editor pruning code
        #    keeps them. It costs nothing and catches half-copies that dropped
        #    one arm of the pair.
        #
        # False positives: a genuinely new step that both kills a keploy
        # process and stops/starts the WinDivert service without being a copy
        # of this region. That step is, by construction, doing the
        # machine-wide thing this whole change exists to prevent on a shared
        # VM, so being asked to justify it in "exempt" below is the correct
        # outcome rather than an annoyance. Steps that do only one of the two
        # (kill keploy, or manage WinDivert) do not match.
        # False negatives: a copy rewritten to use neither the keploy kill nor
        # the WinDivert service AND stripped of every log string - i.e. no
        # longer this region. A copy that renames the driver handling to
        # `sc.exe stop WinDivert` or `Restart-Service` still matches; one that
        # shells out to a non-obvious helper would not, and nothing short of
        # semantic analysis would catch that.
        "fingerprints": [
            {
                "id": "machine-wide-hazard-pair",
                "what": "kills a keploy process AND touches the machine-wide "
                        "WinDivert driver service",
                "all": [
                    r"(?i)\bkeploy",
                    r"(?i)\b(?:stop-process|taskkill)\b",
                    r"(?i)\bwindivert",
                    r"(?i)(?:\b(?:stop|start|restart|set|get)-service\b"
                    r"|\bsc(?:\.exe)?\s+(?:stop|start|delete|config)\b)",
                ],
            },
            {
                "id": "region-log-string",
                "what": "carries a log string that only this region prints",
                "any": [
                    "Recovering stale runner state for workspace",
                    "refusing to fall back to machine-wide cleanup",
                    "Skipping WinDivert driver reset",
                    "Stopping this workspace's leftover keploy",
                ],
            },
        ],
        # Steps the content net must not flag, as (path, container id, step
        # name). Deliberately empty: nothing in this repository legitimately
        # kills keploy and resets the WinDivert service outside the registered
        # call sites. Adding an entry here is a decision to be reviewed, which
        # is the point.
        "exempt": [],
        # Substrings the SOURCE region must contain when it carries the
        # machine-wide hazard pair. Without this the check enforces only
        # CONSISTENCY, never SAFETY: rewriting the region in the .ps1 back to
        # an unscoped machine-wide body and running `--fix` would push that
        # body into all five call sites and still report OK, silently undoing
        # the very regression this region exists to hold. That is not an
        # adversarial scenario - it is one careless edit to the source file.
        #
        # Each marker is load-bearing scoping, not decoration:
        #   wsPrefix / StartsWith( -> the kill is restricted to processes
        #     under THIS job's $GITHUB_WORKSPACE
        #   "refusing to fall back" -> the no-workspace path kills nothing
        #     rather than degrading to machine-wide
        "safety_markers": [
            "wsPrefix",
            "StartsWith(",
            "refusing to fall back to machine-wide cleanup",
        ],
    },
]

# Matches a YAML step-name line, e.g. `      - name: Some step`, quoted or
# not, with or without a trailing comment. Used ONLY as a fallback for files
# the YAML parser could not read - everything else goes through the structural
# walk. Anchored to the `name:` key so prose that merely mentions a step name
# in a comment does not match.
STEP_NAME_LINE = re.compile(
    r"""^\s*-?\s*name:\s*
        (?:  '(?P<sq>(?:[^']|'')*)'
          |  "(?P<dq>(?:[^"\\]|\\.)*)"
          |  (?P<plain>[^\n]*?)
        )
        (?:\s+\#.*)?\s*$
    """,
    re.VERBOSE,
)

# A `run:` whose value is a literal block scalar. A trailing comment after the
# `|` is legal YAML, so allow it.
RUN_BLOCK_LINE = re.compile(r"^\s*run:\s*\|\s*(?:#.*)?$")

SKIP_DIRS = {".git", "node_modules", "vendor", ".venv", "venv", "dist", "build"}


class LayoutError(Exception):
    """A call site is not shaped the way this check can read or rewrite."""


class ParsedFile:
    """A YAML file in the repository, parsed once and reused by every pass."""

    __slots__ = ("path", "rel", "text", "lines", "docs", "error")

    def __init__(self, path: Path):
        self.path = path
        self.rel = path.relative_to(REPO_ROOT).as_posix()
        self.docs: list = []
        self.error = None
        try:
            self.text = path.read_text(errors="replace")
        except OSError as exc:  # unreadable file: treat like a parse failure
            self.text = ""
            self.error = f"could not be read: {exc}"
        else:
            try:
                # compose_all, not safe_load: the node tree carries source
                # marks, which the layout check and --fix need, and it handles
                # multi-document files.
                self.docs = [d for d in yaml.compose_all(self.text) if d is not None]
            except yaml.YAMLError as exc:
                self.error = f"is not valid YAML: {exc}"
        self.lines = self.text.split("\n")

    @property
    def is_workflow(self) -> bool:
        return self.path.parent == WORKFLOW_DIR


class Site:
    """One step found by the structural walk."""

    __slots__ = ("file", "kind", "container", "index", "node", "name", "run")

    def __init__(self, file: ParsedFile, kind, container, index, node, name, run):
        self.file = file
        self.kind = kind            # "job" (workflow) or "runs" (composite action)
        self.container = container  # job id, or "runs"
        self.index = index          # position in the step list
        self.node = node            # the step's MappingNode
        self.name = name            # the step's `name:`, or None
        self.run = run              # the step's `run:` ScalarNode, or None

    @property
    def key(self) -> tuple:
        return (self.file.rel, self.container)

    def where(self, step_name=None) -> str:
        name = step_name if step_name is not None else self.name
        if self.kind == "job":
            place = f"job {self.container!r}"
        else:
            place = f"runs.steps[{self.index}]"
        named = f" step {name!r}" if name else f" step #{self.index}"
        return f"{self.file.rel}: {place}{named}"


# --------------------------------------------------------------------------
# YAML node helpers. yaml.compose* returns nodes rather than Python values, so
# these do the little bit of walking the check needs while keeping the marks.
# --------------------------------------------------------------------------

def _map_items(node) -> list:
    if not isinstance(node, yaml.MappingNode):
        return []
    return [(k.value, v) for k, v in node.value if isinstance(k, yaml.ScalarNode)]


def _map_get(node, key):
    for k, v in _map_items(node):
        if k == key:
            return v
    return None


def _seq_items(node) -> list:
    return list(node.value) if isinstance(node, yaml.SequenceNode) else []


def _scalar(node):
    return node.value if isinstance(node, yaml.ScalarNode) else None


def iter_steps(file: ParsedFile):
    """Yield every step in a file, from both step containers GitHub reads.

    `jobs.<id>.steps` is a workflow; `runs.steps` is a composite action
    (.github/actions/*/action.yml). Walking both is what stops a copy from
    hiding in an action instead of a workflow.
    """
    for root in file.docs:
        if not isinstance(root, yaml.MappingNode):
            continue
        jobs = _map_get(root, "jobs")
        for job_id, job in _map_items(jobs):
            for i, step in enumerate(_seq_items(_map_get(job, "steps"))):
                if isinstance(step, yaml.MappingNode):
                    yield _make_site(file, "job", job_id, i, step)
        runs = _map_get(root, "runs")
        if runs is not None:
            for i, step in enumerate(_seq_items(_map_get(runs, "steps"))):
                if isinstance(step, yaml.MappingNode):
                    yield _make_site(file, "runs", "runs", i, step)


def _make_site(file, kind, container, index, node) -> Site:
    name_node = _map_get(node, "name")
    run_node = _map_get(node, "run")
    return Site(
        file, kind, container, index, node,
        _scalar(name_node),
        run_node if isinstance(run_node, yaml.ScalarNode) else None,
    )


# --------------------------------------------------------------------------
# File discovery
# --------------------------------------------------------------------------

def all_yaml_files() -> list:
    """Every YAML file in the repository, sorted for stable output."""
    out = []
    for path in REPO_ROOT.rglob("*.y*ml"):
        if not path.is_file() or path.suffix not in (".yml", ".yaml"):
            continue
        if any(part in SKIP_DIRS for part in path.relative_to(REPO_ROOT).parts):
            continue
        out.append(path)
    return sorted(out)


def parse_repo(warnings: list) -> list:
    """Parse every YAML file once. Parse failures become loud warnings."""
    if not WORKFLOW_DIR.is_dir():
        raise SystemExit(f"check-inline-regions: missing {WORKFLOW_DIR}")
    files = [ParsedFile(p) for p in all_yaml_files()]
    for f in files:
        if f.error:
            warnings.append(
                f"{f.rel} {f.error}\n"
                f"    This check could not walk it structurally, so it fell back "
                f"to a line-based scan of that file - which is weaker. Fix the "
                f"file, or an inlined copy could hide in it."
            )
    return files


# --------------------------------------------------------------------------
# Region extraction and layout
# --------------------------------------------------------------------------

def extract_region(region: dict) -> str:
    """Return the text between the BEGIN/END markers of the source script."""
    path = REPO_ROOT / region["source"]
    if not path.exists():
        raise SystemExit(f"check-inline-regions: missing source script {path}")
    lines = path.read_text().splitlines()
    begins = [i for i, l in enumerate(lines) if l.startswith(region["begin"])]
    ends = [i for i, l in enumerate(lines) if l.startswith(region["end"])]
    if len(begins) != 1 or len(ends) != 1 or ends[0] <= begins[0]:
        raise SystemExit(
            f"check-inline-regions: {region['source']} must contain exactly one "
            f"{region['begin']!r} line followed by exactly one {region['end']!r} "
            f"line (found {len(begins)} and {len(ends)})"
        )
    return "\n".join(lines[begins[0] + 1:ends[0]]) + "\n"


def indent_of(line: str) -> int:
    return len(line) - len(line.lstrip(" "))


def locate_run_body(lines: list, run_line: int, where: str) -> tuple:
    """Return (start, end) line indices bounding a `run: |` block scalar body.

    `run_line` is the index of the `run: |` line itself, taken from the YAML
    node's source mark, so this never has to guess which step it is looking at.
    """
    if run_line >= len(lines) or not RUN_BLOCK_LINE.match(lines[run_line]):
        raise LayoutError(
            f"{where}: the `run:` value at line {run_line + 1} is not a `run: |` "
            f"literal block scalar; an inlined region must be one so it can be "
            f"compared and rewritten byte-for-byte"
        )
    run_indent = indent_of(lines[run_line])
    end = run_line + 1
    while end < len(lines) and (
        lines[end].strip() == "" or indent_of(lines[end]) > run_indent
    ):
        end += 1
    # Trailing blank lines separate this step from the next one.
    while end > run_line + 1 and lines[end - 1].strip() == "":
        end -= 1
    return run_line + 1, end


# --------------------------------------------------------------------------
# The two nets
# --------------------------------------------------------------------------

def collect_sites(region: dict, files: list, errors: list) -> dict:
    """NAME NET: every step named `step_name`, keyed by (file, container)."""
    step_name = region["step_name"]
    found = {}
    for f in files:
        for site in iter_steps(f):
            if site.name != step_name:
                continue
            if site.key in found:
                errors.append(
                    f"{f.rel}: {site.where()} appears more than once in the same "
                    f"{'job' if site.kind == 'job' else 'step list'}; this check "
                    f"identifies a call site by (file, job) and cannot tell the "
                    f"copies apart"
                )
                continue
            found[site.key] = site
    return found


def fingerprint_hits(region: dict, body: str) -> list:
    """Which of the region's fingerprints this text carries."""
    hits = []
    for fp in region.get("fingerprints", []):
        if any(not re.search(rx, body) for rx in fp.get("all", [])):
            continue
        anys = fp.get("any", [])
        if anys and not any(s in body for s in anys):
            continue
        hits.append(fp)
    return hits


def content_net(region: dict, files: list, errors: list) -> None:
    """CONTENT NET: a fingerprinted `run:` body that is not a call site fails.

    The name net cannot see a copy that was renamed, which is the cheapest
    possible evasion. This pass ignores names entirely: it looks at what the
    body DOES.
    """
    step_name = region["step_name"]
    exempt = {(p, c, n) for p, c, n in region.get("exempt", [])}
    for f in files:
        if f.error:
            continue  # handled by the line-based fallback
        for site in iter_steps(f):
            if site.run is None:
                continue
            body = site.run.value
            if not isinstance(body, str):
                continue
            if site.name == step_name:
                # The name net owns this step - registered or not - and has
                # already compared it byte-for-byte or reported it unknown.
                continue
            if (f.rel, site.container, site.name) in exempt:
                continue
            hits = fingerprint_hits(region, body)
            if not hits:
                continue
            what = "; ".join(f"{h['id']} ({h['what']})" for h in hits)
            errors.append(
                f"{site.where()} has a `run:` body carrying the fingerprint of "
                f"the INLINE REGION in {region['source']} [{what}], but it is "
                f"not a registered call site. A copy of that region - under any "
                f"step name - must be named {step_name!r}, be byte-identical, "
                f"and be registered in REGIONS in {SELF_POSIX}. If this step is "
                f"genuinely not a copy, it still kills keploy processes and/or "
                f"resets the machine-wide WinDivert driver on a VM shared by "
                f"four runner installs: justify it and add it to that region's "
                f"\"exempt\" list."
            )


def fallback_scan(region: dict, files: list, errors: list) -> None:
    """Line-based net for files the YAML parser could not read.

    Weaker than the structural walk by construction - it cannot tell which job
    or step a line belongs to, and it fingerprints the whole file rather than
    one `run:` body - but a file that fails to parse must not become a hole.
    """
    step_name = region["step_name"]
    for f in files:
        if not f.error:
            continue
        for lineno, line in enumerate(f.lines, 1):
            m = STEP_NAME_LINE.match(line)
            if not m:
                continue
            name = m.group("sq")
            if name is not None:
                name = name.replace("''", "'")
            elif m.group("dq") is not None:
                name = m.group("dq")
            else:
                name = (m.group("plain") or "").strip()
            if name == step_name:
                errors.append(
                    f"{f.rel}:{lineno}: a step named {step_name!r} lives in a "
                    f"file this check could not parse ({f.error.split(':')[0]}), "
                    f"so it cannot be kept in sync with {region['source']}. Fix "
                    f"the file so it parses, or delete the copy."
                )
        hits = fingerprint_hits(region, f.text)
        if hits:
            what = "; ".join(f"{h['id']} ({h['what']})" for h in hits)
            errors.append(
                f"{f.rel}: this file could not be parsed and carries the "
                f"fingerprint of the INLINE REGION in {region['source']} "
                f"[{what}]. Fix the file so this check can walk its steps."
            )


# --------------------------------------------------------------------------
# Per-region verification
# --------------------------------------------------------------------------

def check_region_safety(region: dict, text: str) -> list:
    """Fail if the SOURCE region itself lost its job-scoping.

    Every other check here compares the copies against this text, so the text
    is the one thing nothing else can vouch for. If it regains the unscoped
    machine-wide shape, `--fix` will faithfully propagate that to every call
    site and the byte-equality check will pass - consistency preserved, safety
    gone. Checking the source closes that loop.
    """
    markers = region.get("safety_markers") or []
    if not markers:
        return []
    hits = fingerprint_hits(region, text)
    if "machine-wide-hazard-pair" not in {h["id"] for h in hits}:
        # The region no longer kills a process AND touches the driver service,
        # so there is no machine-wide hazard left to scope.
        return []
    missing = [m for m in markers if m not in text]
    if not missing:
        return []
    return [
        f"the INLINE REGION in {region['source']} kills a keploy process and "
        f"touches the machine-wide WinDivert driver service, but has lost the "
        f"job-scoping marker(s): {', '.join(repr(m) for m in missing)}. On the "
        f"shared Windows VM an unscoped body kills a SIBLING runner's job and "
        f"stops a kernel driver another job is capturing through - the exact "
        f"regression this region exists to prevent. Restore the scoping (see "
        f"the header of {region['source']}); do NOT run --fix, which would "
        f"copy the unscoped body into every call site."
    ]


def check_region(region: dict, files: list) -> list:
    errors: list = []
    step_name = region["step_name"]
    text = extract_region(region)
    # Before comparing anything against the region, establish that the region
    # itself is still safe to propagate.
    errors.extend(check_region_safety(region, text))
    expected = {(f, j) for f, j in region["sites"]}
    found = collect_sites(region, files, errors)

    # 1. Unknown / missing call sites. An unknown copy is the failure mode that
    #    matters most: it is how a new divergent copy would otherwise land.
    for key in sorted(found.keys() - expected):
        site = found[key]
        errors.append(
            f"{site.where()} is a step named {step_name!r} that this check does "
            f"not know about. Every copy of the region in {region['source']} "
            f"must be registered in REGIONS in {SELF_POSIX} so it is kept in "
            f"sync - add it there and make it byte-identical, or delete the copy."
        )
    for rel, container in sorted(expected - found.keys()):
        errors.append(
            f"{rel}: expected a step named {step_name!r} in {container!r}, but "
            f"none was found. If the call site was removed on purpose, drop it "
            f"from REGIONS in {SELF_POSIX} and from the site list in the header "
            f"of {region['source']}."
        )

    # 2. Byte-equality of every registered call site.
    for key in sorted(expected & found.keys()):
        site = found[key]
        body = site.run.value if site.run is not None else None
        if not isinstance(body, str):
            errors.append(f"{site.where()} has no `run:` body")
            continue
        if body == text:
            continue
        diff = "".join(difflib.unified_diff(
            text.splitlines(keepends=True),
            body.splitlines(keepends=True),
            fromfile=f"{region['source']} (INLINE REGION)",
            tofile=site.where(),
            n=2,
        ))
        errors.append(
            f"{site.where()} has drifted from the INLINE REGION in "
            f"{region['source']}:\n\n{diff.rstrip()}\n\n"
            f"    Re-sync with: python3 {SELF_POSIX} --fix"
        )

    # 3. Indentation, so the source script's "indented by exactly ten spaces"
    #    description of the layout stays true.
    for key in sorted(expected & found.keys()):
        site = found[key]
        if site.run is None:
            continue
        try:
            start, end = locate_run_body(
                site.file.lines, site.run.start_mark.line, site.where()
            )
        except LayoutError as exc:
            errors.append(str(exc))
            continue
        content = [l for l in site.file.lines[start:end] if l.strip()]
        if content and min(indent_of(l) for l in content) != INDENT:
            errors.append(
                f"{site.where()}: the body is indented by "
                f"{min(indent_of(l) for l in content)} spaces; the region must "
                f"be inlined at exactly {INDENT}. Re-sync with: "
                f"python3 {SELF_POSIX} --fix"
            )

    # 4. Content net and the fallback for unparseable files.
    content_net(region, files, errors)
    fallback_scan(region, files, errors)

    return errors


def check_self_wiring(files: list) -> list:
    """This check must itself be wired into CI, or it guards nothing.

    Everything above is enforced only if some job actually runs this script on
    pull requests. Without this, deleting the `inline-regions` job silently
    disables the whole mechanism while the script keeps passing locally.
    """
    verifiers = []
    fixers = []
    for f in files:
        if not f.is_workflow or f.error:
            continue
        for site in iter_steps(f):
            if site.run is None or SELF.name not in site.run.value:
                continue
            for raw in site.run.value.split("\n"):
                # Drop shell comments so a line merely MENTIONING this script
                # cannot pass for wiring. Erring towards "not wired" is right:
                # the cost is a false failure, not a silent disarm.
                line = raw.split("#", 1)[0]
                if SELF.name not in line:
                    continue
                if "--fix" in line:
                    fixers.append(site.where())
                else:
                    verifiers.append(site.where())
    if verifiers:
        return []
    detail = ""
    if fixers:
        detail = (
            f" The only invocation(s) found pass --fix ({', '.join(sorted(set(fixers)))}), "
            f"which rewrites the call sites instead of failing on drift, so it "
            f"cannot be the CI gate."
        )
    return [
        f"no job under .github/workflows runs `{SELF_POSIX}` in verify mode, so "
        f"nothing enforces any of the above on a pull request.{detail} Restore "
        f"the `inline-regions` job in .github/workflows/golangci-lint.yml (or "
        f"add an equivalent job that runs `python3 {SELF_POSIX}`)."
    ]


# --------------------------------------------------------------------------
# --fix
# --------------------------------------------------------------------------

def fix_region(region: dict, files: list) -> list:
    """Rewrite every REGISTERED call site's body from the source region.

    Driven by the same structural walk as the verify path, so it rewrites the
    registered (file, job) pairs and nothing else - not, as an earlier version
    did, every same-named step in a file that happens to contain one.
    """
    step_name = region["step_name"]
    text = extract_region(region)
    # Refuse to propagate an unsafe region. --fix exists to re-sync copies
    # after an intentional edit to the source; if that edit dropped the
    # job-scoping, fixing would push the machine-wide body into all five
    # call sites at once - turning a one-file mistake into a fleet-wide one.
    unsafe = check_region_safety(region, text)
    if unsafe:
        return unsafe
    new_body = [
        (INDENT * " " + l) if l.strip() else ""
        for l in text.rstrip("\n").split("\n")
    ]
    expected = {(f, j) for f, j in region["sites"]}
    errors: list = []
    found = collect_sites(region, files, errors)
    if errors:
        return errors

    by_file: dict = {}
    for key in sorted(expected & found.keys()):
        site = found[key]
        if site.run is None:
            errors.append(f"{site.where()} has no `run:` body to rewrite")
            continue
        by_file.setdefault(site.file, []).append(site)
    for rel, container in sorted(expected - found.keys()):
        print(f"[{region['id']}] {rel}: no {step_name!r} step in {container!r}, "
              f"nothing to re-sync there")

    for f, sites in sorted(by_file.items(), key=lambda kv: kv[0].rel):
        lines = list(f.lines)
        done = 0
        # Rewrite from the bottom up so earlier line numbers stay valid.
        for site in sorted(sites, key=lambda s: s.run.start_mark.line, reverse=True):
            try:
                start, end = locate_run_body(
                    lines, site.run.start_mark.line, site.where()
                )
            except LayoutError as exc:
                errors.append(str(exc))
                continue
            lines[start:end] = new_body
            done += 1
        if not done:
            print(f"[{region['id']}] {f.rel}: nothing re-synced")
            continue
        f.path.write_text("\n".join(lines))
        print(f"[{region['id']}] re-synced {done} of {len(sites)} call site(s) "
              f"in {f.rel}")
    return errors


# --------------------------------------------------------------------------

def main() -> int:
    parser = argparse.ArgumentParser(
        description="Check that inlined copies of a shared script region match it."
    )
    parser.add_argument(
        "--fix",
        action="store_true",
        help="rewrite each registered call site from the source region, then verify",
    )
    args = parser.parse_args()

    # A vacuous configuration must not print a confident success message. With
    # REGIONS emptied, every loop below is a no-op and the summary would claim
    # guarantees nothing evaluated.
    if not REGIONS:
        print(
            "check-inline-regions: FAILED\n\n"
            "  * REGIONS is empty, so this check guards nothing. Restore the "
            "region entry it is supposed to enforce.\n"
        )
        return 1

    if args.fix:
        fix_errors: list = []
        fix_warnings: list = []
        files = parse_repo(fix_warnings)
        for region in REGIONS:
            fix_errors.extend(fix_region(region, files))
        if fix_errors:
            # Only report warnings here if the run stops: otherwise the verify
            # pass below prints them once, rather than twice per run.
            report_warnings(fix_warnings)
            return report("could not re-sync every registered call site", fix_errors)
        print()

    warnings = []
    files = parse_repo(warnings)
    report_warnings(warnings)

    errors: list = []
    for region in REGIONS:
        errors.extend(check_region(region, files))
    errors.extend(check_self_wiring(files))

    if errors:
        return report(
            "An inlined region must stay byte-identical to its source script. "
            "These steps run before actions/checkout, so they cannot call the "
            "script out of the repository; the copies are the only mechanism "
            "available, and this check is what keeps them honest.",
            errors,
        )

    total = sum(len(r["sites"]) for r in REGIONS)
    print(
        f"check-inline-regions: OK - {total} inlined call site(s) across "
        f"{len(REGIONS)} region(s) are byte-identical to their source scripts, "
        f"no unregistered copy carries a region's fingerprint, and this check "
        f"is wired into a workflow job"
    )
    return 0


def report_warnings(warnings: list) -> None:
    if not warnings:
        return
    print("check-inline-regions: WARNINGS\n", file=sys.stderr)
    for warn in warnings:
        # GitHub renders this as a job annotation; the plain line keeps it
        # readable in a local run.
        print(f"::warning::check-inline-regions: {warn.splitlines()[0]}")
        print(f"  ! {warn}\n", file=sys.stderr)


def report(epilogue: str, errors: list) -> int:
    print("check-inline-regions: FAILED\n")
    for err in errors:
        print(f"  * {err}\n")
    print(epilogue)
    return 1


if __name__ == "__main__":
    sys.exit(main())
