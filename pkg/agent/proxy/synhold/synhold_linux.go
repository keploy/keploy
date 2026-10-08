//go:build linux

package synhold

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	nfqueue "github.com/florianl/go-nfqueue/v2"
	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// tablePrefix names every Holder's table; the port completes it.
const tablePrefix = "keploy_synhold_"

// answeredTTL is how long an answered SYN is remembered, so that a
// retransmission of it already in flight gets the same answer instead of a
// second decision (and a second dial).
const answeredTTL = 10 * time.Second

// Holder holds the SYNs that reach one proxy port over loopback (an
// application in the proxy's network namespace — the host for a native run,
// the agent container for a docker one) until Decide has answered for them.
type Holder struct {
	logger *zap.Logger
	decide Decide
	port   uint16
	table  string
	marks  marks
	nf     *nfqueue.Nfqueue

	// queueCtx ends the queue's reader, and only Close cancels it: the
	// reader unbinds the queue when it ends, and the kernel then drops every
	// SYN still held. decideCtx ends the decisions in flight, when the
	// caller's ctx ends or Close begins; their SYNs are then answered on the
	// still-bound queue.
	queueCtx     context.Context
	cancelQueue  context.CancelFunc
	decideCtx    context.Context
	cancelDecide context.CancelFunc

	verdictMu sync.Mutex // one netlink socket carries every verdict

	// beforeUnbind, if set (tests only), runs in Close just before the
	// queue is unbound.
	beforeUnbind func()

	// selfTestPort is the local port of Start's probe connection (selfTest),
	// which onPacket answers without asking Decide; 0 when no probe runs.
	selfTestPort atomic.Uint32

	mu       sync.Mutex
	pending  map[netip.AddrPort]*hold // the SYN held from each client end
	answered map[synKey]answer        // recently answered SYNs
	expiry   []answeredAt             // the same, oldest first, to expire them in order
	closed   bool
	decides  sync.WaitGroup
}

type answeredAt struct {
	key   synKey
	until time.Time
}

// synKey names one connection attempt: its client end and its initial
// sequence number, which a retransmitted SYN repeats and a new connection
// from the same end (a reused port) does not.
type synKey struct {
	client netip.AddrPort
	isn    uint32
}

type hold struct {
	isn    uint32
	id     uint32 // the queued copy of the SYN that will get the answer
	cancel context.CancelFunc
}

type answer struct {
	out   Outcome
	until time.Time
}

// marks tell the reject rules which answer a SYN that comes back from the
// queue gets. They are random per Holder and compared whole, so a mark some
// other software set on a SYN to this port cannot select a reject by accident.
type marks struct{ refuse, host, net uint32 }

func newMarks() (marks, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return marks{}, err
	}
	m := marks{
		refuse: binary.LittleEndian.Uint32(b[0:]) | 1,
		host:   binary.LittleEndian.Uint32(b[4:]) | 1,
		net:    binary.LittleEndian.Uint32(b[8:]) | 1,
	}
	all := []uint32{m.refuse, m.host, m.net}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[i] == all[j] {
				return newMarks()
			}
		}
	}
	return m, nil
}

