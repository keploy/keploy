//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// sysctlDir writes a net/ipv4 sysctl directory for readBindMovePorts: the
// two files as /proc/sys gives them, each one left out when its content is
// "-".
func sysctlDir(t *testing.T, portRange, unpriv string) string {
	t.Helper()
	dir := t.TempDir()
	for name, v := range map[string]string{"ip_local_port_range": portRange, "ip_unprivileged_port_start": unpriv} {
		if v == "-" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The ports keploy/ebpf's find_free_port moves a bind to, from the
// network namespace's two sysctls: above the ephemeral range when it ends
// below 65535, else below it from the higher of 1024 and the unprivileged
// start, else none.
func TestTheBindMovePortsAreFindFreePortsChoice(t *testing.T) {
	rows := []struct {
		name, portRange, unpriv string
		first, last             int
	}{
		{"the default range: above it", "32768\t60999", "1024", 61000, 65535},
		{"one port above the range", "32768\t65534", "1024", 65535, 65535},
		{"a range to 65535: below it, from 1024", "40000\t65535", "1024", 1024, 39999},
		{"a range to 65535 with the unprivileged ports from 80: from 1024", "1025\t65535", "80", 1024, 1024},
		{"a range to 65535 with the unprivileged ports from 0: from 1024", "1025\t65535", "0", 1024, 1024},
		{"a range to 65535 with the unprivileged ports from 20000: from 20000", "40000\t65535", "20000", 20000, 39999},
		{"a range to 65535 with the unprivileged ports from 1025: from 1025", "1026\t65535", "1025", 1025, 1025},
		{"no room from the range", "1024\t65535", "1024", 0, 0},
		{"no room from the range with the unprivileged ports from 80", "1024\t65535", "80", 0, 0},
		{"no room from a raised unprivileged start", "20000\t65535", "20000", 0, 0},
		{"the unprivileged start does not move the ports above the range", "20000\t60999", "20000", 61000, 65535},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := readBindMovePorts(sysctlDir(t, r.portRange, r.unpriv))
			if err != nil {
				t.Fatal(err)
			}
			if p.first != r.first || p.last != r.last {
				t.Fatalf("range %q, unprivileged from %s: the binds move to %d-%d, want %d-%d", r.portRange, r.unpriv, p.first, p.last, r.first, r.last)
			}
		})
	}
}

// A sysctl that cannot be read, or reads as no port, is an error: the
// Warn then fails closed.
func TestTheBindMovePortsFailOnAnUnreadableSysctl(t *testing.T) {
	rows := []struct{ name, portRange, unpriv, want string }{
		{"no range", "-", "1024", "read net.ipv4.ip_local_port_range"},
		{"no unprivileged start", "32768\t60999", "-", "read net.ipv4.ip_unprivileged_port_start"},
		{"one port in the range", "32768", "1024", "want 2 port(s)"},
		{"three ports in the range", "1 2 3", "1024", "want 2 port(s)"},
		{"two unprivileged starts", "32768\t60999", "1024 1025", "want 1 port(s)"},
		{"no unprivileged start in the file", "32768\t60999", "", "want 1 port(s)"},
		{"a word for a port", "32768\tmany", "1024", `"many" is no port`},
		{"a port above 65535", "32768\t65536", "1024", `"65536" is no port`},
		{"a negative port", "-1\t60999", "1024", `"-1" is no port`},
		{"a negative unprivileged start", "32768\t60999", "-5", `"-5" is no port`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := readBindMovePorts(sysctlDir(t, r.portRange, r.unpriv))
			if err == nil || !strings.Contains(err.Error(), r.want) {
				t.Fatalf("range %q, unprivileged %q: got %+v, %v; want an error with %q", r.portRange, r.unpriv, p, err, r.want)
			}
		})
	}
	// The ports at each end of what a sysctl may hold are read.
	p, err := readBindMovePorts(sysctlDir(t, "0\t65535", "0"))
	if err != nil || p.lo != 0 || p.hi != 65535 || p.unpriv != 0 {
		t.Fatalf("range 0-65535, unprivileged from 0: got %+v, %v", p, err)
	}
}

