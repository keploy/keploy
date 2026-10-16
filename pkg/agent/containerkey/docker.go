package containerkey

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// dockerAPI is the part of the Engine API the index reads.
type dockerAPI interface {
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	ImageInspect(ctx context.Context, imageID string, inspectOpts ...client.ImageInspectOption) (image.InspectResponse, error)
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
	DaemonHost() string
	Close() error
}

// errNoDaemon is the index's error on a host with no Docker daemon socket:
// there are no containers to name, which is not worth a warning.
var errNoDaemon = errors.New("there is no Docker daemon socket")

// dockerIndex knows the running containers of the local Docker daemon: each
// one's image, its IPs and the TCP ports it publishes. Each watch fills it
// once from the container list, then keeps it by Docker's event stream: a
// container that starts or joins a network is read, one that dies is dropped
// at once. Nothing is refreshed on a timer, and a lookup only reads it:
// naming a connection never waits on Docker. A connection's first use that
// finds its destination unplaced has the index list the daemon (Settle), so
// a container whose start event has not come yet is placed all the same.
type dockerIndex struct {
	mu   sync.Mutex
	host string // the endpoint SetDockerHost gave; "" for the environment's
	// parent bounds every watch: the agent's own context (Bind).
	parent context.Context
	// newAPI builds the client; a variable so a test can give its own.
	newAPI func(host string) (dockerAPI, error)
	// hostAddrs lists this host's own addresses: a port published on all
	// addresses is reached at any of them. A variable for tests.
	hostAddrs func() ([]netip.Addr, error)
	// socketExists says whether a unix socket path is there; a variable for
	// tests.
	socketExists func(path string) bool
	// applied, when set, is told each container read once the index holds
	// what was read and the read's bookkeeping is done (tests wait on it).
	applied func(id string)
	// placeDone, when set, is told each container whose placing loop ended
	// (tests wait on it).
	placeDone func(id string)
	// settleWaits, when set, is told each time a Settle waits for a list,
	// and each time a list waits for the placings of containers an event
	// spoke for while it was out (tests wait on it).
	settleWaits func()
	// imageShared, if set, is told each read of a container that takes an
	// image's defaults from another container's read of them in flight
	// (imageConfig): tests see the image read once.
	imageShared func(imageID string)
	// logger, the last session's, says at Debug which containers the index
	// takes in.
	logger *zap.Logger
	// changed, when set, is told (without waiting) whenever what the index
	// knows changes: the keploy CLI pushes it to a docker-mode agent then.
	changed chan struct{}
	// waitOnce: say at Info only the first time a watch waits for its list
	// (the keploy CLI's pusher starts a watch per session); waitSaid: it was.
	waitOnce, waitSaid bool
	// snapSeq numbers the snapshots the CLI gives a docker-mode agent;
	// changeGen counts the changes notify told.
	snapSeq   uint64
	changeGen uint64
	endpoint  string
	// cur is the current watch; nil before the first. Every change a watch
	// makes goes through its own state and counts only while it is cur, so
	// an old watch ending never touches a newer one.
	cur *watchState
	// readSlots bounds the reads of the daemon in flight at once: the first
	// list starts one per container it lists.
	readSlots chan struct{}
	// settleAsked counts the lists of the daemon Settle asked for, and
	// settled the ones that are in (one runs at a time, so they are in in
	// the order asked); settling closes when the one in flight is in, nil
	// while none is.
	settleAsked, settled uint64
	settling             chan struct{}
}

// maxReads is how many containers the index reads from the daemon at once.
const maxReads = 16

// readSlot takes one of the index's read slots, waiting for one, and returns
// its release; an index made with none reads without a bound.
func (r *dockerIndex) readSlot() func() {
	if r.readSlots == nil {
		return func() {}
	}
	r.readSlots <- struct{}{}
	return func() { <-r.readSlots }
}

// listed reports whether st holds container id (the first list put it in).
func (r *dockerIndex) listed(st *watchState, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return st.images[id] != ""
}

// watchState is one watch of the daemon and the index it keeps.
type watchState struct {
	// synced closes once the first list is in, or the watch failed; listed
	// says which.
	synced chan struct{}
	once   sync.Once
	listed bool
	// done closes when the watch has ended: its client closed, and its
	// event stream's reader returned.
	done chan struct{}
	// ctx bounds the watch, and api is its client (nil for a watch that
	// failed as it began): Settle lists the daemon with it.
	ctx context.Context
	api dockerAPI
	// stop ends the watch: its event stream, its reads and its client.
	stop context.CancelFunc
	// err is why the daemon cannot be read. It holds until the next Watch (a
	// session's start): a daemon that comes back mid-session is not read
	// again until then, so a session never flips between the two.
	err error
	// seq counts each container's events: a read applies only if no newer
	// event for its container came while it ran, so reads run side by side
	// and one stuck read holds up no other container. reading marks a
	// container with a read in flight (one at a time: an event meanwhile
	// marks it dirty, and it is read once more); destroyed, one destroyed
	// while read, or while a list may still show it (the first list, or
	// Settle's: settleOut), whose seq goes when the read and the list end.
	seq       map[string]int
	reading   map[string]bool
	dirty     map[string]bool
	destroyed map[string]bool
	// settleOut marks Settle's list out (settleList), from the moment it
	// took the event counts it is applied as of: a destroy meanwhile keeps
	// its container's count, so the list's put loses to it, as to a die.
	settleOut bool
	// placing marks a container an event started or moved that the index
	// has not placed since: where it is, not its configuration, is what a
	// connection accepted meanwhile waits for (Pending). It is placed by its
	// own loop (placeLoop), from the daemon's list, which Docker answers
	// without waiting on the container, beside and never behind its reads:
	// placingRun marks that loop running, placingDirty an event that came
	// while it ran (it runs once more). Its placing ends with the list's
	// answer, its read's put, or its die.
	placing      map[string]bool
	placingRun   map[string]bool
	placingDirty map[string]bool

	images map[string]string // container ID -> image
	names  map[string]string // container ID -> "/<name>"
	// fingerprints is each container's Fingerprint, once a read of it is
	// in; a container the first list put in waits for its read.
	fingerprints map[string]string
	// legacy is each container's fingerprint of the legacy form
	// (legacyFingerprint), read with its Fingerprint.
	legacy map[string]string
	// imageConfigs caches each image's defaults, by image ID, for
	// Fingerprint: an image ID names one configuration for good.
	imageConfigs map[string]ImageConfig
	// imageReads is each image's read in flight, by image ID: a container of
	// an image being read waits for that read, so an image is read once.
	imageReads map[string]*imageRead
	// readEnded closes, and is replaced, whenever a read of any container
	// ends: what waits for a container's fingerprint (Fingerprint) looks
	// again then.
	readEnded chan struct{}
	byIP      map[netip.Addr]string   // container IP -> container ID
	published map[uint16][]binding    // host port -> what publishes it
	ownIPs    map[string][]netip.Addr // container ID -> its IPs, to drop them
	ownPorts  map[string][]uint16     // container ID -> its host ports
	local     map[netip.Addr]bool     // this host's addresses
	// loopbackIsHost: a loopback destination is this host (true wherever the
	// index is read on the host; false in a docker-mode agent, whose
	// loopback is the app container's own).
	loopbackIsHost bool
}

