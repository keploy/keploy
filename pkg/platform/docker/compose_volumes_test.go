package docker

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

// TestAddTopLevelVolume_ExplicitNullVolumes is the regression test for a compose
// file that declares `volumes:` with nothing under it.
//
// The guard used to test only for a ZERO node, which is what an ABSENT key
// decodes to. An explicit empty key decodes to a null SCALAR (Kind
// yaml.ScalarNode, Tag "!!null") instead, so the normalisation was skipped and
// the volume was appended to a scalar's Content -- where the encoder ignores it.
// The generated compose then declared no volume at all while keploy's agent
// service referenced one, and docker compose refused the file. The user saw
// their app fail to start on a compose file docker accepts.
func TestAddTopLevelVolume_ExplicitNullVolumes(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `services:
  app:
    image: alpine:3.21
volumes:
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Guard the premise: if this ever decodes to a zero node, the bug this
	// pins cannot happen and the test is no longer testing anything.
	if compose.Volumes.Kind != yaml.ScalarNode || compose.Volumes.Tag != "!!null" {
		t.Fatalf("expected an explicit `volumes:` to decode as a null scalar, got Kind=%d Tag=%q",
			compose.Volumes.Kind, compose.Volumes.Tag)
	}

	idc.addTopLevelVolume(&compose, "keploy-tls-certs")

	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "keploy-tls-certs") {
		t.Fatalf("the appended volume was never emitted; the agent service would "+
			"reference an undefined volume:\n%s", data)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("reload: %v\n%s", err, data)
	}
	vols, ok := reloaded["volumes"].(map[string]interface{})
	if !ok {
		t.Fatalf("volumes is %T, not a mapping — docker compose rejects this:\n%s",
			reloaded["volumes"], data)
	}
	if _, ok := vols["keploy-tls-certs"]; !ok {
		t.Errorf("volumes mapping does not contain the appended volume: %v", vols)
	}
	// Cosmetic, but pinned so the tag-clearing is not unjustified dead code:
	// docker compose accepts `volumes: !!null` with entries under it, yet
	// emitting it puts a stray null tag in a file operators read while
	// debugging. Scoped to the volumes line rather than the whole document, so
	// a fixture that legitimately contains "!!null" elsewhere cannot trip it.
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "volumes:") && strings.Contains(line, "!!null") {
			t.Errorf("volumes section carries a stray null tag: %q", line)
		}
	}
}

// TestAddTopLevelVolume_AbsentAndPopulated pins the two shapes that already
// worked, so normalising the null case did not break them.
func TestAddTopLevelVolume_AbsentAndPopulated(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	for _, tc := range []struct {
		name string
		in   string
		also string // an existing volume that must survive
	}{
		{"absent", "services:\n  app:\n    image: alpine:3.21\n", ""},
		{"populated", "services:\n  app:\n    image: alpine:3.21\nvolumes:\n  appdata: {}\n", "appdata"},
		{"empty mapping", "services:\n  app:\n    image: alpine:3.21\nvolumes: {}\n", ""},
		// The shapes that survived a fix which enumerated null spellings: each
		// produces the identical "volumes must be a mapping" rejection.
		{"empty sequence", "services:\n  app:\n    image: alpine:3.21\nvolumes: []\n", ""},
		{"empty string", "services:\n  app:\n    image: alpine:3.21\nvolumes: \"\"\n", ""},
		{"tilde null", "services:\n  app:\n    image: alpine:3.21\nvolumes: ~\n", ""},
		{"word null", "services:\n  app:\n    image: alpine:3.21\nvolumes: null\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var compose Compose
			if err := yaml.Unmarshal([]byte(tc.in), &compose); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			idc.addTopLevelVolume(&compose, "keploy-tls-certs")
			data, err := idc.MarshalCompose(&compose)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var reloaded map[string]interface{}
			if err := yaml.Unmarshal(data, &reloaded); err != nil {
				t.Fatalf("reload: %v\n%s", err, data)
			}
			vols, ok := reloaded["volumes"].(map[string]interface{})
			if !ok {
				t.Fatalf("volumes is %T, not a mapping:\n%s", reloaded["volumes"], data)
			}
			if _, ok := vols["keploy-tls-certs"]; !ok {
				t.Errorf("appended volume missing: %v", vols)
			}
			if tc.also != "" {
				if _, ok := vols[tc.also]; !ok {
					t.Errorf("pre-existing volume %q was lost: %v", tc.also, vols)
				}
			}
		})
	}
}

// TestAddTopLevelVolume_Idempotent pins that appending the same volume twice
// does not duplicate the key, which would make the file unloadable.
func TestAddTopLevelVolume_Idempotent(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	var compose Compose
	if err := yaml.Unmarshal([]byte("services:\n  app:\n    image: alpine:3.21\nvolumes:\n"), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	idc.addTopLevelVolume(&compose, "keploy-tls-certs")
	idc.addTopLevelVolume(&compose, "keploy-tls-certs")
	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("duplicate key made the document unloadable: %v\n%s", err, data)
	}
	// Count KEYS in the volumes mapping, not occurrences in the document: the
	// real pipeline also emits the name once per service that mounts it, so a
	// document-wide count would fire spuriously the moment this fixture grew a
	// mount.
	vols, ok := reloaded["volumes"].(map[string]interface{})
	if !ok {
		t.Fatalf("volumes is %T, not a mapping:\n%s", reloaded["volumes"], data)
	}
	if len(vols) != 1 {
		t.Errorf("volumes has %d entries, want 1: %v", len(vols), vols)
	}
}

// TestEnsureMapping_LeavesPopulatedNonMappingAlone pins the guard that stops the
// normalisation becoming data loss.
//
// Reshaping an EMPTY node is safe — there is nothing to lose. Reshaping a
// populated one is not: the content would be dropped on the floor and the user
// would get a compose file silently missing what they wrote. docker compose's
// own "must be a mapping" error is a far better outcome than losing it, so a
// populated non-mapping must survive untouched.
func TestEnsureMapping_LeavesPopulatedNonMappingAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"populated sequence", "volumes:\n  - appdata\n  - logs\n"},
		{"non-empty scalar", "volumes: appdata\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(tc.in), &doc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			node := doc.Content[0].Content[1] // the value of `volumes:`
			before := *node
			beforeKids := len(node.Content)

			ensureMapping(node)

			if node.Kind != before.Kind {
				t.Errorf("Kind changed %d -> %d: a populated node was reshaped, "+
					"which discards what the user wrote", before.Kind, node.Kind)
			}
			if len(node.Content) != beforeKids {
				t.Errorf("Content changed %d -> %d entries", beforeKids, len(node.Content))
			}
			if node.Value != before.Value {
				t.Errorf("Value changed %q -> %q", before.Value, node.Value)
			}
		})
	}
}

// TestAddTopLevelVolume_WarnsWhenVolumesCannotHoldTheEntry covers the shapes
// ensureMapping deliberately refuses to reshape.
//
// `volumes: *alias` is the one that matters: it is LEGAL compose and it is the
// same `x-*` + anchor idiom this file preserves elsewhere, yet keploy cannot
// append its volume to an alias node. Without a warning the user gets only
// docker compose's downstream complaint, which names the agent service and says
// nothing about their `volumes:` line.
func TestAddTopLevelVolume_WarnsWhenVolumesCannotHoldTheEntry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       string
		wantWarn bool
	}{
		{"alias to a populated mapping", "x-vols: &vols\n  appdata: {}\nvolumes: *vols\n", true},
		{"populated sequence", "volumes:\n  - appdata\n", true},
		{"populated scalar", "volumes: appdata\n", true},
		{"explicit null (normalises fine)", "volumes:\n", false},
		{"empty sequence (normalises fine)", "volumes: []\n", false},
		{"already a mapping", "volumes:\n  appdata: {}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			idc := &Impl{logger: zap.New(core)}

			var compose Compose
			if err := yaml.Unmarshal([]byte(tc.in), &compose); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			idc.addTopLevelVolume(&compose, "keploy-tls-certs")

			warns := logs.FilterMessageSnippet("is not a mapping").Len()
			if tc.wantWarn && warns == 0 {
				t.Errorf("no warning for a `volumes:` shape that cannot hold the entry; " +
					"the user would only see docker compose's undefined-volume error")
			}
			if !tc.wantWarn && warns != 0 {
				t.Errorf("warned about a shape that normalised fine: %v",
					logs.All()[0].Message)
			}
			// Whatever happens, the user's own content must survive.
			if tc.wantWarn {
				data, err := idc.MarshalCompose(&compose)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if !strings.Contains(string(data), "appdata") {
					t.Errorf("the user's volumes content was discarded:\n%s", data)
				}
			}
		})
	}
}