// warnBindFallback says once where the app's binds go when a post_bind
// hook did not attach: where they move, and that one which finds each port
// it tries there listened on is left alone and not recorded; or, with no
// room or a sysctl it cannot read, that they are left on the app's ports and
// those are not recorded.
func TestTheBindFallbackWarnSaysWhereTheBindsGo(t *testing.T) {
	attachErr := errors.New("operation not supported")
	rows := []struct {
		name, portRange, unpriv string
		want, not               []string
	}{
		{"room", "32768\t60999", "1024",
			[]string{"the post_bind4 cgroup hook did not attach", "a port in 61000-65535, outside net.ipv4.ip_local_port_range 32768-60999",
				"A bind that finds each port it tries there listened on is left on the port the app names, and that port's incoming traffic is not recorded"},
			[]string{"no port is left", "each bind is left", "cannot say"}},
		{"no room from the range", "1024\t65535", "1024",
			[]string{"no port is left outside net.ipv4.ip_local_port_range 1024-65535 from 1024 up", "net.ipv4.ip_unprivileged_port_start 1024)",
				"each bind is left on the port the app names", "incoming traffic is not recorded"},
			[]string{"a port in"}},
		{"no room from the range with the unprivileged ports from 80", "1024\t65535", "80",
			[]string{"no port is left outside net.ipv4.ip_local_port_range 1024-65535 from 1024 up", "net.ipv4.ip_unprivileged_port_start 80)",
				"incoming traffic is not recorded"},
			[]string{"a port in"}},
		{"no room from a raised unprivileged start", "20000\t65535", "20000",
			[]string{"no port is left outside net.ipv4.ip_local_port_range 20000-65535 from 20000 up", "net.ipv4.ip_unprivileged_port_start 20000)",
				"incoming traffic is not recorded"},
			[]string{"a port in"}},
		{"an unreadable sysctl", "32768\t60999", "-",
			[]string{"cannot say whether any port is left", "read net.ipv4.ip_unprivileged_port_start", "as not recorded"},
			[]string{"a port in"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			warnBindFallback(zap.New(core), "post_bind4", attachErr, sysctlDir(t, r.portRange, r.unpriv))
			all := logs.All()
			if len(all) != 1 {
				t.Fatalf("%d warnings, want 1: %v", len(all), all)
			}
			msg := all[0].Message
			for _, w := range r.want {
				if !strings.Contains(msg, w) {
					t.Errorf("the warning %q does not say %q", msg, w)
				}
			}
			for _, n := range r.not {
				if strings.Contains(msg, n) {
					t.Errorf("the warning %q says %q", msg, n)
				}
			}
			if got := all[0].ContextMap()["error"]; got != attachErr.Error() {
				t.Errorf("the warning's error is %v, want the attach's, %q", got, attachErr)
			}
		})
	}
}

// fakeLink is what refusingAttach hands back for a hook it attaches: no
// link at all, its Close noted.
type fakeLink struct {
	link.Link
	closed bool
}

func (l *fakeLink) Close() error { l.closed = true; return nil }

// refusingAttach is an attach for attachIngress that attaches nothing: it
// refuses the attach types in refuse, each with an error of its own, kept
// by attach type in refused, and hands back a fakeLink, kept by attach type
// in got, for each other.
func refusingAttach(refuse ...ebpf.AttachType) (attach func(link.CgroupOptions) (link.Link, error), got map[ebpf.AttachType]*fakeLink, refused map[ebpf.AttachType]error) {
	got, refused = map[ebpf.AttachType]*fakeLink{}, map[ebpf.AttachType]error{}
	for _, a := range refuse {
		refused[a] = fmt.Errorf("attach %v: operation not supported", a)
	}
	return func(o link.CgroupOptions) (link.Link, error) {
		if err, ok := refused[o.Attach]; ok {
			return nil, err
		}
		l := &fakeLink{}
		got[o.Attach] = l
		return l, nil
	}, got, refused
}

// ownRangeSaid is what the warning says of the agent's own network
// namespace's ephemeral range, read here without readBindMovePorts: "LO-HI",
// or, when it or ip_unprivileged_port_start cannot be read, that it cannot
// say.
func ownRangeSaid(t *testing.T) string {
	t.Helper()
	if _, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start"); err != nil {
		return "cannot say whether any port is left"
	}
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return "cannot say whether any port is left"
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		t.Fatalf("this namespace's net.ipv4.ip_local_port_range is %q", b)
	}
	return "net.ipv4.ip_local_port_range " + f[0] + "-" + f[1]
}

