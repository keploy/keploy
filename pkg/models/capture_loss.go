package models

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// CaptureLossKind names one way a run's capture can be incomplete: a call the
// agent did not intercept, or intercepted and could not record or mock
// (design v14 §3.2, rule 12: capture is never silent).
type CaptureLossKind string

const (
	// CaptureLossPIDMap: the kernel refused a process a place in the agent's
	// target_namespace_pids (pid_map_insert_failures): its calls were not
	// intercepted. The process lives on, for as long as the application it is
	// part of runs (AppLife).
	CaptureLossPIDMap CaptureLossKind = "pid_map_refused"
	// CaptureLossPIDMapExcluded: the kernel refused a process a place in the
	// agent's excluded_pids, so a process that should have been left alone
	// was not excluded, and its children's calls may have been captured as
	// the application's. Such a process started before the recording, or is
	// a child of one that did (the pre-existing-process filter excludes
	// those), so it is no part of the application and outlives it, a restart
	// of it included: counted from the run's start (runLife).
	CaptureLossPIDMapExcluded CaptureLossKind = "pid_map_refused_excluded"
	// CaptureLossTLSAbort: a client-side TLS handshake with keploy's proxy
	// failed other than by the client giving it up: the client rejected the
	// certificate keploy answered with (it does not trust keploy's CA), or
	// keploy refused its hello or could not make its certificate. Its call
	// was neither recorded nor mocked. A client that hung up (a cancel, a
	// timeout, its own stop) made no call to lose and is not counted. Charged
	// to the test it happened in only (PerTest).
	CaptureLossTLSAbort CaptureLossKind = "tls_handshake_failed"
	// CaptureLossTLSAbandoned: a client hung up on its TLS handshake with
	// keploy's proxy once keploy had sent it its certificate, without a
	// word (no alert), or as soon as the handshake was done, having sent
	// nothing since (no data, no close_notify). A client that does not trust
	// keploy's CA may reject the certificate so -- Node.js under TLS 1.3
	// during the handshake; curl, libcurl and Node.js under TLS 1.2 just
	// after it -- and one that gave up (a cancel, a timeout) looks the same,
	// so whether a call was lost cannot be told: the test it happened in is
	// NOT JUDGED (NotJudged), never failed and never passed. Charged to that
	// test only (PerTest).
	CaptureLossTLSAbandoned CaptureLossKind = "tls_handshake_abandoned"
	// CaptureLossSTARTTLS: a parser asked to upgrade a connection to TLS
	// (STARTTLS, Postgres SSLRequest) and keploy could not make the upgrade,
	// so what followed was not parsed.
	CaptureLossSTARTTLS CaptureLossKind = "starttls_failed"
	// CaptureLossParserStall (W8): a connection's parser stopped taking what
	// the relay captured for it, and the chunks still queued were abandoned
	// once it had made no progress for the stall window (counted in time
	// the process could run, not wall time): what they carried was neither
	// recorded nor mocked.
	CaptureLossParserStall CaptureLossKind = "parser_stalled"
	// CaptureLossParserPanic: keploy panicked while it served a connection
	// (a bug in keploy: its parser, or the proxy's handler around it), so the
	// call it was serving was neither answered from a mock nor recorded.
	CaptureLossParserPanic CaptureLossKind = "parser_panicked"
	// CaptureLossDropped: the relay dropped captured bytes for want of room
	// (the per-connection cap, memory pressure), so the call they belong to
	// was not recorded whole.
	CaptureLossDropped CaptureLossKind = "capture_dropped"
	// CaptureLossAppConnLost: a connection of the application's that keploy
	// redirected to its proxy, whose connect() had returned, closed (or its
	// port was reused) before the proxy's side of it reached the proxy: what
	// it carried was neither recorded nor mocked.
	CaptureLossAppConnLost CaptureLossKind = "app_connection_lost"
	// CaptureLossUnpairedConn: a connection reached keploy's proxy that the
	// kernel could not tie to a connection of the application's keploy
	// redirected (the proxy sees other addresses than the application's: a
	// NAT between them, among the causes), so the proxy could not tell
	// where it was going: it was refused, neither recorded nor mocked.
	CaptureLossUnpairedConn CaptureLossKind = "unpaired_connection"
	// CaptureLossRealUncaptured: connections to a mock.real destination's
	// address keploy could neither capture nor relay as real (class e,
	// design v14 §4): it could not read which name one was for, or a name
	// one carried is not listed. What they carried reached nothing keploy
	// saw. What names each destination and what to change. Unlike the other
	// kinds it is never an agent-life count: it is a test's own, counting
	// the connections undecided or used while that test ran (§3.2 as
	// amended for class e), and never taints a later test.
	CaptureLossRealUncaptured CaptureLossKind = "real_uncaptured"
	// CaptureLossAppBytesLost: bytes the application had written to a
	// connection the proxy holds (one it opened earlier, a pooled
	// connection) never reached the proxy: the connection was reset, or the
	// proxy closed it, first. What they carried was neither recorded nor
	// mocked.
	CaptureLossAppBytesLost CaptureLossKind = "app_bytes_lost"
	// CaptureLossUnixSocket (4c a, decision 20): a process of the
	// application connected, or sent a datagram, to a Unix socket no process
	// of it owns and that is not on the allow-list (UnixAllowList): a call
	// over it goes nowhere keploy's proxy sees, so keploy cannot capture it
	// at all. It is not a loss of something keploy saw: the test it taints
	// is NOT JUDGED (NotJudged), never failed.
	CaptureLossUnixSocket CaptureLossKind = "unix_socket"
	// CaptureLossUnixUnseen (decision 20): keploy's report of the
	// application's calls over Unix sockets stopped (it loaded, and then
	// could not read on), so whether a test made one cannot be told. (A
	// kernel that loads the report neither way is one WARN, not this: keploy
	// does not support Unix sockets, and judges as without the report.) A
	// state, not a count: a mark never takes it away (Since), and every test
	// from then on carries it, NOT JUDGED, as a call over one would be. What
	// says why.
	CaptureLossUnixUnseen CaptureLossKind = "unix_unseen"
	// CaptureLossUDPReply (4c c): the application's UDP other than DNS got a
	// reply; keploy leaves such UDP where the application sends it, so it was
	// answered live, neither recorded nor mocked. Only under the proven
	// switch.
	CaptureLossUDPReply CaptureLossKind = "udp_reply"
	// CaptureLossServerFirst (P250): a connection in record whose
	// destination spoke first with a greeting keploy has no parser for (a
	// server-first protocol: SMTP, FTP, POP3, IMAP, NATS) was relayed both
	// ways, every byte, neither recorded nor mocked: each test the
	// application sent data on it in made a call keploy cannot capture, and
	// is NOT JUDGED (NotJudged), never failed. Counted once for each piece
	// of the application's data sent on it, the test's it was sent in
	// (PerTest); what the destination sends on its own charges nothing.
	// Only under the proven switch.
	CaptureLossServerFirst CaptureLossKind = "server_first"
	// CaptureLossRefusedAtAccept (P250): a connection in record whose
	// destination refused it at accept (a RESP error, a MySQL ERR packet in
	// place of its handshake) was relayed, so the application got the
	// refusal, and nothing of it recorded. Counted as CaptureLossServerFirst
	// is (NotJudged, PerTest), with its own text. Only under the proven
	// switch.
	CaptureLossRefusedAtAccept CaptureLossKind = "refused_at_accept"
	// CaptureLossContainerUnread (P116-a): the Docker daemon did not answer
	// keploy's read of the configuration of a container a call reached, for
	// keploy's stall window in time keploy could run (Docker holds an
	// inspect while another operation holds the container, and may hold it
	// for good). The container's identity, its key, is unknown to the call,
	// so whether it was matched, or recorded, under the right one cannot be
	// told: the test it is charged to is NOT JUDGED (NotJudged). A test's own
	// kind (PerTest), counted when the use gives up waiting: in a replay the
	// count charges a call matched to its own test (a replayed test's end
	// settles its calls first). A record charges by the mock instead
	// (ContainerUnreadMetadata), whenever its stamp came, and leaves the
	// count out (Without). Only under the proven switch.
	CaptureLossContainerUnread CaptureLossKind = "container_unread"
	// CaptureLossListenerDropped: the kernel dropped, at keploy's proxy
	// listener, what can lose a connection while a test ran (a SYN or final
	// ACK its SYN or accept queue had no room for: both the listener's drops
	// and its network namespace's ListenDrops rose, models.ListenDrops), so a
	// call of the test's may not have reached keploy. The client's
	// retransmission completes most such connections, and one keploy then
	// accepted lost nothing, but keploy cannot tell it from one whose client
	// gave up: that takes a count of the connects begun to the proxy, which
	// keploy's kernel programs do not keep. So the test is NOT JUDGED
	// (NotJudged), never failed. A test's own kind (PerTest), charged where
	// a call keploy missed then would be (listenerDropWhere): the agent
	// charges it at the end of the test whose window the drop fell in, or,
	// in keploy test, at the read of the misses of the test case that ended
	// before it; a drop between two tests is the next test's; one before a
	// test process's claim (the agent's ClaimInstance), or after the run's
	// last test (its closeDropsGap), is the run's, where every loss there
	// is. The one place a drop is not charged as a miss: in keploy test, one
	// before a set's first test case is that test case's, where a miss is
	// the set's. A drop is counted once both its counts have risen, so one whose
	// second rise comes after a test's end is the next test's. What says why
	// every drop at the listener counted, when keploy could not read its
	// network namespace's count. Only under the proven switch.
	CaptureLossListenerDropped CaptureLossKind = "listener_dropped"
	// CaptureLossKernelSkip: the kernel skipped runs of a kernel program of
	// keploy's that sees the application's connections, and nothing else
	// does that program's work (the hooks' coverage map, programSkips). A
	// tracing program (a raw tracepoint's, a trampoline's, a syscall
	// tracepoint's) has a per-CPU recursion guard: a run of it on a CPU
	// where a run of it is in progress is skipped, and counted in the
	// program's recursion_misses. Where the kernel can preempt a run
	// (fentry and fexit since 5.11, raw tracepoints since 7.0, syscall
	// tracepoints since 7.2, with preemption full or lazy), every other
	// task's run on that CPU is skipped meanwhile. The count is the kernel's
	// recursion_misses (5.12 onwards), so it is EVIDENCE only (Evidence),
	// never a verdict: it fails no test and leaves none not judged, on any
	// kernel. A recording's tests it covers carry it, so their replay, which
	// judges them, can say why one does not replay; on a kernel without the
	// count (5.10) the same tests are judged the same, without it. As what
	// it missed may be a connection, which outlives the test it was opened
	// in (a TCP connect), it covers every test from it on (not PerTest).
	// What names the programs. Only under the proven switch.
	CaptureLossKernelSkip CaptureLossKind = "kernel_skipped"
	// CaptureLossUnclassified: keploy's backstop for skipped runs of its
	// socket() hook (k_sock_create, a cgroup program the kernel runs for
	// every event, 4.10 onwards) could not read a process's PID namespace,
	// so the process is neither a member of the application nor excluded,
	// as the hook would have made it: whether its calls were the
	// application's cannot be told. Counted by that program itself
	// (socket_classify_failures), on every kernel keploy supports, and so
	// NOT JUDGED (NotJudged), from then on (not PerTest): the process lives
	// on. Only under the proven switch.
	CaptureLossUnclassified CaptureLossKind = "process_unclassified"
	// CaptureLossKernelSkipCall: as CaptureLossKernelSkip, for a program
	// whose skipped run can only have been one call's (a send's or a
	// receive's data, not a connection's): EVIDENCE only (Evidence) on the
	// test it happened in, a test's own kind (PerTest). A downstream build's
	// data-plane programs count it. Only under the proven switch.
	CaptureLossKernelSkipCall CaptureLossKind = "kernel_skipped_call"
	// CaptureLossUnread: pieces of what the application's connections
	// carried that a downstream build's capture marked lost because it could
	// not read them (memory it could not read; the kernel's own pages:
	// sendfile, splice, io_uring fixed buffers; a call of more than 16 MiB;
	// bytes moved in calls it saw no end of; bytes the application discarded
	// unread). Each is one call's bytes, which keploy saw and could not
	// record whole: the test it happened in fails (judged), a test's own kind
	// (PerTest). Only under the proven switch.
	CaptureLossUnread CaptureLossKind = "capture_unread"
	// CaptureLossUnrecorded: a connection of the application's that a
	// downstream build's capture stopped recording mid-stream (a capture hole
	// its parser could not re-align across, or a parser that failed), counted
	// once at the stop and once for each piece the connection carries after
	// it: each test it carries traffic in fails (judged), a test's own kind
	// (PerTest). Only under the proven switch.
	CaptureLossUnrecorded CaptureLossKind = "connection_unrecorded"
	// CaptureLossAppSubtreeUntracked: processes forked in the application's
	// subtree that a downstream build's capture could not mark as the
	// application's (its kernel map of the subtree was full, or it could not
	// read the fork's start time), so whether their calls were the
	// application's is left to a /proc walk, which a process reaped by then
	// fails: their calls may be neither recorded nor mocked,
	// and at replay reach a real dependency and pass. Such a process lives on
	// and may call in any later test, so the test it was forked in and every
	// later one fail (judged, not PerTest), for as long as the application
	// that forked it runs (AppLife): across test sets, where one application
	// serves them (--keep-app-alive), until it is started again. A fork
	// between two tests is the next test's. What names the map's bound and
	// the latest such process. Only under the proven switch.
	CaptureLossAppSubtreeUntracked CaptureLossKind = "app_subtree_untracked"
	// CaptureLossAgentStartedAgain: keploy's agent was started again (a new
	// process answered where the one that counted before had) while a loss
	// that outlives a test set could still taint the tests: one counted from
	// the running application's start (AppLife), the run's (runLife), or
	// however far back (runScoped). What the agent before counted went with
	// it, and a process of the application's it could not place may run on,
	// its calls reaching a real dependency that the new agent never sees, so
	// whether the tests are tainted cannot be told until keploy starts the
	// application anew under the new agent. The replay charges it to each
	// such test, never the agent: it leaves the test without a verdict
	// (NotJudged). What names both agent processes. Only under the proven
	// switch.
	CaptureLossAgentStartedAgain CaptureLossKind = "agent_started_again"
)

