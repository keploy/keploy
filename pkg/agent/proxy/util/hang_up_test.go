package util

import (
	"net"
	"testing"
)

// HoldHangUp holds the hang-up on the first conn of the chain that tells one,
// found through wrappers that give the conn they wrap, and its release ends
// the hold once, telling it first when told; a chain with none, or one whose
// wrapper does not give its conn, gets a release that does nothing.
func TestHoldHangUpFindsTheKeptConnThroughItsWrappers(t *testing.T) {
	kept := &countingTeller{}
	chain := &givingWrapper{Conn: &givingWrapper{Conn: kept}}

	release := HoldHangUp(chain)
	if kept.held != 1 {
		t.Fatalf("held %d, want the kept conn under two wrappers held once", kept.held)
	}
	release(true)
	release(true)
	release(false)
	if kept.told != 1 || kept.released != 1 {
		t.Fatalf("told %d, released %d; want each once", kept.told, kept.released)
	}

	untold := &countingTeller{}
	HoldHangUp(&givingWrapper{Conn: untold})(false)
	if untold.told != 0 || untold.released != 1 {
		t.Fatalf("released untold: told %d, released %d; want told 0, released 1", untold.told, untold.released)
	}

	hidden := &countingTeller{}
	HoldHangUp(&hidingWrapper{Conn: hidden})(true)
	if hidden.held != 0 || hidden.told != 0 {
		t.Fatalf("behind a wrapper that does not give its conn: held %d, told %d; want none", hidden.held, hidden.told)
	}
	HoldHangUp(nil)(true)
}

type countingTeller struct {
	net.Conn
	held, released, told int
}

func (c *countingTeller) HoldHangUp() func() {
	c.held++
	return func() { c.released++ }
}

func (c *countingTeller) HangUpTold() { c.told++ }

type givingWrapper struct{ net.Conn }

func (w *givingWrapper) NetConn() net.Conn { return w.Conn }

type hidingWrapper struct{ net.Conn }