// Start holds the handshakes to port until decide answers for each. The queue
// is numbered after the port and the table named after it, so two agents on
// different ports in one namespace never share either. A table left behind by
// an agent that did not stop cleanly — on this port, or on another port whose
// queue nobody has bound — is removed. A queue with nobody reading it lets
// SYNs through (the rule's bypass flag), so a crashed agent leaves handshakes
// completing as they did before, not hanging.
func Start(ctx context.Context, logger *zap.Logger, port uint16, decide Decide) (*Holder, error) {
	if port == 0 {
		return nil, errors.New("no proxy port")
	}
	m, err := newMarks()
	if err != nil {
		return nil, fmt.Errorf("pick marks: %w", err)
	}
	h := &Holder{
		logger:   logger,
		decide:   decide,
		port:     port,
		table:    fmt.Sprintf("%s%d", tablePrefix, port),
		marks:    m,
		pending:  map[netip.AddrPort]*hold{},
		answered: map[synKey]answer{},
	}
	// A table an agent on this port left behind would queue SYNs to the
	// queue the moment it is bound, while its configuration is still being
	// acknowledged. Nothing else can be on this port: the proxy holds it.
	_ = h.removeTable(h.table)

	h.queueCtx, h.cancelQueue = context.WithCancel(context.WithoutCancel(ctx))
	h.decideCtx, h.cancelDecide = context.WithCancel(ctx)
	// A previous agent on this port that is still stopping may hold the
	// queue for a moment after it gave up the port. The kernel refuses a
	// queue another socket holds with EPERM — the same errno as a missing
	// CAP_NET_ADMIN — so retry briefly only while the queue is bound.
	for attempt := 0; ; attempt++ {
		if err = h.bindQueue(); err == nil || !errors.Is(err, unix.EPERM) || attempt == 20 {
			break
		}
		if !queueBound(port) {
			// Released between the bind and the check, or never held (a
			// missing capability): one more bind tells which.
			err = h.bindQueue()
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		h.cancelDecide()
		h.cancelQueue()
		return nil, err
	}

	// The queue is bound before the rules exist, so the sweep below never
	// mistakes this Holder's own table for an abandoned one, and no SYN is
	// queued before something reads the queue.
	h.removeAbandonedTables()
	if err := h.installRules(); err != nil {
		h.cancelDecide()
		h.cancelQueue()
		_ = h.nf.Close()
		return nil, fmt.Errorf("install nf_tables rules: %w", err)
	}
	if err := h.selfTest(); err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("a held handshake cannot be refused on this kernel, so the hold is not used: %w", err)
	}
	return h, nil
}

// selfTestAnswer is how Start's probe connection is answered; a test sets
// Accept to stand in for a kernel where a reject does not take effect.
var selfTestAnswer = Refuse

// selfTest proves, once, that a held SYN reaches this Holder and that a
// reject answer reaches the application as a refused connect: a probe
// connection to the port, from a local port onPacket recognises and answers
// Refuse without asking Decide, must fail with ECONNREFUSED. Where it does
// not (the SYN was never queued, or the kernel let the repeated verdict
// through), the application would see a connect succeed and then EOF while
// keploy believed it had refused it, so the hold is not used at all.
//
// The probe is recognised by its port, not by a mark on its socket: a node's
// firewall may drop marked packets (kube-proxy's KUBE-FIREWALL drops 0x8000).
func (h *Holder) selfTest() error {
	if err := h.probe(unix.AF_INET); err != nil {
		return fmt.Errorf("IPv4: %w", err)
	}
	// The application's IPv6 connections are held too, and their answers
	// are ICMPv6 and IPv6 resets: prove those where IPv6 loopback exists.
	if err := h.probe(unix.AF_INET6); err != nil && !errors.Is(err, errNoLoopback) {
		return fmt.Errorf("IPv6: %w", err)
	}
	return nil
}

var errNoLoopback = errors.New("no loopback address for this family")

// probeTimeout is how long a probe connection waits for its answer.
const probeTimeout = 3 * time.Second

// probe makes one probe connection to the port over the family's loopback.
func (h *Holder) probe(family int) error {
	fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		if family == unix.AF_INET6 {
			return errNoLoopback
		}
		return err
	}
	defer unix.Close(fd)
	var local, remote unix.Sockaddr
	if family == unix.AF_INET6 {
		local = &unix.SockaddrInet6{Addr: netip.IPv6Loopback().As16()}
		remote = &unix.SockaddrInet6{Port: int(h.port), Addr: netip.IPv6Loopback().As16()}
	} else {
		local = &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}
		remote = &unix.SockaddrInet4{Port: int(h.port), Addr: [4]byte{127, 0, 0, 1}}
	}
	if err := unix.Bind(fd, local); err != nil {
		if family == unix.AF_INET6 {
			return errNoLoopback // IPv6 disabled, or no ::1
		}
		return err
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		return err
	}
	var port int
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		port = a.Port
	case *unix.SockaddrInet6:
		port = a.Port
	}
	h.selfTestPort.Store(uint32(port))
	defer h.selfTestPort.Store(0)
	err = connectWithin(fd, remote, probeTimeout)
	switch {
	case err == nil:
		return errors.New("the probe connection was accepted")
	case errors.Is(err, unix.ECONNREFUSED):
		return nil
	}
	return fmt.Errorf("the probe connection was not refused: %w", err)
}

