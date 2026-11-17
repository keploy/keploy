#!/usr/bin/env bash
# E2E: the name `keploy record` gives an app it was given no name for (no
# --app-name, no appName in keploy.yml), with the real binary under sudo, in
# real git checkouts whose folder is not named as their origin remote:
#   1. at the top folder of a checkout of shop (origin
#      https://github.com/acme/shop.git) with no keploy.yml: "shop", the last
#      segment of the remote's URL without .git, not the checkout's folder;
#   2. in a folder mktemp made inside that checkout, with no keploy.yml:
#      "shop-tmp.XXXXXXXXXX", the repository's name and the folder's path
#      inside it. keploy named both after the folder alone, so every run from
#      a mktemp folder made an app "tmp.XXXXXXXXXX";
#   3. outside any repository, with no keploy.yml: the folder's name, as
#      before;
#   4. an existing project: a keploy.yml that names no app, in the folder
#      services/orders of a checkout. The app keeps the name every run gave
#      it, "orders", not "shop-services-orders": keploy writes
#      `appName: "orders"` into that keploy.yml, one line put before its first
#      key and every other byte kept, and a second run reads it and writes
#      nothing.
# In 1-3 the log says which name the run gave and what it was read from, and
# the keploy.yml the run writes names the app, for later runs to keep. Every
# run must exit 0 (its app exits 0 at once).
#
# Env: RECORD_BIN (the keploy binary under test); RECORD_HANG_SECONDS (the
# bound on each recording, 120 by default).
set -uo pipefail
# shellcheck source=sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/sudo_env.sh" || exit 1
# shellcheck source=hang_guard.sh
source "$(dirname "${BASH_SOURCE[0]}")/hang_guard.sh" || exit 1

BIN="${RECORD_BIN:-keploy}"
BIN="$(command -v "$BIN" || echo "$BIN")"
WORK="$(mktemp -d)"
FAIL=0

fail() {
  echo "FAIL: $*"
  FAIL=1
}

# checkout <dir>: a git repository at dir whose origin remote is acme/shop.
checkout() {
  mkdir -p "$1" || exit 1
  git -C "$1" init -q || exit 1
  git -C "$1" remote add origin https://github.com/acme/shop.git || exit 1
}

# record <case> <dir>: `keploy record` of an app that exits 0 at once, as
# root, from dir, logging to $WORK/<case>.log; fails the case unless it
# exits 0 within the hang guard's bound.
record() {
  local name=$1 dir=$2 got
  cd "$dir" || exit 1
  bounded -s KILL "${RECORD_HANG_SECONDS:-120}" sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$BIN" record -c "sh -c 'exit 0'" --disable-tele > "$WORK/$name.log" 2>&1
  got=$?
  cd - >/dev/null || exit 1
  echo "--- $name: keploy record exited $got, in $dir ---"
  if [ "$got" != 0 ] || grep -aqF "$HANG_LINE" "$WORK/$name.log"; then
    fail "$name: keploy record exited $got, want 0"
    tail -n 40 "$WORK/$name.log"
    return 1
  fi
}

# named <case> <dir> <app> <source>: the case's run from dir named the app
# <app> after <source>, said so, and wrote a keploy.yml that names it so.
named() {
  local name=$1 dir=$2 app=$3 from=$4
  if ! grep -aqF "Naming the app \"$app\" after $from;" "$WORK/$name.log"; then
    fail "$name: its log does not say it named the app \"$app\" after $from"
    grep -aF "Naming the app" "$WORK/$name.log"
  fi
  if ! grep -qFx -e "appName: $app" -e "appName: \"$app\"" "$dir/keploy.yml"; then
    fail "$name: the keploy.yml it wrote does not name the app \"$app\""
    grep -a "appName" "$dir/keploy.yml"
  fi
}

# 1. The top folder of a checkout.
checkout "$WORK/checkout"
if record repo-top "$WORK/checkout"; then
  named repo-top "$WORK/checkout" shop "the git remote origin"
fi

# 2. A mktemp folder inside a checkout (another one, so 1's keploy.yml and
# recording are not in it).
checkout "$WORK/checkout2"
TMPD="$(mktemp -d -p "$WORK/checkout2")" || exit 1
SUB="$(basename "$TMPD")"
if record repo-mktemp "$TMPD"; then
  named repo-mktemp "$TMPD" "shop-$SUB" "the git remote origin and the path $SUB inside the repository"
fi

# 3. Outside any repository.
mkdir -p "$WORK/plain/inventory" || exit 1
if record no-repo "$WORK/plain/inventory"; then
  named no-repo "$WORK/plain/inventory" inventory "the working directory"
fi

# 4. An existing project's keploy.yml that names no app.
checkout "$WORK/checkout3"
ORDERS="$WORK/checkout3/services/orders"
mkdir -p "$ORDERS" || exit 1
printf '# the keploy.yml of a project recorded before keploy read repositories\n\npath: ""\n' > "$ORDERS/keploy.yml"
printf '# the keploy.yml of a project recorded before keploy read repositories\n\nappName: "orders"\npath: ""\n' > "$WORK/orders-want.yml"
if record existing-project "$ORDERS"; then
  if ! grep -aqF 'keploy.yml named no app, so keploy wrote appName "orders" into it' "$WORK/existing-project.log"; then
    fail "existing-project: its log does not say keploy wrote appName \"orders\" into keploy.yml"
    grep -aF "appName" "$WORK/existing-project.log"
  fi
  if grep -aqF "Naming the app" "$WORK/existing-project.log"; then
    fail "existing-project: the run gave the app a new name: $(grep -aF "Naming the app" "$WORK/existing-project.log")"
  fi
  if ! cmp -s "$ORDERS/keploy.yml" "$WORK/orders-want.yml"; then
    fail "existing-project: keploy.yml is not the file with one appName \"orders\" line added"
    diff "$WORK/orders-want.yml" "$ORDERS/keploy.yml"
  fi
  if record existing-project-again "$ORDERS"; then
    if grep -aqE 'Naming the app|wrote appName' "$WORK/existing-project-again.log"; then
      fail "existing-project-again: a run of a keploy.yml that names its app named it again: $(grep -aE 'Naming the app|wrote appName' "$WORK/existing-project-again.log")"
    fi
    if ! cmp -s "$ORDERS/keploy.yml" "$WORK/orders-want.yml"; then
      fail "existing-project-again: the run changed a keploy.yml that names its app"
      diff "$WORK/orders-want.yml" "$ORDERS/keploy.yml"
    fi
  fi
fi

sudo rm -rf "$WORK"
if [ "$FAIL" != 0 ]; then
  echo "record app-name e2e FAILED"
  exit 1
fi
echo "record app-name e2e passed"
