package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/synhold"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// unreachedSink is a TopologySink that keeps the unreached rows it is told.
type unreachedSink struct {
	mu   sync.Mutex
	rows []models.UnreachedRow
}

func (*unreachedSink) NoteLive(models.LiveRow)     {}
func (*unreachedSink) NoteMocked(models.MockedRow) {}
func (s *unreachedSink) NoteUnreached(r models.UnreachedRow) {
	s.mu.Lock()
	s.rows = append(s.rows, r)
	s.mu.Unlock()
}

func (s *unreachedSink) got() []models.UnreachedRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]models.UnreachedRow(nil), s.rows...)
}

// dialTimeoutErr is a dial error that says it timed out.
type dialTimeoutErr struct{}

func (dialTimeoutErr) Error() string { return "i/o timeout" }
func (dialTimeoutErr) Timeout() bool { return true }

// dialErr is an error as net's dial returns one for errno.
func dialErr(errno syscall.Errno) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
}

// A failed dial is a dependency's answer only when the destination gave it:
// refused, unreachable, or never answered. The proxy's own failures (a
// cancelled dial, anything else) say nothing of the destination.
func TestUnreachedAnswerIsTheDestinationsAnswer(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
		ok   bool
	}{
		{"refused", dialErr(syscall.ECONNREFUSED), models.UnreachedRefused, true},
		{"host unreachable", dialErr(syscall.EHOSTUNREACH), models.UnreachedUnreachable, true},
		{"network unreachable", dialErr(syscall.ENETUNREACH), models.UnreachedUnreachable, true},
		{"timed out", &net.OpError{Op: "dial", Net: "tcp", Err: dialTimeoutErr{}}, models.UnreachedNoAnswer, true},
		{"wrapped timeout", fmt.Errorf("dialing: %w", dialTimeoutErr{}), models.UnreachedNoAnswer, true},
		{"nil", nil, "", false},
		{"cancelled", context.Canceled, "", false},
		{"the proxy's own limit", dialErr(syscall.EMFILE), "", false},
		{"anything else", errors.New("no such socket"), "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := unreachedAnswer(c.err)
			if got != c.want || ok != c.ok {
				t.Fatalf("unreachedAnswer(%v) = %q, %v; want %q, %v", c.err, got, ok, c.want, c.ok)
			}
		})
	}
}

// Only a keploy mock record under the proven switch notes an unreached
// dependency: not keploy record, not a replay, not without the switch, and
// not with no sink.
func TestAnUnreachedDependencyIsNotedOnlyInAProvenMockRecord(t *testing.T) {
	for _, c := range []struct {
		name     string
		proven   string
		mockMode bool
		session  *agent.Session
		noSink   bool
		want     bool
	}{
		{"a proven mock record", "1", true, &agent.Session{Mode: models.MODE_RECORD}, false, true},
		{"without the switch", "", true, &agent.Session{Mode: models.MODE_RECORD}, false, false},
		{"keploy record", "1", false, &agent.Session{Mode: models.MODE_RECORD}, false, false},
		{"a replay", "1", true, &agent.Session{Mode: models.MODE_TEST}, false, false},
		{"no session", "1", true, nil, false, false},
		{"no sink", "1", true, &agent.Session{Mode: models.MODE_RECORD}, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", c.proven)
			p := &Proxy{logger: zap.NewNop(), mockMode: c.mockMode}
			if c.session != nil {
				p.setSession(c.session)
			}
			s := &unreachedSink{}
			if !c.noSink {
				p.SetTopologySink(s)
			}
			p.noteDialFailed("127.0.0.1:5432", dialErr(syscall.ECONNREFUSED), 3)
			got := s.got()
			if !c.want {
				if len(got) != 0 {
					t.Fatalf("noted %+v; want nothing", got)
				}
				return
			}
			want := models.UnreachedRow{Addr: "127.0.0.1:5432", Answer: models.UnreachedRefused, Instance: 3}
			if len(got) != 1 || got[0] != want {
				t.Fatalf("noted %+v; want [%+v]", got, want)
			}
		})
	}
}

// A dial that failed for no reason of the destination's is noted nowhere.
func TestAProxyFailureIsNoUnreachedDependency(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p := &Proxy{logger: zap.NewNop(), mockMode: true}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	s := &unreachedSink{}
	p.SetTopologySink(s)
	p.noteDialFailed("127.0.0.1:5432", context.Canceled, 0)
	if got := s.got(); len(got) != 0 {
		t.Fatalf("noted %+v for a cancelled dial", got)
	}
}

// A mock record whose held handshake finds the dependency refusing notes
// it, with the instance of the worker that connected; a mock.real
// destination that refuses is its test's REAL row's, never an unreached
// dependency.
func TestARefusingDependencyOfAMockRecordIsNotedUnreached(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	refused := testnet.RefusedAddr(t, "tcp")
	for _, c := range []struct {
		name  string
		label string
		want  []models.UnreachedRow
	}{
		{"a dependency", "", []models.UnreachedRow{{Addr: refused, Answer: models.UnreachedRefused, Instance: 2}}},
		{"a mock.real destination", "db", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop(), mockMode: true}
			p.setSession(&agent.Session{Mode: models.MODE_RECORD})
			s := &unreachedSink{}
			p.SetTopologySink(s)
			dest := ipv4Dest(t, refused)
			dest.Instance = 2
			d := p.dialHeld(context.Background(), fakeHandshakeDest{dest: dest}, appEnd, proxyEnd, dest, c.label)
			if d.Outcome != synhold.Refuse {
				t.Fatalf("decision %+v, want refuse", d)
			}
			if got := s.got(); fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("noted %+v; want %+v", got, c.want)
			}
		})
	}
}