// A post_bind hook that does not attach is kept, by name, with why, and
// said only once the agent's flags are final: the warning names it and
// reads the agent's own network namespace's sysctls, and under
// FlagObserveOnly, where the bind hooks move no bind, nothing is warned.
// When post_bind6 does not attach, post_bind4's half is detached; the
// kernel picks the moved port only when both attached.
func TestTheBindFallbackIsSaidOnceTheFlagsAreFinal(t *testing.T) {
	for _, r := range []struct {
		name   string
		refuse []ebpf.AttachType
		hook   string // the hook refused; "" when both attach
	}{
		{"both attach", nil, ""},
		{"post_bind4 refused", []ebpf.AttachType{ebpf.AttachCGroupInet4PostBind}, "post_bind4"},
		{"post_bind6 refused", []ebpf.AttachType{ebpf.AttachCGroupInet6PostBind}, "post_bind6"},
	} {
		t.Run(r.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)
			h := NewHooks(zap.New(core), &config.Config{})
			attach, got, refused := refusingAttach(r.refuse...)
			if err := h.attachIngress("/sys/fs/cgroup/app", bpfObjects{}, attach); err != nil {
				t.Fatalf("attachIngress: %v, want a refused post_bind hook not to fail it", err)
			}
			if n := logs.Len(); n != 0 {
				t.Fatalf("attachIngress warned %d times before the flags are final: %v", n, logs.All())
			}
			if r.hook == "" {
				if h.postBindRefused != nil || !h.kernelPortAlloc || h.cgPostBind4 == nil || h.cgPostBind6 == nil {
					t.Fatalf("both attached: refused %+v, kernel picks %v, post_bind4 %v, post_bind6 %v; want none refused, the kernel picking, both kept",
						h.postBindRefused, h.kernelPortAlloc, h.cgPostBind4, h.cgPostBind6)
				}
				h.sayBindFallback(0)
				if n := logs.Len(); n != 0 {
					t.Fatalf("both attached, and %d warnings: %v", n, logs.All())
				}
				return
			}
			attachErr := refused[r.refuse[0]]
			if h.postBindRefused == nil || h.postBindRefused.hook != r.hook || !errors.Is(h.postBindRefused.err, attachErr) {
				t.Fatalf("kept refusal %+v, want %s's with the attach's error, %q", h.postBindRefused, r.hook, attachErr)
			}
			if h.kernelPortAlloc || h.cgPostBind4 != nil || h.cgPostBind6 != nil {
				t.Fatalf("%s refused, and the kernel picks %v, post_bind4 %v, post_bind6 %v; want neither kept", r.hook, h.kernelPortAlloc, h.cgPostBind4, h.cgPostBind6)
			}
			if pb4 := got[ebpf.AttachCGroupInet4PostBind]; pb4 != nil && !pb4.closed {
				t.Fatal("post_bind6 refused, and post_bind4 stayed attached")
			}

			h.sayBindFallback(structs.FlagObserveOnly)
			if n := logs.Len(); n != 0 {
				t.Fatalf("observing only, the bind hooks move no bind, and %d warnings: %v", n, logs.All())
			}
			h.sayBindFallback(^structs.FlagObserveOnly)
			all := logs.All()
			if len(all) != 1 {
				t.Fatalf("%d warnings with every flag but observe-only, want 1: %v", len(all), all)
			}
			msg := all[0].Message
			for _, w := range []string{"the " + r.hook + " cgroup hook did not attach", ownRangeSaid(t)} {
				if !strings.Contains(msg, w) {
					t.Errorf("the warning %q does not say %q", msg, w)
				}
			}
			if got := all[0].ContextMap()["error"]; got != attachErr.Error() {
				t.Errorf("the warning's error is %v, want the attach's, %q", got, attachErr)
			}
		})
	}
}

