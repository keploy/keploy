package generic

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// answeredWithin is how soon after it could be a request is answered in these
// rows: 500 ms, an absolute bound, so a window or a read-on deadline that
// grew to a second fails them. A request waits about one window (10 ms).
const answeredWithin = 500 * time.Millisecond

// tlsApp is the application's end beneath its TLS, or its end over TCP.
// While withholding, what TLS writes through it is kept (kept) instead of
// sent, for the row to send.
type tlsApp struct {
	net.Conn
	mu       sync.Mutex
	withhold bool
	kept     []byte
	// sent is what it has sent.
	sent int
}

func (c *tlsApp) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.withhold {
		c.kept = append(c.kept, p...)
		return len(p), nil
	}
	n, err := c.Conn.Write(p)
	c.sent += n
	return n, err
}

func (c *tlsApp) sentSoFar() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent
}

// socketWatch is the proxy's socket beneath the layers above it (TLS, a
// buffered reader). Armed for a request, it counts the reads that went to
// the socket after the request had all come up from it (want bytes on the
// wire), the parser having asked the socket what it holds, with a deadline
// still ahead: each a wait on an empty socket for bytes the request does not
// have. The parser's answer disarms it.
type socketWatch struct {
	net.Conn

	mu       sync.Mutex
	deadline time.Time
	armed    bool
	asked    bool
	taken    int
	want     int
	waits    int
}

func (s *socketWatch) NetConn() net.Conn {
	s.mu.Lock()
	if s.armed {
		s.asked = true
	}
	s.mu.Unlock()
	return s.Conn
}

func (s *socketWatch) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadline = t
	s.mu.Unlock()
	return s.Conn.SetReadDeadline(t)
}

func (s *socketWatch) SetDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadline = t
	s.mu.Unlock()
	return s.Conn.SetDeadline(t)
}

func (s *socketWatch) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.armed && s.asked && s.want > 0 && s.taken >= s.want && time.Now().Before(s.deadline) {
		s.waits++
	}
	s.mu.Unlock()
	n, err := s.Conn.Read(p)
	s.mu.Lock()
	if s.armed {
		s.taken += n
	}
	s.mu.Unlock()
	return n, err
}

// arm starts the count, before the request's first bytes are sent.
func (s *socketWatch) arm() {
	s.mu.Lock()
	s.armed, s.asked, s.taken, s.want, s.waits = true, false, 0, 0, 0
	s.mu.Unlock()
}

// expect sets the request's bytes on the wire, once all are sent.
func (s *socketWatch) expect(want int) {
	s.mu.Lock()
	s.want = want
	s.mu.Unlock()
}

func (s *socketWatch) disarm() {
	s.mu.Lock()
	s.armed = false
	s.mu.Unlock()
}

func (s *socketWatch) waited() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waits
}