// connectWithin connects fd to remote, making fd non-blocking, and returns
// how the connection ended: nil if it was made, the error it failed with
// (ECONNREFUSED if it was refused), or ETIMEDOUT if nothing answered it
// within timeout.
//
// One deadline bounds the wait, however often a signal interrupts it. A
// signal can be handled on the waiting thread: one sent to the process may
// be handled on any thread, and the Go runtime's preemption signal can
// arrive just as a goroutine enters a system call. A blocking connect with
// a timeout (SO_SNDTIMEO) then fails with EINTR, the handshake still under
// way; called again it would wait on, but for the whole timeout again each
// time. So connect only starts the handshake, and its answer is waited for
// in a poll, made again with the time left when a signal interrupts it.
func connectWithin(fd int, remote unix.Sockaddr, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	// A blocking connect would wait for as long as the kernel retries the
	// SYN, far longer than the timeout.
	if err := unix.SetNonblock(fd, true); err != nil {
		return err
	}
	// A non-blocking connect does not sleep, so no signal can fail it.
	if err := unix.Connect(fd, remote); !errors.Is(err, unix.EINPROGRESS) {
		return err
	}
	// Ready to write is connected or failed, and SO_ERROR says which.
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return unix.ETIMEDOUT
		}
		ts := unix.NsecToTimespec(left.Nanoseconds())
		n, err := unix.Ppoll(fds, &ts, nil)
		if err == nil && n > 0 {
			break
		}
		if err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
	}
	errno, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
	if err != nil {
		return err
	}
	if errno != 0 {
		return unix.Errno(errno)
	}
	return nil
}

// bindQueue opens and binds the queue numbered after the port.
func (h *Holder) bindQueue() error {
	nf, err := nfqueue.Open(&nfqueue.Config{
		NfQueue:      h.port,
		MaxPacketLen: 128, // the IP and TCP headers are all a decision reads
		MaxQueueLen:  4096,
		Copymode:     nfqueue.NfQnlCopyPacket,
		// A full queue lets a SYN through instead of dropping it: past 4096
		// handshakes in flight the proxy answers at once, as it used to.
		Flags:        nfqueue.NfQaCfgFlagFailOpen,
		WriteTimeout: time.Second,
	})
	if err != nil {
		return fmt.Errorf("open NFQUEUE %d: %w", h.port, err)
	}
	// Set before the reader starts: its callbacks answer through h.nf.
	h.nf = nf
	if err := nf.RegisterWithErrorFunc(h.queueCtx, h.onPacket, h.onQueueError); err != nil {
		_ = nf.Close()
		h.nf = nil
		return fmt.Errorf("bind NFQUEUE %d: %w", h.port, err)
	}
	return nil
}

// Close stops holding. The queue rule goes first, so no further SYN is
// queued; the rest of the table stays, because removing a hook makes the
// kernel drop every packet queued in the namespace, and the held SYNs still
// need their answers. The decisions in flight are then ended and each held
// SYN answered; then the table goes, while the queue is still bound — so no
// agent starting on this port can have installed its own table under the
// same name yet — and only then is the queue unbound.
func (h *Holder) Close() error {
	err := h.stopQueueing()
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.cancelDecide()
	h.decides.Wait()
	if rerr := h.removeTable(h.table); err == nil {
		err = rerr
	}
	if h.beforeUnbind != nil {
		h.beforeUnbind()
	}
	h.cancelQueue()
	if cerr := h.nf.Close(); err == nil {
		err = cerr
	}
	return err
}

