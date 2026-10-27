package proxy

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
)

func TestRefusedLocallyOnlyForALocalPortWithNothingListening(t *testing.T) {
	addr := testnet.RefusedAddr(t, "tcp")
	_, err := net.Dial("tcp", addr)
	if err == nil {
		t.Fatal("expected the dial to be refused")
	}
	if !refusedLocally(fmt.Errorf("%w (loopback counterpart also failed)", err)) {
		t.Fatalf("a refused dial to %s is the app not listening yet: %v", addr, err)
	}
	if refusedLocally(errors.New("connection reset by peer")) {
		t.Fatal("other errors stay errors")
	}
	remote := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 5432}, Err: err.(*net.OpError).Err}
	if refusedLocally(remote) {
		t.Fatal("a refused dependency that is not local stays an error")
	}
}