// tlsPair is a loopback TCP pair with TLS over it, handshaken: the app's TLS
// end, the conn beneath it, the proxy's TLS end, the proxy's socket and the
// watch on it, beneath the proxy's other layers. With
// fullRecords the app writes 16 KiB records, as most TLS stacks do; without,
// Go's records grow from about one segment. With buffered the proxy's TLS
// reads its socket through a 4 KiB buffered reader (pUtil.Conn), as the
// proxy stacks it.
func tlsPair(t *testing.T, fullRecords, buffered bool) (*tls.Conn, *tlsApp, *tls.Conn, net.Conn, *socketWatch) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "generic"}, DNSNames: []string{"generic"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	a, p := tcpPair(t)
	watch := &socketWatch{Conn: p}
	var under net.Conn = watch
	if buffered {
		under = &pUtil.Conn{Conn: watch, Reader: bufio.NewReader(watch), Logger: zap.NewNop()}
	}
	appSide := &tlsApp{Conn: a}
	srv := tls.Server(under, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	cli := tls.Client(appSide, &tls.Config{InsecureSkipVerify: true, ServerName: "generic", DynamicRecordSizingDisabled: fullRecords})
	errc := make(chan error, 1)
	go func() { errc <- srv.Handshake() }()
	if err := cli.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	return cli, appSide, srv, p, watch
}

// tlsHeld is the proxy's end of a connection (TLS over TCP, or TCP) whose
// parser is held once it has read cut bytes of a request. After the parser's
// first answer, its first read waits until the app has written (ready) and
// the socket's unread count has stood still for 5 ms. The read after the cut
// waits until three windows have passed since the deadline last set and the
// app has written the rest (wrote, the count still again), then reads for
// real: its deadline has passed, so TLS gives what it holds whole or times
// out on the socket, as a held parser's read does. released is when that
// read went on. It gives the connection beneath (NetConn), as the proxy's
// wrappers do. Its answers disarm watch, when it has one.
type tlsHeld struct {
	net.Conn
	raw          net.Conn
	watch        *socketWatch
	cut          int
	ready, wrote chan struct{}

	mu       sync.Mutex
	answered bool
	// phase: 0 to the cut, 1 the hold, 2 after it.
	phase    int
	read     int
	deadline time.Time
	released time.Time
}

func (c *tlsHeld) NetConn() net.Conn { return c.Conn }

func (c *tlsHeld) Write(p []byte) (int, error) {
	if c.watch != nil {
		c.watch.disarm()
	}
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	c.answered = true
	c.mu.Unlock()
	return n, err
}

func (c *tlsHeld) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

// still: ch is closed and the socket's unread count stood still for 5 ms.
func (c *tlsHeld) still(ch chan struct{}) func() bool {
	return func() bool {
		select {
		case <-ch:
		default:
			return false
		}
		n := pUtil.UnreadBytes(c.raw)
		time.Sleep(5 * time.Millisecond)
		return pUtil.UnreadBytes(c.raw) == n
	}
}

func (c *tlsHeld) at(phase int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.phase >= phase
}

func (c *tlsHeld) Read(p []byte) (int, error) {
	c.mu.Lock()
	answered, phase, read := c.answered, c.phase, c.read
	c.mu.Unlock()
	if answered && phase == 0 && read == 0 && !until(c.still(c.ready)) {
		return 0, errors.New("the request never reached the socket")
	}
	if phase == 1 {
		passed := func() bool {
			c.mu.Lock()
			d := c.deadline
			c.mu.Unlock()
			return time.Now().After(d.Add(3 * quietWindow))
		}
		if !until(passed) || !until(c.still(c.wrote)) {
			return 0, errors.New("the deadline never passed, or the rest never came")
		}
		c.mu.Lock()
		c.phase, c.released = 2, time.Now()
		c.mu.Unlock()
	}
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	if c.answered && c.phase == 0 {
		c.read += n
		if c.read >= c.cut {
			c.phase = 1
		}
	}
	c.mu.Unlock()
	return n, err
}

// A request whose parser is held past its window after its first read, the
// rest in the socket or already taken into the TLS layer, is answered whole,
// over TLS as over TCP, through a buffered reader as the proxy stacks one:
// the parser reads on until the socket has given what it held when the
// parser looked, then reads what the layers above it hold, without waiting:
// once the request has all come up from the socket, no read waits on it.
// Counting the plaintext a read returned against the socket's bytes, a
// request whose records the TLS layer had none of when the parser looked
// never reached them (each record is longer on the wire), and the parser
// waited out its read-on deadline on the empty socket.
// Counting the plaintext a read returned against the bytes the socket held,
// it broke off once a record's plaintext outran them: the record's bytes
// TLS had read ahead of the look are in neither count, and the request's
// last record was read as a request of its own, which missed.
func TestAHeldRequestOverTLSIsAnsweredWhole(t *testing.T) {
	kinds := []struct {
		name                      string
		overTLS, full, buf, split bool
	}{
		{"tcp", false, false, false, false},
		{"tls, grown records", true, false, false, false},
		{"tls, full records", true, true, false, false},
		{"tls, full records, the rest after the first read", true, true, false, true},
		{"tls through a buffered reader, grown records", true, false, true, false},
		{"tls through a buffered reader, full records", true, true, true, false},
	}
	// Go's records grow (33308 and 33808 bytes end just past one of them);
	// full records are 16 KiB (32 KiB + 1 ends one byte into the third).
	for _, size := range []int{20000, 33000, 32<<10 + 1, 33308, 33808, 40 << 10} {
		req := varied(size, true)
		recorded := recordedAs("mock-big", req)
		recorded.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "BIGOK"}}}}
		store := eofStore{mocks: []*models.Mock{recordedExchange("mock-ping", "PING", "PONG"), recorded}}
		for _, k := range kinds {
			t.Run(fmt.Sprintf("%d bytes, %s", size, k.name), func(t *testing.T) {
				eachSwitch(t, func(t *testing.T) {
					var app net.Conn
					var appSide *tlsApp
					conn := &tlsHeld{cut: 1, ready: make(chan struct{}), wrote: make(chan struct{})}
					if k.overTLS {
						cli, side, srv, raw, watch := tlsPair(t, k.full, k.buf)
						app, appSide, conn.Conn, conn.raw, conn.watch = cli, side, srv, raw, watch
					} else {
						a, p := tcpPair(t)
						appSide, conn.watch = &tlsApp{Conn: a}, &socketWatch{Conn: p}
						app, conn.Conn, conn.raw = appSide, conn.watch, p
					}
					done := make(chan error, 1)
					go func() {
						done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, store, models.OutgoingOptions{})
					}()
					readAnswer(t, app, "PONG")
					conn.watch.arm()
					onWire := appSide.sentSoFar()
					sentAt := time.Now()
					if k.split {
						// One full record; the rest once the parser has read
						// it, so the TLS layer holds none of the rest when the
						// parser looks.
						if _, err := app.Write(req[:16<<10]); err != nil {
							t.Fatal(err)
						}
						close(conn.ready)
						if !until(func() bool { return conn.at(1) }) {
							t.Fatal("the parser never read the first record")
						}
						if _, err := app.Write(req[16<<10:]); err != nil {
							t.Fatal(err)
						}
					} else {
						if _, err := app.Write(req); err != nil {
							t.Fatal(err)
						}
						close(conn.ready)
					}
					conn.watch.expect(appSide.sentSoFar() - onWire)
					close(conn.wrote)
					readAnswer(t, app, "BIGOK")
					if took := time.Since(sentAt); took > answeredWithin {
						t.Fatalf("answered %v after it was sent", took)
					}
					if n := conn.watch.waited(); n > 0 {
						t.Fatalf("%d reads waited on the socket once the request had all come up from it", n)
					}
					_ = app.Close()
					awaitEnd(t, done, "a connection the application closed")
				})
			})
		}
	}
}