// installRules writes, in one transaction:
//
//	table inet keploy_synhold_<port> {
//	  chain input {
//	    type filter hook input priority filter;
//	    iifname "lo" tcp dport <port> meta mark <refuse> reject with tcp reset
//	    iifname "lo" tcp dport <port> meta mark <host>   reject with icmpx host-unreachable
//	    iifname "lo" tcp dport <port> meta mark <net>    reject with icmpx no-route
//	    iifname "lo" tcp dport <port> tcp flags & (fin|syn|rst|ack) == syn queue num <port> bypass
//	  }
//	}
//
// A SYN the queue answers with a reject comes back through the chain (an
// NF_REPEAT verdict carrying that answer's mark) and meets its reject rule
// before the queue rule.
func (h *Holder) installRules() error {
	c, err := nftables.New()
	if err != nil {
		return err
	}
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: h.table}
	// Replace a table an earlier agent on this port left behind: add (a
	// no-op if it exists), delete, add, all in one transaction.
	c.AddTable(t)
	c.DelTable(t)
	t = c.AddTable(t)
	ch := c.AddChain(&nftables.Chain{
		Name:     "input",
		Table:    t,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookInput,
		Priority: nftables.ChainPriorityFilter,
	})
	toPort := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname("lo")},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(h.port)},
	}
	rule := func(tail ...expr.Any) {
		exprs := append(append([]expr.Any{}, toPort...), tail...)
		c.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: exprs})
	}
	markIs := func(v uint32) []expr.Any {
		return []expr.Any{
			&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(v)},
		}
	}
	rule(append(markIs(h.marks.refuse), &expr.Reject{Type: unix.NFT_REJECT_TCP_RST})...)
	rule(append(markIs(h.marks.host), &expr.Reject{Type: unix.NFT_REJECT_ICMPX_UNREACH, Code: unix.NFT_REJECT_ICMPX_HOST_UNREACH})...)
	rule(append(markIs(h.marks.net), &expr.Reject{Type: unix.NFT_REJECT_ICMPX_UNREACH, Code: unix.NFT_REJECT_ICMPX_NO_ROUTE})...)
	rule(
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 13, Len: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 1, Mask: []byte{tcpFIN | tcpSYN | tcpRST | tcpACK}, Xor: []byte{0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{tcpSYN}},
		&expr.Queue{Num: h.port, Flag: expr.QueueFlagBypass},
	)
	return c.Flush()
}

// stopQueueing deletes the queue rule alone: deleting a rule does not
// unregister the chain's hook, so the SYNs already held stay queued.
func (h *Holder) stopQueueing() error {
	c, err := nftables.New()
	if err != nil {
		return err
	}
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: h.table}
	rules, err := c.GetRules(t, &nftables.Chain{Name: "input", Table: t})
	if err != nil {
		return err
	}
	for _, r := range rules {
		for _, e := range r.Exprs {
			if _, ok := e.(*expr.Queue); ok {
				if err := c.DelRule(r); err != nil {
					return err
				}
			}
		}
	}
	return c.Flush()
}

func (h *Holder) removeTable(name string) error {
	c, err := nftables.New()
	if err != nil {
		return err
	}
	c.DelTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: name})
	return c.Flush()
}

// removeAbandonedTables removes the tables of Holders on other ports whose
// queue nobody has bound: their agent was killed before it could remove
// them. Harmless while they stay (their SYNs bypass an unbound queue), but
// nothing else would ever remove them. A live agent binds its queue before it
// creates its table, so a table whose queue is unbound is never a live one's.
func (h *Holder) removeAbandonedTables() {
	c, err := nftables.New()
	if err != nil {
		return
	}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		return
	}
	for _, t := range tables {
		port, ok := strings.CutPrefix(t.Name, tablePrefix)
		if !ok || t.Name == h.table {
			continue
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			continue
		}
		// Read just before each removal: an agent starting meanwhile binds
		// its queue before it creates its table. This narrows, but cannot
		// close, the window in which an agent binding and installing between
		// this read and the removal loses its table; that takes two agents
		// starting within microseconds of each other in one namespace.
		bound, err := boundQueues()
		if err != nil {
			h.logger.Debug("synhold: cannot tell which queues are bound; leaving other agents' tables alone", zap.Error(err))
			return
		}
		if bound[uint16(n)] {
			continue
		}
		if err := h.removeTable(t.Name); err != nil {
			h.logger.Debug("synhold: failed to remove an abandoned table", zap.String("table", t.Name), zap.Error(err))
		}
	}
}

// queueBound reports whether some socket holds queue n in this namespace.
func queueBound(n uint16) bool {
	bound, err := boundQueues()
	return err == nil && bound[n]
}

// boundQueues reads the NFQUEUE numbers bound in this network namespace.
func boundQueues() (map[uint16]bool, error) {
	f, err := os.Open("/proc/net/netfilter/nfnetlink_queue")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	bound := map[uint16]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if n, err := strconv.ParseUint(fields[0], 10, 16); err == nil {
			bound[uint16(n)] = true
		}
	}
	return bound, sc.Err()
}

