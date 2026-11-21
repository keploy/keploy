package agent

import (
	"context"
)

// ErrRecordPauseUnsupported is what PauseRecording and ResumeRecording return
// when the agent's proxy cannot pause recording: it is not the proxy this
// package ships (a composer's own, or a test's double).
var ErrRecordPauseUnsupported error = recordPauseUnsupportedError{}

// recordPauseUnsupportedError is ErrRecordPauseUnsupported's type. Its
// RecordPauseUnsupported method is how the routes tell it from a failed pause
// (501, not 500) without importing this package's error.
type recordPauseUnsupportedError struct{}

func (recordPauseUnsupportedError) Error() string {
	return "this agent's proxy cannot pause or resume recording"
}

// RecordPauseUnsupported reports true: the agent cannot pause at all.
func (recordPauseUnsupportedError) RecordPauseUnsupported() bool { return true }

// recordPauser is the optional extension of the proxy by which the agent's
// control plane pauses recording on a running agent and resumes it
// (proxy.Proxy implements it). Type-asserted, as NoteStopBegun is, so the
// Proxy interface and its hand-written doubles stay as they are.
type recordPauser interface {
	PauseRecording(ctx context.Context, reason string) error
	ResumeRecording(ctx context.Context) error
	RecordingPaused() (paused bool, reason string)
}

// PauseRecording pauses recording for the agent's control plane (POST
// /agent/record/pause), with reason as free text for the logs: the
// application's traffic is passed through and no test case or mock is
// recorded until ResumeRecording. Idempotent.
func (a *Agent) PauseRecording(ctx context.Context, reason string) error {
	p, ok := a.Proxy.(recordPauser)
	if !ok {
		return ErrRecordPauseUnsupported
	}
	return p.PauseRecording(ctx, reason)
}

// ResumeRecording ends the control plane's pause (POST /agent/record/resume).
// Idempotent.
func (a *Agent) ResumeRecording(ctx context.Context) error {
	p, ok := a.Proxy.(recordPauser)
	if !ok {
		return ErrRecordPauseUnsupported
	}
	return p.ResumeRecording(ctx)
}

// RecordingPaused reports whether the control plane's pause is on, with its
// reason, and whether this agent can pause at all (supported): what
// /agent/health says of both.
func (a *Agent) RecordingPaused() (paused bool, reason string, supported bool) {
	p, ok := a.Proxy.(recordPauser)
	if !ok {
		return false, "", false
	}
	paused, reason = p.RecordingPaused()
	return paused, reason, true
}