// binding is a TCP port a container publishes on the host.
type binding struct {
	ip   netip.Addr // the address it is bound to; unspecified for all of them
	id   string
	port uint16 // the container's own port
}

func newDockerIndex() *dockerIndex {
	return &dockerIndex{newAPI: newDockerAPI, hostAddrs: interfaceAddrs, socketExists: socketExists, readSlots: make(chan struct{}, maxReads)}
}

func newDockerAPI(host string) (dockerAPI, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	return client.NewClientWithOpts(opts...)
}

func socketExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// SetDockerHost points the agent at the Docker endpoint its keploy client
// uses (AgentArgs), which the agent does not inherit: sudo drops the
// environment. Called once, at the agent's start, before any session.
func SetDockerHost(host string) {
	Default.mu.Lock()
	r, ok := Default.res.(*dockerIndex)
	Default.mu.Unlock()
	if ok {
		r.mu.Lock()
		r.host = host
		r.mu.Unlock()
	}
}

// Bind makes ctx, the agent's own, the bound of every watch: when the agent
// stops, the index stops reading the daemon.
func (r *dockerIndex) Bind(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parent = ctx
}

// Watch starts a watch of the daemon, unless the current one is still good,
// and returns once the index has the daemon's running containers, or knows it
// cannot read them, or ctx (the session's) ends. A session's first
// connections then find what was already running. A session that ends before
// the list is in fails that watch, so the next session starts a fresh one. It
// is called when a session starts, before the app runs; it says at Info when
// it has to wait, and on which daemon.
func (r *dockerIndex) Watch(ctx context.Context, logger *zap.Logger) {
	r.mu.Lock()
	r.logger = logger
	need, host, parent := !live(r.cur), r.host, r.parent
	r.mu.Unlock()
	if need {
		// Built and checked outside r.mu: the accept path's lookups wait on
		// it, and this reads files and lists the host's interfaces.
		st, api, endpoint := r.begin(host, parent)
		r.mu.Lock()
		if !live(r.cur) {
			r.cur, r.endpoint = st, endpoint
			if api != nil {
				st.api = api
				go r.watch(st.ctx, st, api)
			}
		} else if api != nil {
			// Another session started a watch meanwhile.
			st.stop()
			_ = api.Close()
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	st := r.cur
	r.mu.Unlock()
	select {
	case <-st.synced:
		return
	default:
	}
	r.mu.Lock()
	quiet := r.waitOnce && r.waitSaid
	r.waitSaid = true
	r.mu.Unlock()
	if logger != nil {
		msg, fields := "keploy names container destinations by the containers of the local Docker daemon, and is waiting for it to list them before the app starts", zap.String("dockerEndpoint", r.Endpoint())
		if quiet {
			logger.Debug(msg, fields)
		} else {
			logger.Info(msg, fields)
		}
	}
	select {
	case <-st.synced:
	case <-ctx.Done():
		r.failUnlisted(st, fmt.Errorf("the session ended before the Docker daemon listed its containers: %w", ctx.Err()))
	}
}

// live reports whether st is a watch a session can use: there is one, it has
// not failed, and its context (the agent's, or the session's the CLI bound it
// to) has not ended. A watch whose context ended is not used even before it
// notices: it is about to fail.
func live(st *watchState) bool {
	return st != nil && st.err == nil && st.ctx.Err() == nil
}

// begin builds a watch: the client, and the endpoint checked at once, so a
// host with no daemon or a remote one never waits. It returns the watch and
// its client, or a watch already failed and no client.
func (r *dockerIndex) begin(host string, parent context.Context) (*watchState, dockerAPI, string) {
	if parent == nil {
		parent = context.Background()
	}
	wctx, stop := context.WithCancel(parent)
	st := newWatchState()
	st.ctx, st.stop = wctx, stop
	refuse := func(err error) (*watchState, dockerAPI, string) {
		st.err = err
		stop()
		st.once.Do(func() { close(st.synced) })
		close(st.done)
		return st, nil, ""
	}
	api, err := r.newAPI(host)
	if err != nil {
		return refuse(err)
	}
	endpoint := api.DaemonHost()
	switch path, unix := strings.CutPrefix(endpoint, "unix://"); {
	case !onThisHost(endpoint, r.hostAddrs):
		_ = api.Close()
		st, _, _ := refuse(fmt.Errorf("%s is not this host's Docker daemon: its containers' ports and IPs are another machine's", endpoint))
		return st, nil, endpoint
	case unix && r.socketExists != nil && !r.socketExists(path):
		_ = api.Close()
		st, _, _ := refuse(fmt.Errorf("%w at %s", errNoDaemon, path))
		return st, nil, endpoint
	}
	return st, api, endpoint
}

// Endpoint names the daemon read, for messages.
func (r *dockerIndex) Endpoint() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.endpoint != "" {
		return r.endpoint
	}
	if r.host != "" {
		return r.host
	}
	return client.DefaultDockerHost
}

// watch reads the daemon for st until the watch ends (it fails, or the
// agent stops); then it closes the client.
func (r *dockerIndex) watch(ctx context.Context, st *watchState, api dockerAPI) {
	defer close(st.done)
	defer func() { _ = api.Close() }()
	// The stream starts from the moment before it is asked for: the daemon
	// replays what happened since, so nothing between here and the list
	// below is missed.
	since := time.Now()
	msgs, errs := api.Events(ctx, events.ListOptions{
		Since: fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()),
		Filters: filters.NewArgs(
			filters.Arg("type", string(events.ContainerEventType)),
			filters.Arg("type", string(events.NetworkEventType)),
			filters.Arg("event", string(events.ActionStart)),
			filters.Arg("event", string(events.ActionDie)),
			filters.Arg("event", string(events.ActionDestroy)),
			filters.Arg("event", string(events.ActionConnect)),
			filters.Arg("event", string(events.ActionDisconnect)),
			// For networks: a new bridge is a new address of this host.
			// Containers' creates come too (the filter is types times
			// events) and are passed over.
			filters.Arg("event", string(events.ActionCreate))),
	})
	// Taken as they come, so the daemon never waits on this subscriber (it
	// drops events for one that does). The watch ends after its reader.
	received := make(chan struct{})
	go func() {
		defer close(received)
		r.receive(ctx, st, api, msgs, errs)
	}()
	defer func() { <-received }()
	r.refreshLocal(st)
	list, err := api.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		r.fail(st, err)
		<-ctx.Done()
		return
	}
	for _, c := range list {
		r.add(ctx, api, st, c)
	}
	r.mu.Lock()
	st.listed = true
	st.forgetDestroyedLocked()
	st.once.Do(func() { close(st.synced) })
	r.mu.Unlock()
	<-ctx.Done()
	r.fail(st, ctx.Err()) // the agent stopped: nothing reads this watch as current
}