const (
	tcpFIN = 0x01
	tcpSYN = 0x02
	tcpRST = 0x04
	tcpACK = 0x10
)

// ifname is an interface name as nf_tables compares it: NUL-padded to
// IFNAMSIZ.
func ifname(n string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, n)
	return b
}

// onPacket runs on the queue's reader and must not block it: a SYN is
// answered from its own goroutine.
//
//   - A retransmission of the SYN being held is dropped: the held one is
//     answered once the destination has answered.
//   - A retransmission of a SYN already answered gets that answer again.
//   - A SYN with a new initial sequence number from an end whose earlier SYN
//     is still held is a new connection on a reused port: the application
//     gave up on the earlier one, so its decision is cancelled and its SYN
//     dropped, and the new one is decided.
func (h *Holder) onPacket(a nfqueue.Attribute) int {
	if a.PacketID == nil {
		return 0
	}
	id := *a.PacketID
	var payload []byte
	if a.Payload != nil {
		payload = *a.Payload
	}
	client, proxy, isn, ok := endpoints(payload)
	if !ok {
		h.verdict(id, Accept)
		return 0
	}
	if p := h.selfTestPort.Load(); p != 0 && uint32(client.Port()) == p &&
		(client.Addr() == netip.AddrFrom4([4]byte{127, 0, 0, 1}) || client.Addr() == netip.IPv6Loopback()) {
		h.verdict(id, selfTestAnswer)
		return 0
	}
	key := synKey{client: client, isn: isn}
	now := time.Now()

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		h.verdict(id, Accept)
		return 0
	}
	if ans, ok := h.answered[key]; ok && now.Before(ans.until) {
		h.mu.Unlock()
		h.verdict(id, ans.out)
		return 0
	}
	if cur, ok := h.pending[client]; ok {
		if cur.isn == isn {
			// A retransmission of the held SYN: hold this copy instead and
			// drop the older one. If something removed a netfilter hook in
			// this namespace meanwhile, the kernel flushed the older copy,
			// and only this one can still be answered.
			old := cur.id
			cur.id = id
			h.mu.Unlock()
			h.verdict(old, Drop)
			return 0
		}
		cur.cancel()
	}
	h.forgetExpired(now)
	ctx, cancel := context.WithCancel(h.decideCtx)
	me := &hold{isn: isn, id: id, cancel: cancel}
	h.pending[client] = me
	h.decides.Add(1)
	h.mu.Unlock()

	go func() {
		defer h.decides.Done()
		defer cancel()
		d := h.decideSafely(ctx, client, proxy)
		out := d.Outcome
		h.mu.Lock()
		id := me.id
		current := h.pending[client] == me
		if current {
			delete(h.pending, client)
			until := time.Now().Add(answeredTTL)
			h.answered[key] = answer{out: out, until: until}
			h.expiry = append(h.expiry, answeredAt{key: key, until: until})
			h.runSafely("keep", d.Keep)
		}
		h.mu.Unlock()
		if !current {
			// A newer connection from this end replaced this one; nothing
			// waits for this SYN any more.
			h.runSafely("discard", d.Discard)
			out = Drop
		}
		h.verdict(id, out)
	}()
	return 0
}

// forgetExpired drops answers past their TTL, oldest first, so each SYN
// costs only the answers that expired since the last one. Called with h.mu
// held.
func (h *Holder) forgetExpired(now time.Time) {
	n := 0
	for ; n < len(h.expiry) && !now.Before(h.expiry[n].until); n++ {
		e := h.expiry[n]
		// The same SYN may have been answered again since; keep that answer.
		if a, ok := h.answered[e.key]; ok && !a.until.After(e.until) {
			delete(h.answered, e.key)
		}
	}
	// Reslicing past the expired head is O(1); append moves only the live
	// entries when it next outgrows the array.
	h.expiry = h.expiry[n:]
}

// runSafely runs a decision's Keep or Discard, if any, so that a panic in it
// can neither leave h.mu locked nor leave the SYN unanswered.
func (h *Holder) runSafely(what string, f func()) {
	if f == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			h.logger.Error("synhold: a held handshake's "+what+" panicked", zap.Any("panic", r))
		}
	}()
	f()
}