// ContainerUnreadMetadata is the mock metadata field the agent stamps a mock
// with when the Docker daemon did not answer keploy's read of its
// container's configuration (CaptureLossContainerUnread), naming the
// container: a record charges the mock's own test with it, whenever the
// read stalled, rather than by the count (Without).
const ContainerUnreadMetadata = "containerUnread"

// NotJudged reports whether a loss of kind k leaves the tests it taints
// without a verdict (decision 20), rather than failing them: a call keploy
// cannot capture at all (a Unix socket, a server-first protocol it relays),
// or a TLS handshake abandoned without a word, or a connection attempt the
// kernel dropped at keploy's listener, which keploy cannot tell lost a call,
// as opposed to one it saw and lost.
func (k CaptureLossKind) NotJudged() bool {
	return k == CaptureLossUnixSocket || k == CaptureLossUnixUnseen || k == CaptureLossTLSAbandoned || k == CaptureLossServerFirst || k == CaptureLossRefusedAtAccept || k == CaptureLossContainerUnread || k == CaptureLossListenerDropped || k == CaptureLossUnclassified || k == CaptureLossAgentStartedAgain
}

// Evidence reports whether a loss of kind k is evidence only, never a
// verdict: the kernel's count of skipped runs of keploy's programs
// (recursion_misses, 5.12 onwards), which a kernel without it (5.10) does
// not have. It fails no test and leaves none not judged (Split takes it into
// neither); a recording's tests it covers carry it as evidence for the
// replay that judges them.
func (k CaptureLossKind) Evidence() bool {
	return k == CaptureLossKernelSkip || k == CaptureLossKernelSkipCall
}

