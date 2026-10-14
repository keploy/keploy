package proxy

import (
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestRefusedLocallyOnlyForALocalPortWithNothingListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	_, err = net.Dial("tcp", addr)
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
