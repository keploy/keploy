# DNS on hosts where glibc asks systemd-resolved

keploy records an application's DNS lookups as DNS mocks in `keploy record`
and answers them from those mocks in `keploy test`. It sees a lookup where the
application sends it: a query to port 53, which keploy's eBPF hooks send to
keploy's DNS server.

On Fedora, Arch and some other Linux distributions, `/etc/nsswitch.conf` has a
hosts line like this one:

```
hosts: files myhostname resolve [!UNAVAIL=return] dns
```

There, glibc does not send the query itself. The `resolve` module asks
systemd-resolved over a local socket, and systemd-resolved sends the query from
its own process. keploy only sees the application's own processes, so it never
sees these lookups.

## What keploy does

When keploy runs a native application (`keploy record -c "..."`,
`keploy test -c "..."`) on such a host, it starts the application in a mount
namespace of its own. In that namespace `/etc/nsswitch.conf` is a read-only
copy whose hosts line keploy can see, for example `hosts: files myhostname dns`:

- Only the application's processes see the copy. keploy never writes the
  host's `/etc/nsswitch.conf`, and the rest of the host keeps resolving names
  as before. The copy refuses writes. A process with `CAP_SYS_ADMIN` in the
  namespace (the application, when keploy runs as root) could still remount
  or unmount it.
- The copy disappears when the application's last process exits, however
  keploy ends (Ctrl+C, SIGKILL, a crash). There is nothing to restore.
- `keploy record` and `keploy test` both do this, so the lookups that were
  recorded are the ones that are replayed.
- The copy's hosts line keeps `files`, `myhostname`, `libvirt`,
  `libvirt_guest` and `dns`, which answer from this machine or over DNS. It
  drops `resolve` and the modules that ask another process or a directory
  service (`mdns*`, `mymachines`, `sss`, `ldap`, `nis`, `wins`, and any module
  keploy does not know). Those lookups go to DNS instead, which keploy records.
- A host whose hosts line names no `resolve` before `dns` gets no copy. That
  includes Ubuntu and Debian defaults, Alpine/musl, and hosts with no
  `/etc/nsswitch.conf`. The application then sees the host's file as it is.
- Docker and Kubernetes applications get no copy: their containers have an
  `/etc` of their own.

## Running keploy as a user

Making the namespace takes the `CAP_SYS_ADMIN` capability. When you run keploy
as your own user, which is the usual way (keploy elevates only its agent,
through sudo), keploy does not have it. **On such a host, keploy run the usual
way therefore gets no copy.** The application starts with the host's file, as
it always did, and keploy logs this once, at INFO:

```
keploy starts the application without a view of /etc/nsswitch.conf of its own, so ...
```