// answering is m answering with answer.
func answering(m *models.Mock, answer string) *models.Mock {
	m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: answer}}}}
	return m
}

// A TLS record that has not all come when the parser reads on is not waited
// for past one window: the parser, held past the window after the request's
// first two records, reads on to what the socket held, the last record's
// first bytes, and the record's read waits for its rest until the deadline
// it read on with, one window, then the request is answered without it. The
// record is the next request's once it comes. A read-on deadline of a second
// answered a second late, and of an hour not at all.
func TestATLSRecordStillComingAsTheParserReadsOnIsNotWaitedFor(t *testing.T) {
	req := varied(33000, true)
	head, tail := req[:32<<10], req[32<<10:]
	store := eofStore{mocks: []*models.Mock{
		recordedExchange("mock-ping", "PING", "PONG"),
		answering(recordedAs("mock-head", head), "HEADOK"),
		answering(recordedAs("mock-tail", tail), "TAILOK"),
	}}
	for _, buf := range []bool{false, true} {
		t.Run(fmt.Sprintf("through a buffered reader: %v", buf), func(t *testing.T) {
			eachSwitch(t, func(t *testing.T) {
				app, appSide, srv, raw, _ := tlsPair(t, true, buf)
				conn := &tlsHeld{Conn: srv, raw: raw, cut: len(head), ready: make(chan struct{}), wrote: make(chan struct{})}
				done := make(chan error, 1)
				go func() {
					done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), conn, nil, store, models.OutgoingOptions{})
				}()
				readAnswer(t, app, "PONG")
				// Two full records.
				if _, err := app.Write(head); err != nil {
					t.Fatal(err)
				}
				close(conn.ready)
				if !until(func() bool { return conn.at(1) }) {
					t.Fatal("the parser never read the request's first two records")
				}
				// The last record, all but its last 10 bytes.
				appSide.mu.Lock()
				appSide.withhold = true
				appSide.mu.Unlock()
				if _, err := app.Write(tail); err != nil {
					t.Fatal(err)
				}
				appSide.mu.Lock()
				record := appSide.kept
				appSide.withhold, appSide.kept = false, nil
				appSide.mu.Unlock()
				if _, err := appSide.Conn.Write(record[:len(record)-10]); err != nil {
					t.Fatal(err)
				}
				close(conn.wrote)
				readAnswer(t, app, "HEADOK")
				conn.mu.Lock()
				released := conn.released
				conn.mu.Unlock()
				if took := time.Since(released); took > answeredWithin {
					t.Fatalf("answered %v after the parser read on", took)
				}
				if _, err := appSide.Conn.Write(record[len(record)-10:]); err != nil {
					t.Fatal(err)
				}
				readAnswer(t, app, "TAILOK")
				_ = app.Close()
				awaitEnd(t, done, "a connection the application closed")
			})
		})
	}
}

// The window is 10 ms. Every request after a connection's first waits one
// window from its first bytes for its answer, so a longer window adds to
// each; a shorter one cuts a request whose bytes pause for less than 10 ms.
func TestARequestsWindowIsTenMilliseconds(t *testing.T) {
	if quietWindow != 10*time.Millisecond {
		t.Fatalf("the window is %v; want 10ms", quietWindow)
	}
}
