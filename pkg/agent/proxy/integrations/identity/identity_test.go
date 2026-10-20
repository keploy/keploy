package identity

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

var (
	recorded = models.PodIdentity{
		Name: "api-55f5fd9659-fzsfr", UID: "0b1c4d6e-9f2a-4c3b-8d7e-1a2b3c4d5e6f",
		IP: "10.244.1.17", HostIP: "192.168.1.9", Node: "worker-1",
	}
	live = models.PodIdentity{
		Name: "api-rpl-1a2b3c", Hostname: "api-rpl-1a2b3c", UID: "7e6d5c4b-3a29-4817-a6f5-e4d3c2b1a0f9",
		IP: "10.244.2.4", HostIP: "192.168.1.10", Node: "worker-2",
	}
)

func TestRewriterReplacesWholeTokensOnly(t *testing.T) {
	r := New([]models.PodIdentity{recorded}, live)
	if r == nil {
		t.Fatal("identities that differ must give a rewriter")
	}
	cases := map[string]string{
		// name: a path segment, a query value, a JSON string, before a '.', at the end
		"/api/v1/namespaces/ns/pods/api-rpl-1a2b3c":        "/api/v1/namespaces/ns/pods/api-55f5fd9659-fzsfr",
		"fieldSelector=metadata.name%3Dapi-rpl-1a2b3c&x=1": "fieldSelector=metadata.name%3Dapi-55f5fd9659-fzsfr&x=1",
		`{"holderIdentity":"api-rpl-1a2b3c_8f1c"}`:         `{"holderIdentity":"api-55f5fd9659-fzsfr_8f1c"}`,
		"api-rpl-1a2b3c.api.ns.svc.cluster.local":          "api-55f5fd9659-fzsfr.api.ns.svc.cluster.local",
		"instance api-rpl-1a2b3c":                          "instance api-55f5fd9659-fzsfr",
		"api-rpl-1a2b3c api-rpl-1a2b3c":                    "api-55f5fd9659-fzsfr api-55f5fd9659-fzsfr",
		// name inside a longer token: left alone
		"api-rpl-1a2b3c-extra":  "api-rpl-1a2b3c-extra",
		"xapi-rpl-1a2b3c":       "xapi-rpl-1a2b3c",
		"api-rpl-1a2b3c2":       "api-rpl-1a2b3c2",
		"api-rpl-1a2b3cd":       "api-rpl-1a2b3cd",
		"api-rpl-1a2b3c_suffix": "api-55f5fd9659-fzsfr_suffix", // a pod name has no _: this one is whole
		// IP: whole address only, dotted and dashed
		"http://10.244.2.4:8080/x":            "http://10.244.1.17:8080/x",
		"ip=10.244.2.4,":                      "ip=10.244.1.17,",
		"10.244.2.40 110.244.2.4 10.244.2.4.": "10.244.2.40 110.244.2.4 10.244.1.17.",
		"10-244-2-4.ns.pod.cluster.local":     "10-244-1-17.ns.pod.cluster.local",
		"DD_AGENT_HOST=192.168.1.10":          "DD_AGENT_HOST=192.168.1.9",
		// node, uid (any case), uid with underscores (cgroup)
		"spec.nodeName=worker-2":               "spec.nodeName=worker-1",
		"worker-20":                            "worker-20",
		"7E6D5C4B-3A29-4817-A6F5-E4D3C2B1A0F9": "0b1c4d6e-9f2a-4c3b-8d7e-1a2b3c4d5e6f",
		"/kubepods/pod7e6d5c4b_3a29_4817_a6f5_e4d3c2b1a0f9/abc": "/kubepods/pod0b1c4d6e_9f2a_4c3b_8d7e_1a2b3c4d5e6f/abc",
		"7e6d5c4b-3a29-4817-a6f5-e4d3c2b1a0f9ab":                "7e6d5c4b-3a29-4817-a6f5-e4d3c2b1a0f9ab",
		// nothing of the live pod: unchanged
		"GET /healthz": "GET /healthz",
		"":             "",
	}
	for in, want := range cases {
		if got := r.String(in); got != want {
			t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	// Bytes returns the same slice when nothing changed.
	b := []byte("GET /healthz")
	if out := r.Bytes(b); &out[0] != &b[0] {
		t.Error("an unchanged body must come back as the same slice")
	}
}

func TestRewriterReverseRestoresTheLiveIdentity(t *testing.T) {
	r := New([]models.PodIdentity{recorded}, live)
	rev := r.Reverse()
	in := `{"metadata":{"name":"api-55f5fd9659-fzsfr","uid":"0b1c4d6e-9f2a-4c3b-8d7e-1a2b3c4d5e6f"},"status":{"podIP":"10.244.1.17","hostIP":"192.168.1.9"},"spec":{"nodeName":"worker-1"}}`
	want := `{"metadata":{"name":"api-rpl-1a2b3c","uid":"7e6d5c4b-3a29-4817-a6f5-e4d3c2b1a0f9"},"status":{"podIP":"10.244.2.4","hostIP":"192.168.1.10"},"spec":{"nodeName":"worker-2"}}`
	if got := rev.String(in); got != want {
		t.Fatalf("reverse:\n got %s\nwant %s", got, want)
	}
	if rev.Reverse() != r {
		t.Fatal("the reverse of the reverse is the rewriter")
	}
	// Forward then reverse is the identity on text that holds only live values.
	if got := rev.String(r.String(want)); got != want {
		t.Fatalf("round trip:\n got %s\nwant %s", got, want)
	}
}

func TestRewriterSkipsWhatMustNotBeAliased(t *testing.T) {
	// Nothing recorded, or nothing that differs: no rewriter.
	if New(nil, live) != nil || New([]models.PodIdentity{{}}, live) != nil {
		t.Fatal("no recorded identity must give no rewriter")
	}
	if New([]models.PodIdentity{live}, live) != nil {
		t.Fatal("identical identities must give no rewriter")
	}
	// Local addresses are never a source or a target.
	r := New([]models.PodIdentity{{Name: "rec-pod-1", IP: "10.0.0.9"}}, models.PodIdentity{Name: "live-pod-1", IP: "0.0.0.0"})
	if got := r.String("bind 0.0.0.0:8080 live-pod-1"); got != "bind 0.0.0.0:8080 rec-pod-1" {
		t.Fatalf("0.0.0.0 was aliased: %q", got)
	}
	// A same-node replay: node and host IP equal, only the pod half differs.
	sameNode := live
	sameNode.Node, sameNode.HostIP = recorded.Node, recorded.HostIP
	r = New([]models.PodIdentity{recorded}, sameNode)
	if got := r.String("worker-1 192.168.1.9 api-rpl-1a2b3c"); got != "worker-1 192.168.1.9 api-55f5fd9659-fzsfr" {
		t.Fatalf("same-node values were touched: %q", got)
	}
	// A short or non-IP value is not a rule.
	if New([]models.PodIdentity{{Name: "ab"}}, models.PodIdentity{Name: "cd"}) != nil {
		t.Fatal("two-letter names are not aliased")
	}
	if New([]models.PodIdentity{{Name: "rec-pod-1", IP: "not-an-ip"}}, models.PodIdentity{Name: "rec-pod-1", IP: "10.0.0.1"}) != nil {
		t.Fatal("a non-IP recorded address must not become a rule")
	}
}

func TestRewriterAliasesToTheFirstRecordedPod(t *testing.T) {
	second := recorded
	second.Name = "api-55f5fd9659-q2x8m"
	r := New([]models.PodIdentity{{}, recorded, second}, live)
	if got := r.String("/pods/api-rpl-1a2b3c"); got != "/pods/api-55f5fd9659-fzsfr" {
		t.Fatalf("got %q", got)
	}
}

func TestLiveReadsTheDownwardAPIEnv(t *testing.T) {
	t.Setenv(PodNameEnv, "api-rpl-1a2b3c")
	t.Setenv(PodUIDEnv, "uid-1")
	t.Setenv(PodIPEnv, "10.244.2.4")
	t.Setenv(NodeEnv, "worker-2")
	t.Setenv(HostIPEnv, "192.168.1.10")
	got := Live()
	if got.Name != "api-rpl-1a2b3c" || got.UID != "uid-1" || got.IP != "10.244.2.4" || got.Node != "worker-2" || got.HostIP != "192.168.1.10" {
		t.Fatalf("Live() = %+v", got)
	}
	if got.Hostname == "" {
		t.Fatal("the hostname comes from the UTS namespace")
	}
	t.Setenv(PodNameEnv, "")
	if got := Live(); got.Name != got.Hostname || got.Name == "" {
		t.Fatalf("without the env the hostname stands in for the name: %+v", got)
	}
}
