package proxy

import (
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
)

// What a bytes wait wants of a connection whose application socket no table
// says the writes of (Windows): the application's own count of its send calls
// on it, kept by keploy's shim in its process (agent.AppSendCounter). Kept
// apart from the platform's reads so every platform's rows hold it.

// appSends is what keploy's shim had counted of the application's send calls
// on a connection when it was read (agent.AppSendCounter.AppSent): done, the
// bytes the calls completed with; started, every byte handed to a call;
// pending, the calls whose outcome the shim has not seen.
type appSends struct {
	done, started uint64
	pending       uint32
}

// sentWant is what a wait wants the proxy's side of a connection to receive,
// from received, what that side had received, read BEFORE s: once no send
// call is pending, every byte the calls completed with (s.done), which is all
// the application gave the kernel; while some are, every byte handed to one
// (s.started), which is never below what can arrive, and the wait asks the
// counts again (sending) until none is (sentAgain): a call cancelled, or one
// that failed, sends nothing, so done, not started, is the most that will
// ever arrive. A side that received more than the application had handed to
// send calls holds bytes the counts miss (a send the shim did not see), and
// nothing it says can be trusted: unknown.
func sentWant(received uint64, s appSends) (want uint64, sending bool, err error) {
	if received > s.started {
		return 0, false, sentPastStarted(received, s)
	}
	if s.pending == 0 {
		return s.done, false, nil
	}
	return s.started, true, nil
}

// sentAgain is a wait's want after a later read of the counts, s (read after
// received), for a connection whose earlier read had calls pending: want
// while some still are; once none is, every call made before the wait began
// has its outcome, and every byte any call completed with is in the kernel's
// hands, so received reaching s.done receives all the application wrote
// before the wait. Calls made since the wait began can only add to s.done, so
// the want never grows past the earlier one: the bytes of the calls made
// before it are no more than that (started).
func sentAgain(want, received uint64, s appSends) (uint64, bool, error) {
	if received > s.started {
		return 0, false, sentPastStarted(received, s)
	}
	if s.pending == 0 {
		return min(want, s.done), false, nil
	}
	return want, true, nil
}

// sentWrote is what the application had written to a connection as the
// proxy began closing it, from the counts then (appWrote, noteWroteAtClose):
// what its calls completed with once none is pending; while some are, every
// byte handed to one, which may still arrive. Its FIN is in neither: the
// shim counts bytes, and the proxy's side's count (TCP_INFO's BytesIn) holds
// no FIN.
func sentWrote(s appSends) appWrote {
	if s.pending == 0 {
		return appWrote{want: s.done}
	}
	return appWrote{want: s.started}
}

// sentPastStarted is the unknown answer for a proxy's side that had received
// more than the application had handed to send calls.
func sentPastStarted(received uint64, s appSends) error {
	return fmt.Errorf("%w: the proxy's side had received %d bytes, more than the %d keploy's shim saw the application hand to send calls on the connection: it sent in a way the shim does not count", models.ErrProxyConnsUnsupported, received, s.started)
}