// AppLife says a loss of kind k lasts as long as the application whose
// process it lost runs: a test set the same application serves after the one
// it happened in (--keep-app-alive) is charged it too, from the
// application's start (LossBases.App), rather than from the set's own mark.
// That is a process of the application's that keploy could not place: one
// the kernel refused a place in the PID map (pid_map_refused), or one the
// capture could not mark as in the application's subtree
// (app_subtree_untracked).
func (k CaptureLossKind) AppLife() bool {
	return k == CaptureLossPIDMap || k == CaptureLossAppSubtreeUntracked
}

// runLife says a loss of kind k is a process that outlives the application,
// counted from the run's start (LossBases.Run): one the kernel refused a
// place in excluded_pids (pid_map_refused_excluded), which started before
// the recording, or is a child of one that did.
func (k CaptureLossKind) runLife() bool {
	return k == CaptureLossPIDMapExcluded
}

// runScoped says a loss of kind k is counted from the base of what keploy
// cannot capture at all however far back it was made (LossBases.Capture):
// what keploy cannot capture at all over a connection that outlives tests
// (NotJudged), a process keploy could not classify (process_unclassified),
// and the kernel's count of skipped runs (Evidence), but for a test's own
// kind (PerTest), which is counted from the test's mark as every other kind
// is.
func (k CaptureLossKind) runScoped() bool {
	return (k.NotJudged() || k.Evidence()) && !k.PerTest()
}