In that case the application's DNS lookups through glibc are not recorded as
mocks in `keploy record`. In `keploy test` they resolve live, through
systemd-resolved, the same way they did when recording. A name that does not
resolve at replay time (a VPN-only name with the VPN down, a service name from
the recording machine's network, any name on a machine without network) then
fails the application's lookup. keploy answers such a name itself, with its
proxy's address, only for a lookup it sees. Earlier keploy versions rewrote
the host's `/etc/nsswitch.conf` during `keploy test`, so `keploy test` saw
those lookups and answered them; such replays now fail unless keploy can make
the copy.

To let keploy make the copy while the application still runs as you, make
the keploy binary root's and writable by root alone, then give it
`CAP_SYS_ADMIN`:

```bash
KEPLOY_BIN="$(readlink -f "$(command -v keploy)")"
sudo chown root "$KEPLOY_BIN"
sudo chmod go-w "$KEPLOY_BIN"
# On a machine other people use, let only a group you trust run it
# (here "devs"): uncomment, and run it again after every update.
# sudo chgrp devs "$KEPLOY_BIN" && sudo chmod 0750 "$KEPLOY_BIN"
# Last: changing the binary's owner or group drops a file capability.
sudo setcap cap_sys_admin+p "$KEPLOY_BIN"
```

If the binary is executable by its owner alone (mode 0700, as a private
install with a strict umask leaves it), you could not run it once root owns
it. First, before the `chown`, let your own user run it, and no one else,
with an ACL entry (the `acl` package's `setfacl`):
`sudo setfacl -m u:"$(id -u)":rx "$KEPLOY_BIN"`. If that fails (no `acl`
package, or a filesystem without ACLs), stop there: nothing has changed. Do
not use `chmod g+rx` for this: the binary's group may hold other users.

Do not skip the `chown`. keploy's install script and the manual install leave
the binary owned by the user who installed it. The owner of a file can change
it through a shared writable memory mapping, and the kernel keeps the file
capability through such a change (`write` would drop it). So a capability on
a binary you own lets any program running as you take `CAP_SYS_ADMIN`, with
no password.

Run all of these again after each keploy update, any restriction included:
to a group (`chgrp` and `chmod 0750`), or to you alone (`sudo chmod go-rx`
first, then the ACL entry above: a `chmod go-rx` after `setfacl` empties the
ACL's mask, so your entry grants nothing; `chmod go-w` leaves the mask's
read and execute alone).
The update replaces the binary with a new one that anyone may run, without
the capability.

**Before you do, know what it grants.** `CAP_SYS_ADMIN` is close to root.
With it set on the binary, every user who can run that binary holds
`CAP_SYS_ADMIN` in the keploy process they start, whatever keploy command
they run. `+p` makes it permitted, not effective: keploy makes it effective
only on the one thread that makes the application's copy, and no other code
in the keploy CLI makes it effective. A flaw in keploy that let someone run
code of their own inside keploy would still have it. (keploy's agent still
runs as root, through sudo, as before. The file capability does not change
that.) Do this only on a machine where every user who can run keploy could have
root anyway, such as a single-user development machine.

If the binary is on a `nosuid` mount, keploy runs with `no_new_privs`, or the
capability bounding set lacks `CAP_SYS_ADMIN` (inside most containers), the
kernel does not give keploy the capability, and keploy says so in its INFO
line.

What the application gets:

- It runs as you, with no capability. A program started by `exec` gets none
  of the capabilities of a parent that has file capabilities, unless it has
  its own.
- It gets the environment you started keploy with. A binary with a file
  capability starts in the kernel's secure mode. In that mode, glibc's loader
  removes variables it treats as unsafe for a privileged program (such as
  `LD_PRELOAD`, `LD_LIBRARY_PATH`, `TMPDIR` and `LOCALDOMAIN`, and from
  glibc 2.39 `MALLOC_*`) from keploy's environment, and Go's runtime may set
  `GOTRACEBACK=none`.
  keploy gives the commands it starts for you (the application and the
  replay's scripts) the environment as you started keploy with it. Variables
  that keploy sets for them itself still apply. Helper programs that keploy
  runs for its own work (sudo, the docker CLI) get keploy's own environment,
  without the removed variables.
- It inherits two things secure mode changes, which keploy cannot undo, as
  the kernel changes them before keploy runs. A stack size soft limit above
  8 MiB (`ulimit -s unlimited`) is lowered to 8 MiB. Personality flags such
  as `setarch -R`'s no-randomization are cleared.
- keploy itself, in every command, runs in secure mode:
  - Where the loader removed nothing (the usual case), Go's runtime runs
    keploy in secure mode too: a panic keploy does not recover prints no
    goroutine stacks whatever `GOTRACEBACK` says (one it recovers still logs
    its stack), and a runtime fatal error (such as concurrent map writes) or
    a fatal signal exits after one line. To debug such a crash, reproduce it
    without the capability (`sudo setcap -r "$KEPLOY_BIN"`) or as root.
  - Where the loader removed something, Go's runtime runs without the
    kernel's vDSO (slower clock reads) and, on arm64, without its CPU feature
    list (generic atomics and crypto).
  - keploy is not dumpable by you. You cannot attach `strace` or a debugger
    to it as your user, and its `/proc` files are root's. Whether a crash
    leaves a core file depends on `fs.suid_dumpable` (systemd-coredump sets
    it to 2 and stores the core).
  - Its parent-death signal is cleared, so a wrapper that relies on
    `PR_SET_PDEATHSIG` to end keploy no longer does.
  - keploy's own process ignores the variables the loader removed. Its
    temporary files go to `/tmp` whatever `TMPDIR` says, and its own name
    lookups ignore `RES_OPTIONS` and `LOCALDOMAIN`.

Running keploy as root (`sudo -E keploy record -c "..."`) also makes the copy,
but then the application also runs as root.

## Hosts an earlier keploy left rewritten

Earlier keploy versions rewrote every hosts line of the host's
`/etc/nsswitch.conf` to `hosts: files dns` while `keploy test` ran, and put
the original back when they stopped. A run that was killed or crashed left
the rewritten line in place, and so did two runs that overlapped (the later
one saved the earlier one's rewrite as the original). Since then every
process on such a host has looked up names through `/etc/hosts` and DNS
alone, not through the host's own hosts line: no `resolve` (nss-resolve), no
`myhostname`, and none of the other modules it named (mDNS, `mymachines`,
libvirt, NIS, `sss`, `ldap`, `wins` and the like). Where
`/etc/resolv.conf` names systemd-resolved's stub (`127.0.0.53`, as on
Fedora), DNS lookups still reach systemd-resolved, split DNS included; where
it names other servers, they bypass it.

keploy no longer writes the file. Where authselect manages it (checked on
Fedora 44, authselect 1.7.1: `/etc/nsswitch.conf` links to
`/etc/authselect/nsswitch.conf`, and the selected profile is one of the
default profiles authselect ships), keploy can tell the rewrite, because no
such profile writes that line, and it says so once, at INFO. `authselect check` still reports the
rewritten file as valid. To put authselect's hosts line back:

```bash
sudo authselect apply-changes -b
```

This writes every file authselect manages again from its profile, PAM's
(`/etc/pam.d/system-auth` and the others) included, as any authselect change
does; `-b` keeps a copy of the current files under
`/var/lib/authselect/backups` first. keploy says nothing where it cannot tell
the rewrite from a line written on purpose: a file authselect did not
generate, a host that opted out of authselect, or a custom or vendor
authselect profile. If your hosts line there is exactly `hosts: files dns` and you did
not write it, put back your distribution's default line.
