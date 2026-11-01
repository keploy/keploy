package containerkey

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"

	"go.keploy.io/server/v3/pkg/matcher/learn"
)

// ConnZone is the zone of the app container whose process made the
// connection the proxy accepted from app (its accepted end's remote address)
// at proxy (its local one), for a docker-mode agent. That agent runs in a PID
// namespace of its own, which its app containers share (docker run's
// --pid=container:<agent>, compose's pid: service:<agent>), so it reads no
// process's zone by the kernel's pid (learn.ProcZones); they share its
// network namespace too, so the connection's address names no container.
// The connection's socket names it: the kernel gives the cgroup of the app's
// end, which this namespace holds (socketCgroup, kernel 5.9+), and that
// cgroup's directory, under the cgroup2 mount (the host's, which keploy's
// agent container mounts at /sys/fs/cgroup), names the container
// (docker-<id>.scope, docker/<id>). The zone is the one the keploy CLI read
// of that container (ContainerZone), as the container's own rules
// (learn.ZoneOfFile). A cgroup that names a container the list held does
// not hold, one Docker started as the connection was made, asks the CLI for
// a list first (Settle).
//
// Unknown, with why: an agent that takes no lists; an app end gone, or one
// whose cgroup the kernel does not give (a kernel before 5.9, or one built
// without CONFIG_SOCK_CGROUP_DATA); a cgroup under no cgroup2 mount the
// agent sees (a host of cgroup v1 alone, where every socket is in the
// unmounted root); a cgroup of no container of the list; a container the CLI
// read no zone of, or one whose zone it could not read.
func ConnZone(ctx context.Context, app, proxy netip.AddrPort) (learn.ProcZone, error) {
	Default.mu.Lock()
	p, ok := Default.res.(*pushedIndex)
	Default.mu.Unlock()
	if !ok {
		return learn.ProcZone{}, errors.New("this agent takes no container list from keploy's CLI, so it is given no app container's zone")
	}
	return p.connZone(ctx, socketCgroup, hostCgroups, app, proxy)
}

// connZone is ConnZone by cgroupOf (socketCgroup) and dirs.
func (p *pushedIndex) connZone(ctx context.Context, cgroupOf func(local, remote netip.AddrPort) (uint64, error), dirs *cgroupDirs, app, proxy netip.AddrPort) (learn.ProcZone, error) {
	const which = "keploy cannot tell which app container made the connection: "
	cg, err := cgroupOf(app, proxy)
	if err != nil {
		return learn.ProcZone{}, errors.New(which + err.Error())
	}
	path, err := dirs.path(cg)
	if err != nil {
		return learn.ProcZone{}, errors.New(which + err.Error())
	}
	id, named := p.containerIn(path)
	if id == "" && named {
		// A container Docker started as the connection was made: the
		// CLI's next list holds it.
		p.Settle(ctx)
		id, _ = p.containerIn(path)
	}
	if id == "" {
		return learn.ProcZone{}, fmt.Errorf("%sits socket is in cgroup %s, of no container in the list keploy's CLI gave this agent", which, path)
	}
	z, ok := p.ContainerZone(ctx, id)
	switch {
	case !ok:
		return learn.ProcZone{}, fmt.Errorf("keploy's CLI gave no zone of container %s, which made the connection: it is no app container, it is gone, or the CLI's read of it stalled", p.describe(id))
	case z.Err != "":
		return learn.ProcZone{}, fmt.Errorf("keploy's CLI could not read the zone of container %s, which made the connection: %s", p.describe(id), z.Err)
	}
	return learn.ZoneOfFile(z.Name, z.Data)
}

// containerIn is the container of the list held whose ID cgroup path holds
// (the innermost, for a container's cgroup under another's), and whether the
// path holds a container ID at all (64 hex digits), of the list held or not.
func (p *pushedIndex) containerIn(path string) (id string, named bool) {
	ids := containerIDsIn(path)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st == nil || p.st.err != nil {
		return "", len(ids) > 0
	}
	for _, c := range ids {
		if p.st.images[c] != "" {
			id = c
		}
	}
	return id, len(ids) > 0
}

// describe names container id of the list held, for a message.
func (p *pushedIndex) describe(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st == nil {
		return shortID(id)
	}
	return p.st.describeLocked(id)
}

// containerIDsIn is every run of exactly 64 lowercase hex digits in s (a
// Docker container's ID), outermost first.
func containerIDsIn(s string) []string {
	var ids []string
	run := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			run++
			continue
		}
		if run == 64 {
			ids = append(ids, s[i-64:i])
		}
		run = 0
	}
	return ids
}

// cgroupDirCap bounds cgroupDirs' cache: past it, the cache starts again.
const cgroupDirCap = 4096

// cgroupDirs finds the directory of a cgroup by its ID (find:
// findCgroupDir), once: a cgroup's ID is never another's, and a directory
// not found then is never found after (a socket's cgroup is made before
// the socket is, and an ID is not given again once its cgroup is removed).
// A find that could not read the mounts, or a part of a tree, is not kept.
type cgroupDirs struct {
	find func(id uint64) (path string, found bool, err error)
	mu   sync.Mutex
	// walk is held by a find: the first connections of a container miss
	// together, and one walk of the tree answers them all.
	walk sync.Mutex
	m    map[uint64]string // "" where none was found
}

// hostCgroups is the agent's cgroupDirs.
var hostCgroups = &cgroupDirs{find: findCgroupDir}

// path is cgroup id's path in its hierarchy (/system.slice/docker-<id>.scope).
func (d *cgroupDirs) path(id uint64) (string, error) {
	if p, hit := d.cached(id); hit {
		return p, cgroupNotFound(id, p)
	}
	d.walk.Lock()
	defer d.walk.Unlock()
	if p, hit := d.cached(id); hit {
		return p, cgroupNotFound(id, p)
	}
	p, found, err := d.find(id)
	if err != nil {
		return "", err
	}
	if !found {
		p = ""
	}
	d.mu.Lock()
	if d.m == nil || len(d.m) >= cgroupDirCap {
		d.m = map[uint64]string{}
	}
	d.m[id] = p
	d.mu.Unlock()
	return p, cgroupNotFound(id, p)
}

func (d *cgroupDirs) cached(id uint64) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.m[id]
	return p, ok
}

// cgroupNotFound is path's error for cgroup id where no directory was found.
func cgroupNotFound(id uint64, path string) error {
	if path != "" {
		return nil
	}
	return fmt.Errorf("its socket is in cgroup %d, which is under no cgroup2 mount keploy's agent sees (keploy's agent container mounts the host's /sys/fs/cgroup to see an app container's; a host of cgroup v1 alone puts every socket in its cgroup2 root, which it does not mount)", id)
}