// Split is l's losses that fail a test (judged) and those that leave it
// without a verdict (notJudged, CaptureLossKind.NotJudged). Evidence (a
// kernel's count of skipped runs) is in neither: it is no verdict
// (Evidence).
func (l CaptureLosses) Split() (judged, notJudged CaptureLosses) {
	for _, c := range l {
		switch {
		case c.Kind.Evidence():
		case c.Kind.NotJudged():
			notJudged = append(notJudged, c)
		default:
			judged = append(judged, c)
		}
	}
	return judged, notJudged
}

// WithoutEvidence is l without its evidence (CaptureLossKind.Evidence):
// what a verdict reads.
func (l CaptureLosses) WithoutEvidence() CaptureLosses {
	var out CaptureLosses
	for _, c := range l {
		if !c.Kind.Evidence() {
			out = append(out, c)
		}
	}
	return out
}

// Evidence is l's evidence only (CaptureLossKind.Evidence).
func (l CaptureLosses) Evidence() CaptureLosses {
	var out CaptureLosses
	for _, c := range l {
		if c.Kind.Evidence() {
			out = append(out, c)
		}
	}
	return out
}

// EvidenceText is keploy's sentence for l's evidence (Evidence), "" for
// none: what the kernel skipped during the recording, which judges nothing.
func (l CaptureLosses) EvidenceText() string {
	var parts []string
	for _, c := range l.Evidence() {
		s := captureLossText(c)
		if w := captureWhatShape(c.What); w != "" {
			s += " (" + w + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

// LossBases are the counts a caller's losses are counted from, each kind
// from the one its loss lasts from (SinceWithin):
//   - Set: the test set's mark (a record's or a session's: its start), for
//     every kind but those below;
//   - App: the running application's start (AppLife): Set where it started
//     for this set, earlier where one application serves several;
//   - Run: the run's start (runLife): a process that outlives the
//     application;
//   - Capture: what keploy cannot capture at all, and the kernel's skips
//     (runScoped), however far back: the run's first mark, or the hooks'
//     load (nil) where the application may have run before it.
type LossBases struct {
	Set, App, Run, Capture CaptureLosses
}

// SameBases is LossBases with every base b: a caller whose one base is the
// start of everything it counts (a record, a single run).
func SameBases(b CaptureLosses) LossBases { return LossBases{Set: b, App: b, Run: b, Capture: b} }

// SinceWithin is what grew from each kind's base on (LossBases). What keploy
// cannot capture at all over a connection that outlives tests (runScoped: a
// Unix socket) outlives the test, the test set and the session it was opened
// in, so the application's first call over one taints every test after it in
// the run, however far back it was made. A process that lives as long as the
// application (AppLife), or outlives it (runLife), taints every test while it
// lives. A test's own kind that leaves it without a verdict (an abandoned TLS
// handshake) is counted from Set, as the losses that fail a test are.
func (l CaptureLosses) SinceWithin(b LossBases) CaptureLosses {
	var out CaptureLosses
	for _, c := range l.Since(b.Set) {
		if !c.Kind.runScoped() && !c.Kind.AppLife() && !c.Kind.runLife() {
			out = append(out, c)
		}
	}
	for _, c := range l.Since(b.App) {
		if c.Kind.AppLife() {
			out = append(out, c)
		}
	}
	for _, c := range l.Since(b.Run) {
		if c.Kind.runLife() {
			out = append(out, c)
		}
	}
	for _, c := range l.Since(b.Capture) {
		if c.Kind.runScoped() {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// NotJudgedText is keploy's sentence for losses that leave a test without a
// verdict (Split's notJudged): what keploy cannot capture, and that it
// therefore cannot prove the test's calls were all captured.
func (l CaptureLosses) NotJudgedText() string {
	var parts []string
	for _, c := range l {
		if c.Kind.Evidence() {
			continue // no verdict: EvidenceText says it
		}
		s := captureLossText(c)
		if c.What != "" {
			s += " (" + c.What + ")"
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return ""
	}
	return "not judged: " + strings.Join(parts, "; ") + ", so keploy cannot prove this test's calls were all captured"
}

// trustKeployCA is how each kind of TLS client that rejects keploy's
// certificate without a word is made to trust keploy's CA: Node.js reads
// NODE_EXTRA_CA_CERTS; the curl command line takes --cacert, or the
// CURL_CA_BUNDLE or SSL_CERT_FILE environment variable, which only the command
// line reads; an application on libcurl (PHP's Guzzle and curl extension,
// pycurl) sets CURLOPT_CAINFO itself, or its library's own option for it.
const trustKeployCA = "Node.js: set NODE_EXTRA_CA_CERTS to keploy's CA certificate; the curl command line: --cacert, CURL_CA_BUNDLE or SSL_CERT_FILE; libcurl clients such as PHP's Guzzle and pycurl: CURLOPT_CAINFO (Guzzle's verify, pycurl's CAINFO), which libcurl does not take from those variables"

// UnixUnseenRemedy is what to do when keploy's report of the application's
// calls over Unix sockets stopped while running (CaptureLossUnixUnseen). A
// state of keploy's, not of the application's dependencies, so nothing done
// to them (reaching them over TCP included) ends it. What says what failed.
const UnixUnseenRemedy = "report the reason to keploy (its report of the application's Unix-socket calls stopped; nothing done to the application or its dependencies changes that)"

// KernelSkipRemedy is what to do when the kernel skipped runs of keploy's
// kernel programs (CaptureLossKernelSkip): a kernel that does not preempt
// them does not skip them, and the agent's log names the programs.
const KernelSkipRemedy = "run keploy where the kernel does not preempt its programs (a kernel with voluntary or no preemption: /sys/kernel/debug/sched/preempt lists the modes this one offers), or report the programs keploy's agent log names to keploy"

// AgentStartedAgainRemedy is what to do when keploy's agent was started
// again while the application ran on (CaptureLossAgentStartedAgain).
const AgentStartedAgainRemedy = "find why the agent restarted (its container's restart count, an OOM kill, its log); where keploy starts the application (a command, without --keep-app-alive), the test-sets after its next start are judged again"

// ListenerDroppedRemedy is what to do when the kernel dropped connection
// attempts at keploy's proxy listener (CaptureLossListenerDropped): its SYN
// or accept queue was full, from the host's load or from more connects at
// once than the queues hold.
const ListenerDroppedRemedy = "find why the kernel dropped connection attempts at keploy's proxy listener (`nstat -az TcpExtListenDrops TcpExtListenOverflows` counts them: a host loaded enough to starve keploy's agent, or more connects at once than net.core.somaxconn and net.ipv4.tcp_max_syn_backlog let its queues hold)"

// NotJudgedRemedy is what to do about losses that leave a test without a
// verdict (Split's notJudged), for a next_step: reach a Unix-socket
// dependency over TCP; report to keploy why it cannot see those calls at
// all; make a TLS client that abandoned its handshake trust keploy's CA; list
// a server-first destination in mock.real. Empty for none.
func (l CaptureLosses) NotJudgedRemedy() string {
	var unix, unseen, tls, serverFirst, refused, docker, dropped, unclassified, restarted bool
	for _, c := range l {
		switch c.Kind {
		case CaptureLossAgentStartedAgain:
			restarted = true
		case CaptureLossUnclassified:
			unclassified = true
		case CaptureLossUnixSocket:
			unix = true
		case CaptureLossUnixUnseen:
			unseen = true
		case CaptureLossTLSAbandoned:
			tls = true
		case CaptureLossServerFirst:
			serverFirst = true
		case CaptureLossRefusedAtAccept:
			refused = true
		case CaptureLossContainerUnread:
			docker = true
		case CaptureLossListenerDropped:
			dropped = true
		}
	}
	var do []string
	if unix {
		do = append(do, "reach that dependency over a TCP address (mocked, or listed in mock.real)")
	}
	if unseen {
		do = append(do, UnixUnseenRemedy)
	}
	if tls {
		do = append(do, "make the TLS client trust keploy's CA ("+trustKeployCA+")")
	}
	if serverFirst {
		do = append(do, "list the server-first destination in keploy.yml mock.real, to reach it for real by design")
	}
	if refused {
		do = append(do, "find why the destination refused the application's connections (its connection limit, or its protected mode)")
	}
	if docker {
		do = append(do, "make `docker inspect` of the container named answer (another operation on it, such as a stop's grace period or a hung exec, holds it)")
	}
	if dropped {
		do = append(do, ListenerDroppedRemedy)
	}
	if unclassified {
		do = append(do, "report it to keploy, with the agent's log, which names the process (a read of a process's PID namespace failed)")
	}
	if restarted {
		do = append(do, AgentStartedAgainRemedy)
	}
	return strings.Join(do, ", and ")
}

// ErrMocksNotWritten says mocks the recording captured were not written to
// the mock file (a write failed, or the stop was asked to end before a slow
// or stuck disk had taken them): the recording is incomplete, a capture loss
// as much as a call never captured.
var ErrMocksNotWritten = errors.New("mocks the recording captured were not written to the mock file")

// CaptureLoss is how many times one kind of capture loss happened in the
// agent's life, with what the latest one was (never a value the app sent);
// for CaptureLossRealUncaptured, how many connections were charged to one
// test.
type CaptureLoss struct {
	Kind  CaptureLossKind `json:"kind"`
	Count uint64          `json:"count"`
	What  string          `json:"what,omitempty"`
}

// CaptureLosses are the agent's counts of every kind of capture loss, each
// only ever growing for the life of the agent.
type CaptureLosses []CaptureLoss

// ErrCaptureLossesUnsupported reports that an agent cannot count its capture
// losses (hooks without the counters). It means "unknown", never "none".
var ErrCaptureLossesUnsupported = errors.New("this agent cannot tell whether it captured every call")

// PerTest says a loss of kind k, in the agent's counts, is the test's it
// happened in, not a taint of every later test (design v14 §3.2's taint,
// which the agent-life kinds carry). A kind is a test's own only where what
// it lost is one call of the test the count lands in:
//   - a TLS handshake that failed, or was abandoned: that one call, lost or
//     maybe lost; the client's later calls are captured, or lost, on their
//     own (tls_handshake_failed, tls_handshake_abandoned);
//   - the application's data on a connection keploy relays without parsing
//     (server_first, refused_at_accept), counted for each piece as it is
//     sent;
//   - a call whose container's configuration the Docker daemon did not give
//     (container_unread), counted when the call gives up waiting;
//   - a connection attempt the kernel dropped at keploy's listener while the
//     test ran, or before it while no test was open (listener_dropped),
//     counted once both its counts have risen: at the test's end, or, for
//     one after it, at the next begin or the read that follows the test
//     (keploy test's read of a test case's misses, a test process's claim,
//     the run's last read), where a call keploy missed then would be
//     charged but for the one place listenerDropWhere names;
//   - one call's bytes the capture skipped, could not read, or stopped
//     recording (kernel_skipped_call, capture_unread,
//     connection_unrecorded, counted for each piece the connection carries);
//   - bytes the application wrote to a connection that ended before they
//     reached the proxy (app_bytes_lost), counted once for the connection:
//     it is over, so no later call goes with it, and the application's next
//     call, on another connection, is captured or lost on its own.
//
// One between two tests is the next test's, as a miss between tests is;
// one before a test process's claim, or after the run's last test, is the
// run's. Every other kind is agent-life: it taints the test it happened in and
// every later one. That is a process keploy could not place, which lives on
// (pid_map_refused, pid_map_refused_excluded, process_unclassified,
// app_subtree_untracked); a connection keploy stopped parsing, failed
// serving, or dropped bytes of, whose later calls may go with it
// (starttls_failed, parser_stalled, parser_panicked, capture_dropped); a
// connection that never reached keploy's proxy whole, so
// that no call of it was read and which test made one cannot be told
// (app_connection_lost, unpaired_connection); what keploy cannot capture at
// all (unix_socket, unix_unseen, udp_reply); the kernel's count of skipped
// runs of a program that sees connections (kernel_skipped); and a kind this
// build does not know. (CaptureLossRealUncaptured is a test's own too, but never in the
// agent's counts: the agent charges it to its test itself.)
func (k CaptureLossKind) PerTest() bool {
	return k == CaptureLossTLSAbort || k == CaptureLossTLSAbandoned || k == CaptureLossServerFirst || k == CaptureLossRefusedAtAccept || k == CaptureLossContainerUnread || k == CaptureLossListenerDropped || k == CaptureLossKernelSkipCall || k == CaptureLossUnread || k == CaptureLossUnrecorded || k == CaptureLossAppBytesLost
}

// Charged is what a test is charged from l, the counts as it ends: each kind
// that taints every later test as SinceWithin(b) has it, and each kind that
// is the test's own (PerTest) as it grew since test (the counts as the
// previous test ended, or b.Set for the first). Sorted by kind.
func (l CaptureLosses) Charged(b LossBases, test CaptureLosses) CaptureLosses {
	var out CaptureLosses
	for _, c := range l.SinceWithin(b) {
		if !c.Kind.PerTest() {
			out = append(out, c)
		}
	}
	for _, c := range l.Since(test) {
		if c.Kind.PerTest() {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Without is l without kind k's count: a record charges a container read
// the daemon did not answer by the mock it marked
// (ContainerUnreadMetadata), not by when it was counted.
func (l CaptureLosses) Without(k CaptureLossKind) CaptureLosses {
	var out CaptureLosses
	for _, c := range l {
		if c.Kind != k {
			out = append(out, c)
		}
	}
	return out
}

// Since is what grew from base on: each kind whose count is higher than in
// base, with the growth as its count. A count lower than base's (an agent
// restarted under the run) is taken whole. Sorted by kind.
func (l CaptureLosses) Since(base CaptureLosses) CaptureLosses {
	was := make(map[CaptureLossKind]uint64, len(base))
	for _, b := range base {
		was[b.Kind] = b.Count
	}
	var out CaptureLosses
	for _, c := range l {
		if c.Kind == CaptureLossUnixUnseen && c.Count > 0 {
			out = append(out, c) // a state: no mark takes it away
			continue
		}
		b := was[c.Kind]
		switch {
		case c.Count > b:
			out = append(out, CaptureLoss{Kind: c.Kind, Count: c.Count - b, What: c.What})
		case c.Count < b && c.Count > 0:
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Max is l and o merged kind by kind, each with the larger count (its What
// with it): two reads of losses counted from one base, taken in either order
// (the ends of parallel tests' reads), are as much as the later one. Sorted
// by kind.
func (l CaptureLosses) Max(o CaptureLosses) CaptureLosses {
	at := make(map[CaptureLossKind]int, len(l)+len(o))
	var out CaptureLosses
	for _, c := range append(append(CaptureLosses(nil), l...), o...) {
		if i, ok := at[c.Kind]; ok {
			if c.Count > out[i].Count {
				out[i] = c
			}
			continue
		}
		at[c.Kind] = len(out)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Plus is l and o added kind by kind (each with the What of the later of
// the two, o's, when it has one). Sorted by kind.
func (l CaptureLosses) Plus(o CaptureLosses) CaptureLosses {
	at := make(map[CaptureLossKind]int, len(l)+len(o))
	var out CaptureLosses
	for _, c := range append(append(CaptureLosses(nil), l...), o...) {
		if i, ok := at[c.Kind]; ok {
			out[i].Count += c.Count
			if c.What != "" {
				out[i].What = c.What
			}
			continue
		}
		at[c.Kind] = len(out)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Text is keploy's sentence for the losses, for a test's reason: what was
// not captured, and that the test's verdict therefore rests on calls keploy
// never saw.
func (l CaptureLosses) Text() string {
	if len(l) == 0 {
		return ""
	}
	var parts []string
	for _, c := range l {
		s := captureLossText(c)
		if w := captureWhatShape(c.What); w != "" {
			s += " (" + w + ")"
		}
		parts = append(parts, s)
	}
	return "capture incomplete: " + strings.Join(parts, "; ")
}

// captureWhatShape is what a capture loss was, as a report names it. Its
// producers give keploy's own words and a destination (host and port), which
// are kept as they are. A word that carries a path, a query or credentials
// (one of / ? # @ =) is written as a miss's target is (targetShape): a
// host-first destination keeps its host and port when they read as one
// (hostPort), an absolute URL keeps only its path's shape, credentials and
// the host they precede are {}, query values are {}, and path segments are
// shaped by PathShape, which keeps a segment that reads as a name.
func captureWhatShape(what string) string {
	words := strings.Fields(what)
	for i, w := range words {
		if strings.ContainsAny(w, "/?#@=") {
			words[i] = targetShape(w)
		}
	}
	return strings.Join(words, " ")
}

// listenerDropWhere is where a listener_dropped loss is charged: where a call
// keploy missed then would be, but for the one place it names (in keploy
// test, before a set's first test case, a drop is that test case's and a
// miss the set's). Its text says it, as the count lands on a test's line, a
// test case's, or the run's.
const listenerDropWhere = "a drop is charged where a call keploy missed then would be: to the test it fell in (in keploy test, to the test case that ended before it, until that test case's misses were read), otherwise to the next test, but to the run when it fell before a test process claimed the run (keploy's SDK claims it as the process starts) or after the run's last test; one exception: in keploy test, a drop before a test set's first test case is that test case's, where a missed call would be the set's"

func captureLossText(c CaptureLoss) string {
	switch c.Kind {
	case CaptureLossPIDMap:
		return fmt.Sprintf("the kernel refused %d insert(s) into keploy's PID map target_namespace_pids, so a process's calls were not intercepted while this test or one before it ran", c.Count)
	case CaptureLossPIDMapExcluded:
		return fmt.Sprintf("the kernel refused %d insert(s) into keploy's PID map excluded_pids, so a process that predates the run was not excluded, and its children's calls may have been captured as the application's", c.Count)
	case CaptureLossTLSAbort:
		return fmt.Sprintf("%d TLS handshake(s) with keploy's proxy failed other than by the client hanging up (a client that does not trust keploy's CA rejects keploy's certificate), so those calls were neither recorded nor mocked", c.Count)
	case CaptureLossSTARTTLS:
		return fmt.Sprintf("keploy could not make %d STARTTLS upgrade(s), so what followed on those connections was not parsed", c.Count)
	case CaptureLossParserStall:
		return fmt.Sprintf("%d connection(s)' parser stopped taking what keploy captured, and what was still queued for it was abandoned, neither recorded nor mocked", c.Count)
	case CaptureLossParserPanic:
		return fmt.Sprintf("keploy's parser panicked %d time(s) while it served a connection (or its proxy did around it), so the calls it was serving were neither answered from a mock nor recorded; this is a bug in keploy, and its agent's log has the panic and where it happened", c.Count)
	case CaptureLossDropped:
		return fmt.Sprintf("keploy dropped captured bytes %d time(s) for want of room, so the calls they belong to were not recorded whole", c.Count)
	case CaptureLossUnpairedConn:
		return fmt.Sprintf("%d connection(s) reached keploy's proxy that it could not tie to a connection of the application's (another address in between, a NAT among the causes), so keploy could not tell where they were going and refused them", c.Count)
	case CaptureLossAppBytesLost:
		return fmt.Sprintf("on %d connection(s) to keploy's proxy, bytes the application had written never reached the proxy (the connection was reset, or closed, first), so what they carried was neither recorded nor mocked", c.Count)
	case CaptureLossAppConnLost:
		return fmt.Sprintf("%d connection(s) of the application's to keploy's proxy closed before the proxy received them, so what they carried was neither recorded nor mocked", c.Count)
	case CaptureLossRealUncaptured:
		return fmt.Sprintf("keploy could neither capture nor relay as real %d connection(s) to a mock.real destination's address", c.Count)
	case CaptureLossTLSAbandoned:
		return fmt.Sprintf("a client hung up on %d TLS handshake(s) with keploy's proxy once keploy had sent its certificate, without a word, during the handshake or as soon as it was done: a client that does not trust keploy's CA rejects it so (Node.js, curl and libcurl do) and one that gave up (a cancel, a timeout) looks the same, so whether a call was lost cannot be told; if the client does not trust keploy's CA, make it trust it ("+trustKeployCA+")", c.Count)
	case CaptureLossUDPReply:
		return fmt.Sprintf("the application's UDP was answered live %d time(s) (a socket counted once per read it was answered before), which keploy neither records nor mocks (UDP other than DNS); add a port-only bypassRules entry for it if it is meant to be live, or reach it over TCP", c.Count)
	case CaptureLossUnixSocket:
		return fmt.Sprintf("the application called a Unix socket keploy cannot capture (%d time(s)) while this test or one before it ran", c.Count)
	case CaptureLossUnixUnseen:
		return "keploy's report of the application's calls over Unix sockets stopped, so whether this test or one before it made one cannot be told"
	case CaptureLossServerFirst:
		return fmt.Sprintf("the application's calls over a server-first protocol keploy has no parser for (its server greets first: SMTP, FTP, POP3) were passed through for real, neither recorded nor mocked: it sent data on them %d time(s)", c.Count)
	case CaptureLossRefusedAtAccept:
		return fmt.Sprintf("a destination refused the application's connections at accept, and its calls on them were passed through for real, neither recorded nor mocked: it sent data on them %d time(s)", c.Count)
	case CaptureLossContainerUnread:
		return fmt.Sprintf("the Docker daemon did not answer keploy's read of a container's configuration (%d time(s) a call to it was matched or recorded), so keploy cannot tell which container those calls reached, nor whether they were matched or recorded as that container's", c.Count)
	case CaptureLossListenerDropped:
		if c.What != "" {
			// Its network namespace's count unread: every packet the
			// listener dropped counted (What says why).
			return fmt.Sprintf("the kernel dropped %d packet(s) at keploy's proxy listener (a full SYN or accept queue, or a client that gave up its connect: keploy could not tell which), so a call may not have reached keploy; "+listenerDropWhere, c.Count)
		}
		return fmt.Sprintf("the kernel dropped %d connection attempt(s) at keploy's proxy listener (its SYN or accept queue full), so a call may not have reached keploy: a client that retried one, which keploy then accepted, lost nothing, but keploy cannot tell that from one that gave up; "+listenerDropWhere, c.Count)
	case CaptureLossKernelSkip:
		return fmt.Sprintf("during its recording the kernel skipped %d capture run(s) of keploy's programs that see the application's connections (a run of a tracing program is skipped on a CPU where another run of it was preempted), while this test or one before it ran", c.Count)
	case CaptureLossKernelSkipCall:
		return fmt.Sprintf("during its recording the kernel skipped %d capture run(s) of keploy's programs that capture the application's calls (a run of a tracing program is skipped on a CPU where another run of it was preempted), while this test ran", c.Count)
	case CaptureLossUnread:
		return fmt.Sprintf("keploy's capture could not read %d piece(s) of what the application's connections carried (memory it could not read; the kernel's own pages: sendfile, splice, io_uring fixed buffers; a call of more than 16 MiB; bytes moved in calls the capture saw no end of; bytes the application discarded unread), so the calls they belong to were not recorded whole", c.Count)
	case CaptureLossUnrecorded:
		return fmt.Sprintf("keploy stopped recording a connection of the application's mid-stream (a capture hole its parser could not re-align across, or a parser that failed), and %d piece(s) of its traffic, its stops included, were neither recorded nor mocked", c.Count)
	case CaptureLossUnclassified:
		return fmt.Sprintf("keploy could not tell %d time(s) whether a process was the application's (a read of its PID namespace failed where nothing had classified it), so its calls may have gone unseen while this test or one before it ran", c.Count)
	case CaptureLossAppSubtreeUntracked:
		return fmt.Sprintf("keploy could not mark %d process(es) forked in the application's subtree as the application's (its map of the subtree was full, or it could not read the fork's start time), so their calls may have been neither recorded nor mocked while this test ran, or before it; report it to keploy with the agent's log, which names the process", c.Count)
	case CaptureLossAgentStartedAgain:
		return "keploy's agent was started again while the application ran on, and what the agent before counted went with it: a process of the application's that it could not place, or a call keploy cannot capture at all, may taint this test, and the new agent cannot see it (keploy judges the tests again once it starts the application anew)"
	default:
		return fmt.Sprintf("%d capture loss(es) of kind %s", c.Count, c.Kind)
	}
}

// CaptureLossesUnverified says, for a session whose capture-loss counts could
// not be read although the agent keeps them (under the proven switch), what
// that leaves unproven: like CaptureUnverified, it is no evidence of a loss.
func CaptureLossesUnverified(err error) string {
	return fmt.Sprintf("capture unverified: keploy could not read whether the agent captured every call (%v), so it cannot prove that no call went unrecorded or unmocked. Check the agent's log for why its count could not be read", err)
}