// decideSafely is Decide with a panic turned into Accept: a held SYN must be
// answered whatever happens to the decision.
func (h *Holder) decideSafely(ctx context.Context, client, proxy netip.AddrPort) (d Decision) {
	defer func() {
		if r := recover(); r != nil {
			h.logger.Error("synhold: deciding a held handshake panicked; letting it complete",
				zap.String("client", client.String()), zap.Any("panic", r))
			d = Decision{Outcome: Accept}
		}
	}()
	return h.decide(ctx, client, proxy)
}

// verdict gives a queued SYN its answer: let it through, drop it, or send it
// back through the chain carrying the mark of its reject rule.
func (h *Holder) verdict(id uint32, out Outcome) {
	h.verdictMu.Lock()
	defer h.verdictMu.Unlock()
	var err error
	switch out {
	case Accept:
		err = h.nf.SetVerdict(id, nfqueue.NfAccept)
	case Refuse:
		err = h.nf.SetVerdictWithOption(id, nfqueue.NfRepeat, nfqueue.WithMark(h.marks.refuse))
	case HostUnreachable:
		err = h.nf.SetVerdictWithOption(id, nfqueue.NfRepeat, nfqueue.WithMark(h.marks.host))
	case NetUnreachable:
		err = h.nf.SetVerdictWithOption(id, nfqueue.NfRepeat, nfqueue.WithMark(h.marks.net))
	default:
		err = h.nf.SetVerdict(id, nfqueue.NfDrop)
	}
	if err != nil {
		h.logger.Debug("synhold: failed to answer a held handshake", zap.Uint32("packet", id), zap.Stringer("answer", out), zap.Error(err))
	}
}

func (h *Holder) onQueueError(err error) int {
	if errors.Is(err, context.Canceled) || h.queueCtx.Err() != nil {
		return 1 // Close is unbinding the queue
	}
	var op interface{ Timeout() bool }
	if errors.As(err, &op) && op.Timeout() {
		return 0
	}
	h.logger.Debug("synhold: NFQUEUE read error", zap.Error(err))
	// ENOBUFS: the kernel dropped messages it could not deliver (the queue's
	// fail-open let those SYNs through). ENOENT: a verdict for a packet the
	// kernel had already flushed. Neither repeats on its own.
	// Anything else may repeat at once: do not spin on it.
	if !errors.Is(err, unix.ENOBUFS) && !errors.Is(err, unix.ENOENT) {
		time.Sleep(100 * time.Millisecond)
	}
	return 0
}

// endpoints reads the source (the application's end), destination (the
// proxy's) and initial sequence number of an IPv4 or IPv6 TCP SYN.
func endpoints(p []byte) (client, proxy netip.AddrPort, isn uint32, ok bool) {
	if len(p) < 1 {
		return client, proxy, 0, false
	}
	switch p[0] >> 4 {
	case 4:
		ihl := int(p[0]&0x0f) * 4
		if ihl < 20 || len(p) < ihl+8 || p[9] != unix.IPPROTO_TCP {
			return client, proxy, 0, false
		}
		src, _ := netip.AddrFromSlice(p[12:16])
		dst, _ := netip.AddrFromSlice(p[16:20])
		tcp := p[ihl:]
		return netip.AddrPortFrom(src, binary.BigEndian.Uint16(tcp)),
			netip.AddrPortFrom(dst, binary.BigEndian.Uint16(tcp[2:])),
			binary.BigEndian.Uint32(tcp[4:]), true
	case 6:
		// The rule only queues TCP, so a next header other than TCP means
		// extension headers this does not walk; let the SYN through.
		if len(p) < 48 || p[6] != unix.IPPROTO_TCP {
			return client, proxy, 0, false
		}
		src, _ := netip.AddrFromSlice(p[8:24])
		dst, _ := netip.AddrFromSlice(p[24:40])
		tcp := p[40:]
		return netip.AddrPortFrom(src, binary.BigEndian.Uint16(tcp)),
			netip.AddrPortFrom(dst, binary.BigEndian.Uint16(tcp[2:])),
			binary.BigEndian.Uint32(tcp[4:]), true
	}
	return client, proxy, 0, false
}