// receive applies the stream's events as they come: a die drops its
// container at once, a start or a network change starts a read of its
// container beside the others. The stream ending fails the watch.
func (r *dockerIndex) receive(ctx context.Context, st *watchState, api dockerAPI, msgs <-chan events.Message, errs <-chan error) {
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				r.fail(st, errors.New("the Docker event stream ended"))
				return
			}
			r.event(ctx, st, api, m)
		case err := <-errs:
			if ctx.Err() == nil {
				if err == nil {
					err = errors.New("the Docker event stream ended")
				}
				r.fail(st, err)
			}
			return
		case <-ctx.Done():
			return
		}
	}
}

// event applies one Docker event to st: a die or a destroy drops its
// container at once; a start or a network change reads it, one read at a
// time per container.
func (r *dockerIndex) event(ctx context.Context, st *watchState, api dockerAPI, m events.Message) {
	id, read := m.Actor.ID, false
	switch {
	case m.Type == events.NetworkEventType && (m.Action == events.ActionCreate || m.Action == events.ActionDestroy):
		r.refreshLocal(st)
		return
	case m.Type == events.NetworkEventType && (m.Action == events.ActionConnect || m.Action == events.ActionDisconnect):
		id, read = m.Actor.Attributes["container"], true
	case m.Type == events.ContainerEventType && m.Action == events.ActionStart:
		read = true
	case m.Type == events.ContainerEventType && (m.Action == events.ActionDie || m.Action == events.ActionDestroy):
	default:
		return // a container's create: nothing to apply
	}
	if id == "" {
		return
	}
	r.mu.Lock()
	st.seq[id]++
	seq := st.seq[id]
	r.mu.Unlock()
	if !read {
		r.put(st, entry{id: id}, seq)
		r.mu.Lock()
		if st.placing[id] {
			r.endPlacingLocked(st, id) // a container that died has nowhere to be placed
		}
		r.mu.Unlock()
		if m.Action == events.ActionDestroy {
			r.forget(st, id)
		}
		return
	}
	r.startRead(ctx, api, st, id, true)
}

// forget drops the event count of a destroyed container, which no event
// names again (IDs are never reused), once no read of it is in flight.
func (r *dockerIndex) forget(st *watchState, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Until the first list is in, or while Settle's list is out, the list
	// may still have the container: its count stays, so the list's put loses
	// to the destroy: a count gone is 0, which Settle's list takes for a
	// container no event spoke for.
	if st.reading[id] || !st.listed || st.settleOut {
		st.destroyed[id] = true
		return
	}
	delete(st.seq, id)
}

// startRead reads container id, unless a read of it is in flight: then that
// read runs once more when it ends, as of the latest event. placing says the
// container is to be placed too (an event started or moved it): by a loop of
// its own, beside the read.
func (r *dockerIndex) startRead(ctx context.Context, api dockerAPI, st *watchState, id string, placing bool) {
	if placing && r.claimPlace(st, id) {
		go r.placeLoop(ctx, api, st, id)
	}
	if r.claimRead(st, id) {
		go r.readLoop(ctx, api, st, id)
	}
}

// claimPlace marks container id as being placed and reports whether the
// caller starts its placing loop: false when one runs, which then runs once
// more.
func (r *dockerIndex) claimPlace(st *watchState, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !st.placing[id] {
		// A list now says it is being placed (snapshot's Placing): a
		// docker-mode agent waits for it as the index here does.
		st.placing[id] = true
		if r.cur == st {
			r.notify()
		}
	}
	if st.placingRun[id] {
		st.placingDirty[id] = true
		return false
	}
	st.placingRun[id] = true
	return true
}

// placeLoop places container id from the daemon's list (placeFromList), once
// more for each event that came meanwhile. It takes no read slot and never
// waits for a read: held configuration reads, in every slot, never keep a
// container from being placed.
func (r *dockerIndex) placeLoop(ctx context.Context, api dockerAPI, st *watchState, id string) {
	for {
		r.mu.Lock()
		seq := st.seq[id]
		delete(st.placingDirty, id)
		r.mu.Unlock()
		r.placeFromList(ctx, api, st, id, seq)
		r.mu.Lock()
		// Once more for an event that came meanwhile, unless its placing
		// ended since (a die).
		again := st.placingDirty[id] && st.placing[id] && st.err == nil && ctx.Err() == nil
		if !again {
			delete(st.placingRun, id)
			delete(st.placingDirty, id)
		}
		r.mu.Unlock()
		if !again {
			if r.placeDone != nil {
				r.placeDone(id)
			}
			return
		}
	}
}

