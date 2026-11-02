//go:build linux

package linux

import (
	"context"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// Where the hold cannot start (here the main object is not loaded; on a
// kernel without sk_lookup, the ingress object does not load), keploy says
// once, at WARN, the window it leaves: a call between the app's listen and
// keploy's forwarder is refused and not recorded. It then holds nothing, and
// the ingress manager falls back to the forwarder alone.
func TestAHoldThatCannotStartSaysTheWindowItLeaves(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	h := NewHooks(zap.New(core), &config.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.startIngressHold(ctx, t.TempDir())
	if h.IngressHeld() || h.IngressParkListener() != nil || h.IngressLookupCounts() != nil {
		t.Fatal("a hold that could not start reports one")
	}
	if _, ok := h.MovedPort(8080); ok {
		t.Fatal("MovedPort answered with nothing held")
	}
	h.RegisterIngressForwarder(8080, nil) // no-ops with nothing held
	h.UnregisterIngressForwarder(8080)
	warns := logs.FilterMessageSnippet("keploy cannot hold the application's port from its bind").All()
	if len(warns) != 1 {
		t.Fatalf("%d warnings, want one: %v", len(warns), logs.All())
	}
	w := warns[0]
	if !strings.Contains(w.Message, "refused") || !strings.Contains(w.Message, "not recorded") || !strings.Contains(w.Message, "Started ingress forwarding") {
		t.Fatalf("the warning does not say the window: %q", w.Message)
	}
	fields := w.ContextMap()
	if fields["next_step"] == nil || fields["kernel"] == nil || fields["error"] == nil {
		t.Fatalf("the warning lacks its kernel, error or next_step: %v", fields)
	}
}

// The bind hooks note what the app binds (structs.FlagIngressHold) only for
// a hold that is on: set when it is, cleared when it is not, even if a
// downstream build's customizer set it, and no other bit is touched.
func TestTheHoldFlagIsSetOnlyForAHoldThatIsOn(t *testing.T) {
	others := structs.FlagKernelPortAlloc | structs.FlagClassifies
	if got := ingressHoldFlag(others, true); got != others|structs.FlagIngressHold {
		t.Fatalf("held: flags %#x, want %#x", got, others|structs.FlagIngressHold)
	}
	if got := ingressHoldFlag(others|structs.FlagIngressHold, false); got != others {
		t.Fatalf("not held: flags %#x, want %#x", got, others)
	}
	if structs.FlagIngressHold != 1<<5 {
		t.Fatalf("FlagIngressHold is %#x; KEPLOY_FLAG_INGRESS_HOLD is bit 5", structs.FlagIngressHold)
	}
}

// The ingress object reads the notes the main object's bind hooks write, so
// every map the two objects both declare is the main object's, handed to the
// ingress object in place of its own (ingressShared). A map left out would
// load as the ingress object's own empty copy: the lookup would find no note
// and hold nothing, silently. Each shared map is declared alike in both, or
// the replacement is refused at load.
func TestTheIngressObjectSharesEveryMapItHasWithTheMainObject(t *testing.T) {
	ing, err := loadIngress()
	if err != nil {
		t.Fatal(err)
	}
	main, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	var both []string
	for name := range ing.Maps {
		if _, ok := main.Maps[name]; ok {
			both = append(both, name)
		}
	}
	sort.Strings(both)
	want := append([]string(nil), ingressShared...)
	sort.Strings(want)
	if strings.Join(both, ",") != strings.Join(want, ",") {
		t.Fatalf("maps in both objects %v, ingressShared %v", both, want)
	}
	for _, name := range ingressShared {
		a, b := ing.Maps[name], main.Maps[name]
		if a.Type != b.Type || a.KeySize != b.KeySize || a.ValueSize != b.ValueSize || a.MaxEntries != b.MaxEntries || a.Flags != b.Flags {
			t.Errorf("%s: ingress object %v %d/%d/%d flags %#x, main object %v %d/%d/%d flags %#x", name,
				a.Type, a.KeySize, a.ValueSize, a.MaxEntries, a.Flags, b.Type, b.KeySize, b.ValueSize, b.MaxEntries, b.Flags)
		}
	}
}

// A hold whose main object lacks one of the shared maps does not start, and
// says which map it lacks; nothing of the ingress object is loaded first.
func TestAHoldWithoutOneOfTheMainObjectsMapsNamesIt(t *testing.T) {
	for _, missing := range ingressShared {
		t.Run(missing, func(t *testing.T) {
			h := NewHooks(zap.NewNop(), &config.Config{})
			// Not loaded in the kernel: the guard only checks that each is there.
			h.objects.AppPortInfo, h.objects.AppPortBinds = &ebpf.Map{}, &ebpf.Map{}
			h.objects.AppBindSocks, h.objects.PendingHolds = &ebpf.Map{}, &ebpf.Map{}
			switch missing {
			case "app_port_info":
				h.objects.AppPortInfo = nil
			case "app_port_binds":
				h.objects.AppPortBinds = nil
			case "app_bind_socks":
				h.objects.AppBindSocks = nil
			case "pending_holds":
				h.objects.PendingHolds = nil
			default:
				t.Fatalf("ingressShared names %s, which this test does not know", missing)
			}
			hold, err := h.loadIngressHold(t.TempDir())
			if hold != nil || err == nil || !strings.Contains(err.Error(), "the main object's "+missing+" is not loaded") {
				t.Fatalf("hold %v, err %v; want none, and an error naming %s", hold, err, missing)
			}
		})
	}
}

// The park listener goes into the sockmap by its socket: a map that is not
// there, or a listener that is not a TCP one, is refused before the map is
// touched.
func TestTheParkListenerGoesInOnlyAsATCPListenerIntoALoadedMap(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcp.Close() }()
	if err := putListener(nil, 0, tcp); err == nil || !strings.Contains(err.Error(), "the map is not loaded") {
		t.Fatalf("no map: %v", err)
	}
	unix, err := net.Listen("unix", filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close() }()
	if err := putListener(&ebpf.Map{}, 0, unix); err == nil || !strings.Contains(err.Error(), "*net.UnixListener, not a *net.TCPListener") {
		t.Fatalf("a unix listener: %v", err)
	}
}

// Each shape of the app's bind takes the IPv6 calls its note says, as the
// lookup matches them (ingress_bound_here), so the manager checks IPv6 only
// where the hold takes IPv6 calls: none for an IPv4 socket or an IPv6 one on
// a v4-mapped address (the bind hooks note both as IPv4), :: for an IPv6
// socket on ::, dual-stack or IPV6_V6ONLY, and the address for one on ::1.
func TestEachBindShapeTakesTheIPv6ItsNoteSays(t *testing.T) {
	for _, c := range []struct {
		name string
		key  appBindKey
		want string
	}{
		{"0.0.0.0", appBindKey{Kind: appBindV4}, ""},
		{"::ffff:127.0.0.1", appBindKey{Kind: appBindV4, Addr: [16]byte{127, 0, 0, 1}}, ""},
		{":: without IPV6_V6ONLY", appBindKey{Kind: appBindV6Dual}, "::"},
		{":: with IPV6_V6ONLY", appBindKey{Kind: appBindV6}, "::"},
		{"::1", appBindKey{Kind: appBindV6, Addr: netip.IPv6Loopback().As16()}, "::1"},
		{"an unknown kind", appBindKey{Kind: 7}, ""},
	} {
		a, ok := ipv6Taken(c.key)
		got := ""
		if ok {
			got = a.String()
		}
		if got != c.want || ok != (c.want != "") {
			t.Errorf("a bind of %s: takes IPv6 %q (%v), want %q", c.name, got, ok, c.want)
		}
	}
}

// A hold that cannot read the notes says so, so the manager checks all of
// IPv6 rather than none; with no hold there is nothing to check.
func TestAHoldWithoutItsNotesSaysSo(t *testing.T) {
	h := NewHooks(zap.NewNop(), &config.Config{})
	if a, err := h.AppIPv6Binds(8080); a != nil || err != nil {
		t.Fatalf("no hold: %v, %v; want nothing", a, err)
	}
	h.ingress = &ingressHold{}
	if _, err := h.AppIPv6Binds(8080); err == nil {
		t.Fatal("a hold without its notes map read them")
	}
}