// sendFinalAgentInfo, the end of load, says where the app's binds go only
// once send has taken the flags the bind hooks read, and from those flags:
// the downstream build's customizer's first, then the kernel picking the
// moved port when both post_bind hooks attached, which a customizer that
// sets the flags whole does not clear, then start's. So under the
// customizer's FlagObserveOnly, where the bind hooks move no bind, nothing
// is warned, and a start or a send that fails says nothing.
func TestTheBindFallbackIsSaidWithTheFlagsSent(t *testing.T) {
	const otherFlag = 1 << 30 // a customizer's flag the agent does not use
	startErr, sendErr := errors.New("the watches did not start"), errors.New("the kernel took nothing")
	for _, r := range []struct {
		name      string
		refuse    []ebpf.AttachType
		customize uint32 // the flags the customizer sets, whole; no customizer when 0
		failStart bool
		failSend  bool
		warns     bool
	}{
		{"post_bind4 refused, no customizer", []ebpf.AttachType{ebpf.AttachCGroupInet4PostBind}, 0, false, false, true},
		{"post_bind6 refused, a customizer's other flag", []ebpf.AttachType{ebpf.AttachCGroupInet6PostBind}, otherFlag, false, false, true},
		{"post_bind4 refused, observing only", []ebpf.AttachType{ebpf.AttachCGroupInet4PostBind}, structs.FlagObserveOnly, false, false, false},
		{"post_bind6 refused, observing only", []ebpf.AttachType{ebpf.AttachCGroupInet6PostBind}, structs.FlagObserveOnly | otherFlag, false, false, false},
		{"both attach, no customizer", nil, 0, false, false, false},
		{"both attach, observing only", nil, structs.FlagObserveOnly, false, false, false},
		{"both attach, a customizer's other flag", nil, otherFlag, false, false, false},
		{"post_bind4 refused, start fails", []ebpf.AttachType{ebpf.AttachCGroupInet4PostBind}, 0, true, false, false},
		{"post_bind4 refused, send fails", []ebpf.AttachType{ebpf.AttachCGroupInet4PostBind}, 0, false, true, false},
	} {
		t.Run(r.name, func(t *testing.T) {
			prev := agent.AgentInfoCustomizer
			t.Cleanup(func() { agent.AgentInfoCustomizer = prev })
			agent.AgentInfoCustomizer = nil
			if r.customize != 0 {
				agent.AgentInfoCustomizer = func(info *structs.AgentInfo) { info.Flags = r.customize }
			}
			core, logs := observer.New(zapcore.WarnLevel)
			h := NewHooks(zap.New(core), &config.Config{})
			attach, _, refused := refusingAttach(r.refuse...)
			if err := h.attachIngress("/sys/fs/cgroup/app", bpfObjects{}, attach); err != nil {
				t.Fatalf("attachIngress: %v", err)
			}
			want := r.customize
			if len(r.refuse) == 0 {
				want |= structs.FlagKernelPortAlloc
			}

			var started, sent bool
			var gotSent structs.AgentInfo
			start := func(info *structs.AgentInfo) error {
				started = true
				if info.Flags != want {
					t.Errorf("start got the flags %#x, want %#x: the customizer's, then the kernel's pick when both post_bind hooks attached", info.Flags, want)
				}
				if n := logs.Len(); n != 0 {
					t.Errorf("%d warnings before the flags are sent: %v", n, logs.All())
				}
				if r.failStart {
					return startErr
				}
				info.Flags |= structs.FlagProvenUDP
				info.DNSPort = 26789
				return nil
			}
			send := func(info structs.AgentInfo) error {
				sent, gotSent = true, info
				if n := logs.Len(); n != 0 {
					t.Errorf("%d warnings before the flags are sent: %v", n, logs.All())
				}
				if r.failSend {
					return sendErr
				}
				return nil
			}
			err := h.sendFinalAgentInfo(structs.AgentInfo{KeployAgentNsPid: 7}, start, send)

			switch {
			case r.failStart:
				if !errors.Is(err, startErr) || sent {
					t.Fatalf("start failed: sendFinalAgentInfo returned %v, sent %v; want start's error, nothing sent", err, sent)
				}
			case r.failSend:
				if !errors.Is(err, sendErr) {
					t.Fatalf("send failed: sendFinalAgentInfo returned %v, want send's error", err)
				}
			default:
				if err != nil || !started || !sent {
					t.Fatalf("sendFinalAgentInfo returned %v, started %v, sent %v; want nil, both", err, started, sent)
				}
				if w := (structs.AgentInfo{KeployAgentNsPid: 7, DNSPort: 26789, Flags: want | structs.FlagProvenUDP}); gotSent != w {
					t.Fatalf("sent %+v, want %+v: info with the customizer's flags, the kernel's pick's, and start's", gotSent, w)
				}
			}

			all := logs.FilterLevelExact(zapcore.WarnLevel).All()
			if !r.warns {
				if len(all) != 0 {
					t.Fatalf("%d warnings, want none: %v", len(all), all)
				}
				return
			}
			if len(all) != 1 {
				t.Fatalf("%d warnings, want 1: %v", len(all), all)
			}
			hook := "post_bind4"
			if r.refuse[0] == ebpf.AttachCGroupInet6PostBind {
				hook = "post_bind6"
			}
			if msg := all[0].Message; !strings.Contains(msg, "the "+hook+" cgroup hook did not attach") {
				t.Errorf("the warning %q does not name %s", msg, hook)
			}
			if got := all[0].ContextMap()["error"]; got != refused[r.refuse[0]].Error() {
				t.Errorf("the warning's error is %v, want the attach's, %q", got, refused[r.refuse[0]])
			}
		})
	}
}
