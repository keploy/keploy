package proxy

import (
	"context"
	"fmt"
	"net"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// The agent's control plane can pause recording on a running agent and resume
// it later, without restarting it: while the application's rollout is in
// progress, say, when what the application does is not what anyone wants
// recorded. While paused the agent is a passthrough: the application's
// traffic flows as ever, and no test case and no mock comes of it.
//
// The pause is one reason among the memory guard's for
// memoryguard.IsRecordingPaused (memoryguard.SetExternalPause), so everything
// that already honours a memory pause honours this one, and what keeps a
// memory pause from leaving a test case half captured keeps this one from it
// too: every mock manager opens a span at the pause and closes it at the
// resume, drops the mocks requested within it, and the recording leaves out
// every test case whose window overlaps it.
//
// What a memory pause does not do, this one adds. A connection that arrives
// during the pause is relayed unread from its first byte (servePaused; the
// ingress does the same), before any TLS interception and before any parser
// looks at it. Such a connection, and one a parser gave up to a raw relay for
// the pause, can never be recorded again, so each is tracked
// (util.DefaultPausePassthroughs) and reset at the resume: the application
// reconnects, and its new connection is recorded.
//
// The resume resets those connections BEFORE it ends the pause. So no test
// case that starts after the pause's span has closed can make a call on a
// connection that is still passed through; one that starts earlier overlaps
// the span, and is left out.

// PauseRecording pauses recording for the agent's control plane, with reason
// as free text for the logs. Idempotent: a repeated call only replaces the
// reason. The pause is process memory, so an agent that restarts starts
// unpaused.
func (p *Proxy) PauseRecording(_ context.Context, reason string) error {
	p.recordPauseMu.Lock()
	defer p.recordPauseMu.Unlock()
	was, _ := memoryguard.ExternalPaused()
	// Tracking opens before the flag is set: whatever reads the flag finds
	// the registry ready to track it.
	util.DefaultPausePassthroughs.Open()
	memoryguard.SetExternalPause(true, reason)
	if was {
		p.logger.Debug("the control plane asked to pause recording, which is already paused for it", zap.String("reason", reason))
		return nil
	}
	p.logger.Info("Recording paused by the control plane: the application's traffic is passed through, and no test case or mock is recorded until it resumes recording",
		zap.String("reason", reason),
		zap.String("next_step", "nothing to do: POST /agent/record/resume resumes recording; the test cases in flight at the pause and at the resume are left out rather than saved half captured"))
	return nil
}

// ResumeRecording ends the control plane's pause. It resets every connection
// passed through unread for the pause, so that each is reconnected and
// recorded, and then lets recording go on. A pause the memory guard holds
// stays on. Idempotent.
func (p *Proxy) ResumeRecording(_ context.Context) error {
	p.recordPauseMu.Lock()
	defer p.recordPauseMu.Unlock()
	was, reason := memoryguard.ExternalPaused()
	// Before the pause ends (see the note above). It also stops the tracking
	// of new ones, which are recorded from here on.
	reset := util.DefaultPausePassthroughs.ResetAll()
	memoryguard.SetExternalPause(false, "")
	if !was {
		p.logger.Debug("the control plane asked to resume recording, which it had not paused")
		return nil
	}
	fields := []zap.Field{
		zap.String("paused_for", reason),
		zap.Int("connections_reset", reset),
	}
	if memoryguard.IsRecordingPaused() {
		p.logger.Info("The control plane resumed recording, but recording stays paused for another reason: the agent is short of memory",
			append(fields, zap.String("pause_cause", memoryguard.PauseCause()))...)
		return nil
	}
	p.logger.Info("Recording resumed by the control plane: the connections passed through during the pause were reset, so the application reconnects and its calls are recorded again",
		append(fields, zap.String("next_step", "nothing to do: a client that pools its connections reconnects; what the pause's connections carried was not recorded"))...)
	return nil
}

// RecordingPaused reports whether the control plane's pause is on, and the
// reason it was asked with.
func (p *Proxy) RecordingPaused() (paused bool, reason string) {
	return memoryguard.ExternalPaused()
}

// servePaused relays a connection that arrived while the control plane had
// paused recording to its destination, unread, until either side ends it or
// the resume resets it. pc tracks it for that reset, and already holds
// srcConn. The caller closes srcConn when this returns; this function closes
// what it dials.
func (p *Proxy) servePaused(ctx context.Context, pc *util.PausedConn, srcConn net.Conn, dstAddr string, pre *util.Predialed) error {
	defer pc.Done()
	p.logger.Debug("recording is paused by the control plane, so this connection is passed through to its destination unread and nothing on it is recorded; it is reset when recording resumes, so the application reconnects under capture",
		zap.String("destination", dstAddr))

	dstConn, err := util.DialDestination(ctx, p.logger, "tcp", util.DialTarget{Addr: dstAddr, Predialed: pre})
	if err != nil {
		// %w keeps the dial's own error classifiable (refusedLocally) by the
		// accept loop, which logs it.
		return fmt.Errorf("recording paused: passing the connection through to %s: %w", dstAddr, err)
	}

	pt := &noSessionPassthrough{src: srcConn, dst: dstConn, dest: dstAddr}
	defer pt.end(false)
	if !pc.Add(dstConn) {
		// Recording resumed while the destination was being dialled, and
		// the reset took the application's side: the application reconnects,
		// and that connection is recorded.
		pt.end(true)
		return nil
	}

	// The relay does not watch ctx, and the proxy's shutdown expects its
	// connections to end once ctx is cancelled.
	stop := context.AfterFunc(ctx, func() { pt.end(true) })
	defer stop()

	pt.relay()
	return nil
}