// claimRead marks container id as being read and reports whether the caller
// starts the read: false when one is in flight, which then runs once more.
func (r *dockerIndex) claimRead(st *watchState, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st.reading[id] {
		st.dirty[id] = true
		return false
	}
	st.reading[id] = true
	if r.cur == st && st.images[id] != "" && st.fingerprints[id] == "" {
		// A list now says it is being read (snapshot's Reading): a
		// docker-mode agent waits for the list that ends the read.
		r.notify()
	}
	return true
}

func (r *dockerIndex) readLoop(ctx context.Context, api dockerAPI, st *watchState, id string) {
	for {
		r.mu.Lock()
		seq := st.seq[id]
		delete(st.dirty, id)
		r.mu.Unlock()
		release := r.readSlot()
		err := r.read(ctx, api, st, id, seq)
		release()
		if err != nil {
			r.fail(st, err)
		}
		r.mu.Lock()
		again := st.dirty[id] && st.err == nil && ctx.Err() == nil
		if !again {
			delete(st.reading, id)
			delete(st.dirty, id)
			if st.destroyed[id] && st.listed && !st.settleOut {
				delete(st.destroyed, id)
				delete(st.seq, id)
			}
			close(st.readEnded)
			st.readEnded = make(chan struct{})
			if r.cur == st && st.images[id] != "" && st.fingerprints[id] == "" {
				// A container the index holds whose read ended with no
				// fingerprint (it could not be read): a list no longer
				// says it is being read. A docker-mode agent waiting for
				// its fingerprint, or for the reads in flight, is told
				// by that list.
				r.notify()
			}
		}
		r.mu.Unlock()
		if r.applied != nil {
			r.applied(id)
		}
		if !again {
			return
		}
	}
}

