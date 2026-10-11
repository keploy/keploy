//go:build linux

package connseq

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

// tcpUpstream dials a loopback listener and returns the proxy's end, as
// util.DialUpstream makes it, and the destination's.
func tcpUpstream(t testing.TB) (*Upstream, net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		accepted <- c
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dest := <-accepted
	if dest == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = c.Close(); _ = dest.Close() })
	u := NewUpstream(c)
	if u.socket == nil {
		t.Fatal("a TCP connection's Upstream has no socket to count by")
	}
	return u, dest
}

// awaitSent waits until the socket under u has received n bytes in all.
func awaitSent(t *testing.T, u *Upstream, n uint64) {
	t.Helper()
	for limit := time.Now().Add(5 * time.Second); ; {
		if got := u.socket.received(); got >= n {
			return
		}
		if time.Now().After(limit) {
			t.Fatalf("the socket received %d bytes in 5s, want %d", u.socket.received(), n)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

func readString(t *testing.T, r io.Reader, size int) string {
	t.Helper()
	buf := make([]byte, size)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

// Bytes the destination sent before a client chunk was read are numbered
// before it, though nothing read them until after it was numbered: the socket
// had them. Bytes it sent after are after it, though they are read in the same
// Read: the Read stops at the cut, and the next one returns them.
func TestFromClientIsAfterWhatTheSocketHadReceivedReadOrNot(t *testing.T) {
	t.Parallel()
	u, dest := tcpUpstream(t)
	const early, answer = "HTTP/1.1 408 Request Timeout\r\n\r\n", "HTTP/1.1 200 OK\r\n\r\n"
	if _, err := dest.Write([]byte(early)); err != nil {
		t.Fatal(err)
	}
	awaitSent(t, u, uint64(len(early)))
	req := u.FromClient() // nothing has read the 408
	if _, err := dest.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	awaitSent(t, u, uint64(len(early)+len(answer))) // one Read could now take both
	if got := readString(t, u, 1024); got != early {
		t.Fatalf("the first Read returned %q, want only the bytes before the cut, %q", got, early)
	}
	first := u.FromDest()
	if got := readString(t, u, 1024); got != answer {
		t.Fatalf("the second Read returned %q, want %q", got, answer)
	}
	second := u.FromDest()
	if !before(first, req) || !before(req, second) {
		t.Fatalf("408 %d, request %d, answer %d: want 408 < request < answer", first, req, second)
	}
}

// With TLS above the Upstream, what the socket received and what the Upstream
// returned are counted in the same bytes, the records' own: a record the
// destination sent before a client chunk was read is numbered before it, and
// one it sent after, after it, though both were in the socket when TLS read.
func TestFromClientOrdersTLSRecordsByWhenTheyReachedTheSocket(t *testing.T) {
	t.Parallel()
	u, destRaw := tcpUpstream(t)
	cert := selfSigned(t)
	dest := tls.Server(destRaw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13,
		SessionTicketsDisabled: true})
	client := tls.Client(u, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) //nolint:gosec // test server
	handshook := make(chan error, 1)
	go func() { handshook <- dest.Handshake() }()
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-handshook; err != nil {
		t.Fatal(err)
	}
	if Of(client) != u {
		t.Fatal("Of does not find the Upstream under TLS")
	}
	base := u.socket.received()
	// A TLS 1.3 record is its plaintext, a content type byte, a 5 byte header
	// and a 16 byte tag.
	const early, answer, overhead = "HTTP/1.1 408 Request Timeout\r\n\r\n", "HTTP/1.1 200 OK\r\n\r\n", 22
	if _, err := dest.Write([]byte(early)); err != nil {
		t.Fatal(err)
	}
	awaitSent(t, u, base+uint64(len(early)+overhead))
	req := u.FromClient()
	if _, err := dest.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	awaitSent(t, u, base+uint64(len(early)+len(answer)+2*overhead))
	if got := readString(t, client, 1024); got != early {
		t.Fatalf("the first Read returned %q, want %q", got, early)
	}
	first := u.FromDest()
	if got := readString(t, client, 1024); got != answer {
		t.Fatalf("the second Read returned %q, want %q", got, answer)
	}
	second := u.FromDest()
	if !before(first, req) || !before(req, second) {
		t.Fatalf("408 %d, request %d, answer %d: want 408 < request < answer", first, req, second)
	}
}

// A socket's count is the one source of what the destination had sent, for
// the life of the connection: once the proxy has closed the socket and it can
// no longer be asked, it gives the last count it gave, never what Read has
// returned instead.
func TestASocketThatCannotBeAskedGivesItsLastCount(t *testing.T) {
	t.Parallel()
	u, dest := tcpUpstream(t)
	if _, err := dest.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	awaitSent(t, u, 3) // nothing has read them
	_ = u.Close()
	u.FromClient()
	if len(u.cuts) != 1 || u.cuts[0].at != 3 {
		t.Fatalf("cuts %+v, want one at the socket's last count, 3", u.cuts)
	}
}

// On Linux a TCP connection is numbered by its socket's count, always: one
// whose socket cannot be reached is refused where its Upstream is made, never
// numbered by what Read returned instead, the rule off Linux, with nothing to
// say the rule changed.
func TestATCPConnWhoseSocketCannotBeReachedIsRefused(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("an Upstream was made for a TCP conn with no socket, numbering by what was read")
		}
	}()
	NewUpstream(&net.TCPConn{}) // no socket under it: SyscallConn fails
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dest"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// What numbering costs per chunk: a client chunk asks the socket for its count
// (one getsockopt), a destination chunk takes a lock.
func BenchmarkFromClient(b *testing.B) {
	u, _ := tcpUpstream(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		u.FromClient()
	}
}

func BenchmarkFromDest(b *testing.B) {
	u, _ := tcpUpstream(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		u.FromDest()
	}
}
