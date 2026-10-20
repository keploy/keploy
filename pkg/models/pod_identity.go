package models

import (
	"fmt"
	"strings"
)

// PodIdentity is what a Kubernetes pod can learn about itself and put into an
// outgoing call: its name (metadata.name, also the default HOSTNAME), uid, IP,
// the node's name and IP (status.hostIP, where node-local agents are reached)
// and hostname (spec.hostname when the workload sets one, else the name).
//
// A replay runs the app in a copy pod under a NEW name/uid/ip, so a call that
// carries the pod's own identity differs from its recording and misses its
// mock. The recorder writes the recorded pod's identity into the test set's
// config.yaml metadata (the keys below; k8s-proxy passes them on --metadata),
// the replayer hands it to the agent on OutgoingOptions.RecordedPods, and the
// agent rewrites the copy's identity to the recorded one before matching
// (pkg/agent/proxy/integrations/identity).
type PodIdentity struct {
	Name     string `json:"name" yaml:"name"`
	UID      string `json:"uid,omitempty" yaml:"uid,omitempty"`
	IP       string `json:"ip,omitempty" yaml:"ip,omitempty"`
	HostIP   string `json:"hostIP,omitempty" yaml:"hostIP,omitempty"`
	Node     string `json:"node,omitempty" yaml:"node,omitempty"`
	Hostname string `json:"hostname,omitempty" yaml:"hostname,omitempty"`
}

// The test-set config metadata keys the recorded pod's identity is written
// under. Flat (strvals nests on '.'), and fixed: k8s-proxy writes them.
const (
	PodIdentityMetaName     = "recordedPodName"
	PodIdentityMetaUID      = "recordedPodUID"
	PodIdentityMetaIP       = "recordedPodIP"
	PodIdentityMetaHostIP   = "recordedPodHostIP"
	PodIdentityMetaNode     = "recordedPodNode"
	PodIdentityMetaHostname = "recordedPodHostname"
)

// IsZero reports whether the identity names no pod.
func (p PodIdentity) IsZero() bool { return strings.TrimSpace(p.Name) == "" }

// PodIdentityFromMetadata reads the recorded pod's identity from a test set's
// config metadata. ok is false when it names no pod. The metadata came
// through strvals, which types what it parses (an all-digit value becomes a
// number), so every value is read through fmt.Sprint.
func PodIdentityFromMetadata(meta map[string]interface{}) (PodIdentity, bool) {
	get := func(key string) string {
		v, ok := meta[key]
		if !ok || v == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	p := PodIdentity{
		Name:     get(PodIdentityMetaName),
		UID:      get(PodIdentityMetaUID),
		IP:       get(PodIdentityMetaIP),
		HostIP:   get(PodIdentityMetaHostIP),
		Node:     get(PodIdentityMetaNode),
		Hostname: get(PodIdentityMetaHostname),
	}
	if p.IsZero() {
		return PodIdentity{}, false
	}
	return p, true
}

// IdentityAliasing is what a replay hands the agent about pod identity
// (OutgoingOptions.Identity). RecordedPods are the identities of the pods
// that recorded the test set being replayed (its config.yaml metadata,
// PodIdentityFromMetadata). The agent replaces every whole-token occurrence
// of its OWN pod identity (the downward-API env, identity.Live) with the
// recorded one in a live request before matching, and puts its own back into
// the served response: an app that puts its pod name, uid, IP, node or host
// IP into a call then finds its mock although the replay pod is not the
// recorded pod. Disabled turns that off (--disableIdentityAliasing). A nil
// IdentityAliasing, or one without recorded pods, aliases nothing: a set
// recorded before the identity was written, or outside Kubernetes.
type IdentityAliasing struct {
	RecordedPods []PodIdentity `json:"recordedPods,omitempty"`
	Disabled     bool          `json:"disabled,omitempty"`
}

// NewIdentityAliasing is the options for recordedPods, or nil when there is
// nothing to carry: no recorded pod and the aliasing not disabled.
func NewIdentityAliasing(recordedPods []PodIdentity, disabled bool) *IdentityAliasing {
	if len(recordedPods) == 0 && !disabled {
		return nil
	}
	return &IdentityAliasing{RecordedPods: recordedPods, Disabled: disabled}
}