// placeFromList places container id from the daemon's list, as of its event
// seq, before its configuration is read: Docker answers a list from its own
// view of its containers, without waiting on any of them, where an inspect
// waits for a container another operation holds (Podman's list may wait as
// an inspect does). Its placing ends
// with the list's answer, whatever it is: a container the list does not show
// running (not started yet, or gone) has no address to place, and its start
// marks it again; one listed by image ID (its repository is in its
// configuration only) is indexed by its read, and a connection to it
// meanwhile is compared by its authority. The container keeps the
// fingerprint the index has of it (entry.fromList).
func (r *dockerIndex) placeFromList(ctx context.Context, api dockerAPI, st *watchState, id string, seq int) {
	list, err := api.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(filters.Arg("id", id))})
	if err == nil {
		for _, c := range list {
			if c.ID != id || isImageID(c.Image, c.ImageID) {
				continue
			}
			e := summaryEntry(c)
			e.placed, e.fromList = true, true
			if r.put(st, e, seq) {
				return // its placing ended with it
			}
			break
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if st.placing[id] && st.seq[id] == seq {
		r.endPlacingLocked(st, id)
	}
}

// endPlacingLocked ends container id's placing: what waits for it
// (AwaitReadsOf) looks again, and a docker-mode agent is told. r.mu is held.
func (r *dockerIndex) endPlacingLocked(st *watchState, id string) {
	delete(st.placing, id)
	close(st.readEnded)
	st.readEnded = make(chan struct{})
	if r.cur == st {
		r.notify()
	}
}

// imageRead is one read of an image's defaults, which every container of the
// image read meanwhile waits for.
type imageRead struct {
	done chan struct{}
	ic   ImageConfig
	err  error
}

// imageConfig is image imageID's defaults, read once per image ID: from the
// cache, from a read of it in flight, or read now. An image the daemon no
// longer has gives none (and is not cached).
func (r *dockerIndex) imageConfig(ctx context.Context, api dockerAPI, st *watchState, imageID string) (ImageConfig, error) {
	r.mu.Lock()
	if ic, ok := st.imageConfigs[imageID]; ok {
		r.mu.Unlock()
		return ic, nil
	}
	if rd := st.imageReads[imageID]; rd != nil {
		r.mu.Unlock()
		if r.imageShared != nil {
			r.imageShared(imageID)
		}
		select {
		case <-rd.done:
			return rd.ic, rd.err
		case <-ctx.Done():
			return ImageConfig{}, ctx.Err()
		}
	}
	rd := &imageRead{done: make(chan struct{})}
	st.imageReads[imageID] = rd
	r.mu.Unlock()
	img, err := api.ImageInspect(ctx, imageID)
	found := err == nil
	switch {
	case client.IsErrNotFound(err):
		err = nil
	case err != nil:
	case img.Config != nil:
		rd.ic = ImageConfig{Env: img.Config.Env, Cmd: img.Config.Cmd, Entrypoint: img.Config.Entrypoint, Labels: img.Config.Labels}
	}
	rd.err = err
	r.mu.Lock()
	delete(st.imageReads, imageID)
	if found {
		st.imageConfigs[imageID] = rd.ic
	}
	r.mu.Unlock()
	close(rd.done)
	return rd.ic, err
}

// entry is what the index keeps of one container; one with no image only
// drops the container.
type entry struct {
	id    string
	name  string // "/<container name>", as Docker writes it
	image string
	// fingerprint is the container's Fingerprint, and legacy its legacy
	// one (legacyFingerprint); "" from the list, which does not carry its
	// configuration.
	fingerprint, legacy string
	// placed: putting it ends its placing (placeFromList, or a read that
	// found where it is), in the same change, so no list shows it neither
	// placing nor placed.
	placed bool
	// fromList: what the daemon's list gives, with no configuration: the
	// container keeps the fingerprint the index has of it (its read may
	// have put it in first), since a container's configuration does not
	// change while it lives.
	fromList bool
	ips      []netip.Addr
	bindings []binding
	hostPort []uint16 // bindings[i]'s host port
}

func (e *entry) publish(hostIP string, hostPort, containerPort uint16) {
	if hostPort == 0 {
		return
	}
	for _, ip := range boundTo(hostIP) {
		e.bindings = append(e.bindings, binding{ip: ip, id: e.id, port: containerPort})
		e.hostPort = append(e.hostPort, hostPort)
	}
}

func (e *entry) addIP(s string) {
	if a, err := netip.ParseAddr(s); err == nil {
		e.ips = append(e.ips, a.Unmap())
	}
}

// add indexes a container from the list, which carries all the index needs,
// unless the list shows its image as an image ID (the tag has since moved to
// another image): that container is read on its own, off the first list's
// way, so a container Docker holds locked never holds up a session's start.
// A container an event has spoken for since the stream began is left to that
// event.
func (r *dockerIndex) add(ctx context.Context, api dockerAPI, st *watchState, c container.Summary) {
	if isImageID(c.Image, c.ImageID) {
		// Its repository is in its configuration only: it is indexed by its
		// read. Not as being placed: what is at an address no connection
		// waits for, as no placing of it is under way (a connection to it
		// before its read is in is compared by its authority, and misses).
		r.startRead(ctx, api, st, c.ID, false)
		return
	}
	r.mu.Lock()
	spoken := st.seq[c.ID] != 0
	r.mu.Unlock()
	if spoken {
		return // an event spoke for it since the stream began: its read, or its die, has it
	}
	// Its configuration is read beside the others (its fingerprint): marked
	// as being read before the list's entry is in, so nothing that finds the
	// entry takes its missing fingerprint for an unreadable one.
	start := r.claimRead(st, c.ID)
	defer func() {
		if start {
			go r.readLoop(ctx, api, st, c.ID)
		}
	}()
	r.put(st, summaryEntry(c), 0)
}

// summaryEntry is what the index keeps of a container from the daemon's
// list: its name, image and addresses, and no fingerprint (the list does not
// carry its configuration).
func summaryEntry(c container.Summary) entry {
	e := entry{id: c.ID, image: NormalizeImage(c.Image)}
	if len(c.Names) > 0 {
		e.name = c.Names[0]
	}
	for _, p := range c.Ports {
		if p.Type == "tcp" {
			e.publish(p.IP, p.PublicPort, p.PrivatePort)
		}
	}
	if c.NetworkSettings != nil {
		for _, n := range c.NetworkSettings.Networks {
			if n != nil {
				e.addIP(n.IPAddress)
				e.addIP(n.GlobalIPv6Address)
			}
		}
	}
	return e
}

// read (re)indexes one container from its inspect, as of its event seq. A
// container already gone, or no longer running, is dropped; any other
// failure is returned. Every outcome is said at Debug.
func (r *dockerIndex) read(ctx context.Context, api dockerAPI, st *watchState, id string, seq int) error {
	got, err := api.ContainerInspect(ctx, id)
	switch {
	case client.IsErrNotFound(err):
		r.passOver(st, id, seq, "it is gone")
		return nil
	case err != nil && ctx.Err() != nil:
		return nil // the watch ended
	case err != nil && r.listed(st, id):
		// One container the index holds from the list could not be read:
		// it keeps its entry with no fingerprint (its calls are compared by
		// no key), and every other container keeps its own.
		r.say(st, "could not read a container's configuration", zap.String("containerID", id), zap.Error(err))
		return nil
	case err != nil:
		r.passOver(st, id, seq, "it could not be read: "+err.Error())
		return err
	case got.ContainerJSONBase != nil && got.State != nil && !got.State.Running:
		r.passOver(st, id, seq, "it is not running")
		return nil
	}
	// A read of a running container places it too, as of its event seq.
	e := entry{id: id, image: imageOf(got), placed: true}
	if got.ContainerJSONBase != nil {
		e.name = got.Name
	}
	fp, legacy, err := r.fingerprintOf(ctx, api, st, got)
	switch {
	case err != nil && ctx.Err() != nil:
		return nil // the watch ended
	case err != nil:
		// Its image could not be read: the container is indexed with no
		// fingerprint (its calls are compared by no key), and the session
		// keeps every other container's.
		r.say(st, "could not read a container's image", zap.String("containerID", id), zap.Error(err))
	default:
		e.fingerprint, e.legacy = fp, legacy
	}
	if ns := got.NetworkSettings; ns != nil {
		for _, n := range ns.Networks {
			if n != nil {
				e.addIP(n.IPAddress)
				e.addIP(n.GlobalIPv6Address)
			}
		}
		for p, bs := range ns.Ports {
			if p.Proto() != "tcp" {
				continue
			}
			for _, b := range bs {
				if hostPort, err := strconv.ParseUint(b.HostPort, 10, 16); err == nil {
					e.publish(b.HostIP, uint16(hostPort), uint16(p.Int()))
				}
			}
		}
	}
	if !r.put(st, e, seq) {
		r.say(st, "did not index a container", zap.String("containerID", id), zap.String("reason", "the index moved on while it was read: a newer event for it came, or the watch ended"))
	}
	return nil
}

// passOver drops a container a read found no use for, and says why.
func (r *dockerIndex) passOver(st *watchState, id string, seq int, why string) {
	r.put(st, entry{id: id}, seq)
	r.say(st, "did not index a container", zap.String("containerID", id), zap.String("reason", why))
}

// say logs at Debug, if st is the current watch and a session gave a
// logger; never while holding r.mu.
func (r *dockerIndex) say(st *watchState, msg string, fields ...zap.Field) {
	r.mu.Lock()
	logger, current := r.logger, r.cur == st
	r.mu.Unlock()
	if logger != nil && current {
		logger.Debug(msg, fields...)
	}
}

// put replaces what st has of e's container, as of the container's event
// seq, and reports whether it did: a newer event for it (a die while this
// read ran) wins. A container that now holds an address or a host port takes
// it from any other container st still has there, so a die Docker dropped
// never leaves a dead container answering for a port or an IP another one
// took. Nothing changes when st is no longer the current watch.
func (r *dockerIndex) put(st *watchState, e entry, seq int) bool {
	if !r.apply(st, e, seq) {
		return false
	}
	if e.image != "" {
		r.say(st, "indexed a container", zap.String("containerID", e.id), zap.String("image", e.image))
	}
	return true
}

func (r *dockerIndex) apply(st *watchState, e entry, seq int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != st || st.err != nil || st.seq[e.id] != seq {
		return false
	}
	// The CLI pushes what changed to its docker-mode agent (notify): a
	// container dropped that the index never held changes nothing.
	had := st.images[e.id] != ""
	if e.fromList && e.fingerprint == "" {
		e.fingerprint, e.legacy = st.fingerprints[e.id], st.legacy[e.id]
	}
	st.drop(e.id)
	if e.image == "" {
		if had {
			r.notify()
		}
		return true
	}
	defer r.notify()
	st.images[e.id] = e.image
	st.names[e.id] = e.name
	if e.fingerprint != "" {
		st.fingerprints[e.id] = e.fingerprint
		st.legacy[e.id] = e.legacy
	}
	for _, a := range e.ips {
		st.byIP[a] = e.id
	}
	st.ownIPs[e.id] = e.ips
	for i, b := range e.bindings {
		p := e.hostPort[i]
		kept := st.published[p][:0:0]
		for _, old := range st.published[p] {
			if !overlap(old.ip, b.ip) {
				kept = append(kept, old)
			}
		}
		st.published[p] = append(kept, b)
	}
	st.ownPorts[e.id] = e.hostPort
	if e.placed && st.placing[e.id] {
		r.endPlacingLocked(st, e.id)
	}
	return true
}

// setChanged makes changed the signal notify tells: the keploy CLI's pusher
// gives each session a signal of its own.
func (r *dockerIndex) setChanged(changed chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changed = changed
}

// notify counts a change and tells changed, if set, without waiting; r.mu
// is held.
func (r *dockerIndex) notify() {
	r.changeGen++
	if r.changed == nil {
		return
	}
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func newWatchState() *watchState {
	return &watchState{synced: make(chan struct{}), done: make(chan struct{}), stop: func() {},
		seq: map[string]int{}, reading: map[string]bool{}, dirty: map[string]bool{}, destroyed: map[string]bool{},
		placing: map[string]bool{}, placingRun: map[string]bool{}, placingDirty: map[string]bool{}, imageReads: map[string]*imageRead{},
		images: map[string]string{}, names: map[string]string{}, fingerprints: map[string]string{}, legacy: map[string]string{}, imageConfigs: map[string]ImageConfig{}, readEnded: make(chan struct{}),
		byIP: map[netip.Addr]string{}, published: map[uint16][]binding{},
		ownIPs: map[string][]netip.Addr{}, ownPorts: map[string][]uint16{}, loopbackIsHost: true}
}

// drop removes a container from st; r.mu is held.
func (st *watchState) drop(id string) {
	for _, a := range st.ownIPs[id] {
		if st.byIP[a] == id {
			delete(st.byIP, a)
		}
	}
	for _, p := range st.ownPorts[id] {
		var kept []binding
		for _, b := range st.published[p] {
			if b.id != id {
				kept = append(kept, b)
			}
		}
		if len(kept) == 0 {
			delete(st.published, p)
		} else {
			st.published[p] = kept
		}
	}
	delete(st.ownIPs, id)
	delete(st.ownPorts, id)
	delete(st.images, id)
	delete(st.names, id)
	delete(st.fingerprints, id)
	delete(st.legacy, id)
}

// overlap reports whether two bindings of one host port reach the same
// address: the same address, or a wildcard and any address of its family.
// Docker never gives two running containers overlapping bindings, so a
// container that now holds one takes it from any container before it.
func overlap(a, b netip.Addr) bool {
	return a == b || ((a.IsUnspecified() || b.IsUnspecified()) && a.Is4() == b.Is4())
}

// boundTo is the addresses a published port's HostIP binds: "" binds all
// addresses of both families.
func boundTo(hostIP string) []netip.Addr {
	if hostIP == "" {
		return []netip.Addr{netip.IPv4Unspecified(), netip.IPv6Unspecified()}
	}
	a, err := netip.ParseAddr(hostIP)
	if err != nil {
		return nil
	}
	return []netip.Addr{a.Unmap()}
}

// imageOf is the reference the container was created from (Config.Image),
// in Docker's short form. A container created from an image ID keeps the ID
// as it was given: it names no repository.
func imageOf(got container.InspectResponse) string {
	ref, id := "", ""
	if got.Config != nil {
		ref = got.Config.Image
	}
	if got.ContainerJSONBase != nil {
		id = got.Image
	}
	if ref == "" {
		return id
	}
	if isImageID(ref, id) {
		return ref
	}
	return NormalizeImage(ref)
}

// isImageID reports whether ref names the image ID id rather than a
// repository: "sha256:" and the ID, or at least 12 of its hex digits, as
// Docker abbreviates IDs. A short all-hex repository name (db, cafe) is a
// name.
func isImageID(ref, id string) bool {
	bare := strings.TrimPrefix(id, "sha256:")
	if r, ok := strings.CutPrefix(ref, "sha256:"); ok {
		return r != "" && strings.HasPrefix(bare, r)
	}
	if len(ref) < 12 {
		return false
	}
	for _, c := range ref {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return strings.HasPrefix(bare, ref)
}

func (r *dockerIndex) refreshLocal(st *watchState) {
	addrs, err := r.hostAddrs()
	local := map[netip.Addr]bool{}
	if err == nil {
		for _, a := range addrs {
			local[a.Unmap()] = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == st && !maps.Equal(st.local, local) {
		st.local = local
		r.notify()
	}
}

// fail ends watch st with err, which every lookup then returns while st is
// the current watch. A watch that already failed keeps its first error.
func (r *dockerIndex) fail(st *watchState, err error) {
	r.mu.Lock()
	failed := r.failLocked(st, err)
	r.mu.Unlock()
	if failed {
		r.say(st, "the container index stopped", zap.Error(err))
	}
}

// failUnlisted fails st only if its first list is not in: a session that
// ends after that leaves a good watch to the next one.
func (r *dockerIndex) failUnlisted(st *watchState, err error) {
	r.mu.Lock()
	failed := !st.listed && r.failLocked(st, err)
	r.mu.Unlock()
	if failed {
		r.say(st, "the container index stopped", zap.Error(err))
	}
}

func (r *dockerIndex) failLocked(st *watchState, err error) bool {
	if st.err != nil {
		return false
	}
	st.err = err
	st.stop()
	st.once.Do(func() { close(st.synced) })
	if r.cur == st {
		r.notify() // an older watch's end changes nothing a lookup reads
	}
	return true
}

// Lookup is the container dst reaches by the current watch's index: a
// container's own IP, or a TCP port a container publishes at that address
// (or on all addresses of its family, when dst is this host's). It never
// waits. err says the daemon cannot be read.
func (r *dockerIndex) Lookup(dst netip.AddrPort) (Container, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.cur
	if st == nil {
		return Container{}, false, nil
	}
	if st.err != nil {
		return Container{}, false, st.err
	}
	c, ok := st.find(dst)
	return c, ok, nil
}

// Fingerprint is container id's fingerprint, waiting while a read of it is in
// flight (one the first list or an event started): a key is never taken
// before its container's configuration is in. ok is false when the index
// does not hold the container, or ctx ends first.
func (r *dockerIndex) Fingerprint(ctx context.Context, id string) (string, bool) {
	fp, _, ok := r.Fingerprints(ctx, id)
	return fp, ok
}

// Fingerprints is Fingerprint with the container's legacy fingerprint
// (legacyFingerprint), read with it (fingerprintsResolver).
func (r *dockerIndex) Fingerprints(ctx context.Context, id string) (string, string, bool) {
	for {
		r.mu.Lock()
		st := r.cur
		if st == nil || st.err != nil {
			r.mu.Unlock()
			return "", "", false
		}
		fp, legacy, held, reading, ended := st.fingerprints[id], st.legacy[id], st.images[id] != "", st.reading[id], st.readEnded
		r.mu.Unlock()
		switch {
		case fp != "" && held:
			return fp, legacy, true
		case !reading:
			return "", "", false
		}
		select {
		case <-ended:
		case <-ctx.Done():
			return "", "", false
		case <-st.ctx.Done():
			return "", "", false
		}
	}
}

// Pending says whether a container could be at dst by the current watch
// (couldBeAt), and which containers are being read now: what a connection to
// dst the index has not placed waits for, at its first use, before it is
// looked up again (Keyer.place). It never waits.
func (r *dockerIndex) Pending(dst netip.AddrPort) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.cur
	if st == nil || st.err != nil {
		return nil, false
	}
	return st.placingIDs(), st.couldBeAt(dst.Addr())
}

// AwaitReadsOf waits until none of the containers ids is still being placed
// in the current watch, or ctx (or the watch) ends. A container started later
// is not waited for: it would hold every call while containers keep starting.
func (r *dockerIndex) AwaitReadsOf(ctx context.Context, ids []string) {
	for {
		r.mu.Lock()
		st := r.cur
		if st == nil || st.err != nil || !st.placingAny(ids) {
			r.mu.Unlock()
			return
		}
		ended := st.readEnded
		r.mu.Unlock()
		select {
		case <-ended:
		case <-ctx.Done():
			return
		case <-st.ctx.Done():
			return
		}
	}
}

// Settle waits until a list of the daemon asked after Settle was called is
// in, and every running container it shows that the index did not hold is
// placed, or ctx (or the watch) ends: a container Docker started before a
// connection's first use is then placed, though its start event has not
// reached the index (the stream lags the daemon on a busy host). A container
// an event spoke for while the list was out is left to that event, and its
// placing is waited for. Like a placing (placeFromList), it never waits for a
// container's configuration: what it places is read beside, as the first
// list's containers are. Lists are asked one at a time: the uses that come
// while one is out share the next. A list that fails places nothing, and the
// Settle that waited for it returns.
func (r *dockerIndex) Settle(ctx context.Context) {
	r.mu.Lock()
	st, after := r.cur, r.settleAsked
	r.mu.Unlock()
	for {
		r.mu.Lock()
		if r.cur != st || st == nil || st.err != nil || st.api == nil || !st.listed || r.settled > after {
			r.mu.Unlock()
			return
		}
		wait := r.settling
		if wait == nil {
			r.settleAsked++
			wait = make(chan struct{})
			r.settling = wait
			go r.settleList(st, r.settleAsked, wait)
		}
		waits := r.settleWaits
		r.mu.Unlock()
		if waits != nil {
			waits()
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return
		case <-st.ctx.Done():
			return
		}
	}
}

// settleList is Settle's list number n: it places, from the daemon's list,
// each running container the index does not hold and no event spoke for
// since the list was asked (as of the event counts then, so a die that
// overtakes the list wins), starting its configuration's read, and waits for
// the placings of those an event spoke for meanwhile; then it closes done.
// A container listed by image ID (its repository is in its configuration
// only) is not placed, and no read of it is started, as the first list's add
// starts one: its start event's read places it.
func (r *dockerIndex) settleList(st *watchState, n uint64, done chan struct{}) {
	defer func() {
		r.mu.Lock()
		r.settled, r.settling = n, nil
		st.settleOut = false
		st.forgetDestroyedLocked()
		r.mu.Unlock()
		close(done)
	}()
	r.mu.Lock()
	asked := maps.Clone(st.seq)
	st.settleOut = true
	r.mu.Unlock()
	list, err := st.api.ContainerList(st.ctx, container.ListOptions{})
	if err != nil {
		r.say(st, "could not list the daemon's containers for a connection's first use", zap.Error(err))
		return
	}
	var spoken []string
	for _, c := range list {
		if isImageID(c.Image, c.ImageID) {
			continue
		}
		seq := asked[c.ID]
		r.mu.Lock()
		held := st.images[c.ID] != ""
		read := !held && st.seq[c.ID] == seq && !st.reading[c.ID]
		if read {
			// Marked before its entry is in, as the first list's are
			// (add): nothing that finds it takes its missing fingerprint
			// for an unreadable one.
			st.reading[c.ID] = true
		}
		r.mu.Unlock()
		if held {
			continue
		}
		e := summaryEntry(c)
		e.placed, e.fromList = true, true
		if !r.put(st, e, seq) {
			// An event spoke for it since the list was asked: its own
			// placing has it, or its die.
			spoken = append(spoken, c.ID)
		}
		if read {
			go r.readLoop(st.ctx, st.api, st, c.ID)
		}
	}
	if len(spoken) > 0 {
		r.mu.Lock()
		waits := r.settleWaits
		r.mu.Unlock()
		if waits != nil {
			waits()
		}
		r.AwaitReadsOf(st.ctx, spoken)
	}
}

// forgetDestroyedLocked drops the event counts of the containers destroyed
// while a list was out, once no read of them is in flight either: no list
// out can show them now. The index's lock is held.
func (st *watchState) forgetDestroyedLocked() {
	for id := range st.destroyed {
		if !st.reading[id] {
			delete(st.destroyed, id)
			delete(st.seq, id)
		}
	}
}

// placingIDs is the containers being placed. The index's lock is held.
func (st *watchState) placingIDs() []string {
	if len(st.placing) == 0 {
		return nil
	}
	ids := make([]string, 0, len(st.placing))
	for id := range st.placing {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// placingAny reports whether any of ids is still being placed. The index's
// lock is held.
func (st *watchState) placingAny(ids []string) bool {
	for _, id := range ids {
		if st.placing[id] {
			return true
		}
	}
	return false
}

// couldBeAt reports whether a container could be at address a by st: not a
// loopback or unspecified address where it is the app container's own (a
// docker-mode agent's), which no container but the app's is at. Everywhere
// else the keyer's own rule holds (couldBeContainer). The index's lock is
// held.
func (st *watchState) couldBeAt(a netip.Addr) bool {
	a = a.Unmap()
	return st.loopbackIsHost || !(a.IsLoopback() || a.IsUnspecified())
}

// fingerprintOf is the Fingerprint of an inspected container, reading its
// image's defaults once per image ID. An image Docker no longer has (removed
// while the container runs) gives no defaults to leave out: the fingerprint
// then counts them, and the container misses rather than matches.
func (r *dockerIndex) fingerprintOf(ctx context.Context, api dockerAPI, st *watchState, got container.InspectResponse) (fp, legacy string, err error) {
	c := Config{}
	if got.Config != nil {
		c.Env, c.Cmd, c.Entrypoint, c.Labels = got.Config.Env, got.Config.Cmd, got.Config.Entrypoint, got.Config.Labels
	}
	imageID := ""
	if got.ContainerJSONBase != nil {
		imageID = got.Image
	}
	var ic ImageConfig
	if imageID != "" {
		var err error
		if ic, err = r.imageConfig(ctx, api, st, imageID); err != nil {
			return "", "", err
		}
	}
	c.Image = ic
	return Fingerprint(c), legacyFingerprint(c), nil
}

// find is the container dst reaches by st: a container's own IP, or a TCP
// port a container publishes at that address (or on all addresses of its
// family, when dst is this host's). r.mu is held.
func (st *watchState) find(dst netip.AddrPort) (Container, bool) {
	ip := dst.Addr().Unmap()
	if id, ok := st.byIP[ip]; ok {
		return Container{ID: id, Image: st.images[id], Port: dst.Port(), Fingerprint: st.fingerprints[id], Legacy: st.legacy[id]}, true
	}
	// A loopback or unspecified destination is this host only where the
	// index is read on the host. In a docker-mode agent it is the app
	// container's own, which no published port is bound to, whatever the
	// host's own addresses (loopback among them) say.
	if !st.loopbackIsHost && (ip.IsLoopback() || ip.IsUnspecified()) {
		return Container{}, false
	}
	onHost := ip.IsLoopback() || st.local[ip]
	for _, b := range st.published[dst.Port()] {
		if b.ip == ip || (onHost && b.ip.IsUnspecified() && b.ip.Is4() == ip.Is4()) {
			return Container{ID: b.id, Image: st.images[b.id], Port: b.port, Fingerprint: st.fingerprints[b.id], Legacy: st.legacy[b.id]}, true
		}
	}
	return Container{}, false
}

// onThisHost reports whether a Docker endpoint is this host's daemon: a
// local socket or pipe, or TCP to one of this host's own addresses.
func onThisHost(endpoint string, hostAddrs func() ([]netip.Addr, error)) bool {
	u, err := client.ParseHostURL(endpoint)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "unix", "npipe":
		return true
	case "tcp", "http", "https":
	default:
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	if a.Unmap().IsLoopback() {
		return true
	}
	addrs, err := hostAddrs()
	if err != nil {
		return false
	}
	for _, h := range addrs {
		if h.Unmap() == a.Unmap() {
			return true
		}
	}
	return false
}

// interfaceAddrs is this host's own addresses.
func interfaceAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}

// AgentFlag is the hidden `keploy agent` flag that carries the keploy
// client's Docker endpoint to a native agent (AgentArgs, SetDockerHost).
const AgentFlag = "docker-host"

// contextEndpoint is the endpoint of the docker CLI's current context (which
// DOCKER_HOST and DOCKER_CONTEXT choose, as for the docker CLI), "" when the
// docker CLI cannot say. A variable so a test can give its own.
var contextEndpoint = func() string {
	out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// AgentArgs is what the native launcher appends to its agent's argv under the
// proven switch: the Docker endpoint the tests' containers run on, so the
// agent reads the same daemon. That is DOCKER_HOST when set, else the docker
// CLI's current context (Docker Desktop for Linux, rootless, colima: none of
// them at the default socket). Without the switch, nothing, so the argv is
// what it was without container keys.
func AgentArgs() []string {
	if !provenswitch.On() {
		return nil
	}
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = contextEndpoint()
	}
	if host == "" {
		return nil
	}
	return []string{"--" + AgentFlag + "=" + host}
}
