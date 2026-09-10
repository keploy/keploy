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

			warns := logs.FilterMessageSnippet("cannot hold a volume entry").Len()
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

// TestAddTopLevelVolume_ResolvesAnAliasByCopy covers `volumes: *vols`,
// which is legal compose and the same `x-*` + anchor idiom the rest of this
// change works to preserve.
//
// An alias node is a REFERENCE: it holds no entries, so appending to it writes
// into a node the encoder never emits. The generated file then declared no
// volume while the agent service mounted one, and docker compose rejected it
// with "refers to undefined volume" — an error naming a service the user never
// wrote.
//
// The alias is resolved BY COPY: `volumes:` receives a clone of the fragment and
// keploy's entry is appended to that. Appending into the anchored node instead
// would hand the entry to every other alias of it, which is what the sibling
// test below pins.
func TestAddTopLevelVolume_ResolvesAnAliasByCopy(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `x-vols: &vols
  appdata: {}
volumes: *vols
services:
  app:
    image: alpine:3.21
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	idc.addTopLevelVolume(&compose, "keploy-tls-certs")

	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	vols, ok := reloaded["volumes"].(map[string]interface{})
	if !ok {
		t.Fatalf("volumes is %T, not a mapping:\n%s", reloaded["volumes"], data)
	}
	if _, ok := vols["keploy-tls-certs"]; !ok {
		t.Errorf("the volume was not declared; the agent service would reference an "+
			"undefined volume:\n%s", data)
	}
	// The user's own volume must survive...
	if _, ok := vols["appdata"]; !ok {
		t.Errorf("the user's own volume was lost: %v", vols)
	}
	// ...and the anchored fragment must be UNCHANGED. Appending into it instead
	// of copying would hand keploy's volume to every other alias of it.
	frag, ok := reloaded["x-vols"].(map[string]interface{})
	if !ok {
		t.Fatalf("x-vols is %T, not the mapping the user wrote:\n%s", reloaded["x-vols"], data)
	}
	if _, leaked := frag["keploy-tls-certs"]; leaked {
		t.Errorf("keploy's volume was written into the user's shared `x-vols` "+
			"fragment; every other alias of it inherits the entry:\n%s", data)
	}
	if len(frag) != 1 {
		t.Errorf("the user's fragment gained entries: %v", frag)
	}
}

// TestAddTopLevelVolume_AliasIsIdempotent pins that following the alias does not
// break the duplicate check — appending twice through a reference must still
// produce one entry, or the generated file carries a duplicate mapping key and
// will not load at all.
func TestAddTopLevelVolume_AliasIsIdempotent(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = "x-vols: &vols\n  appdata: {}\nvolumes: *vols\n"
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
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
	vols, _ := reloaded["volumes"].(map[string]interface{})
	if len(vols) != 2 {
		t.Errorf("volumes has %d entries, want 2 (appdata + keploy-tls-certs): %v", len(vols), vols)
	}
}

// TestAddTopLevelVolume_AliasToNonMappingIsLeftAlone pins the guard on following
// an alias.
//
// Following it blindly would append into whatever the anchor happens to be. If
// that is a scalar the entries vanish exactly as they did before this change —
// but now silently and without the warning, because the code believes it found a
// container. Worse, it would be writing into the user's own fragment. An alias is
// followed only when it resolves to something that can actually hold entries.
func TestAddTopLevelVolume_AliasToNonMappingIsLeftAlone(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	idc := &Impl{logger: zap.New(core)}

	// A populated sequence. Under copy-resolution either shape discriminates —
	// dropping the guard leaves `volumes:` an empty mapping and never touches
	// the fragment — so what this pins is the WARNING, plus the fragment being
	// left exactly as written rather than reshaped to suit us.
	const in = `x-thing: &thing
  - just-a-string
volumes: *thing
services:
  app:
    image: alpine:3.21
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	idc.addTopLevelVolume(&compose, "keploy-tls-certs")

	if logs.FilterMessageSnippet("cannot hold a volume entry").Len() == 0 {
		t.Error("no warning for an alias resolving to a scalar — the volume cannot " +
			"be declared there, and silence is what made this class of failure hard " +
			"to diagnose in the first place")
	}

	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	// The user's fragment must be untouched: appending into it corrupts a value
	// they wrote, and unlike a scalar a sequence's contents really are emitted.
	frag, ok := reloaded["x-thing"].([]interface{})
	if !ok {
		t.Fatalf("x-thing is %T, not the sequence the user wrote:\n%s", reloaded["x-thing"], data)
	}
	if len(frag) != 1 || frag[0] != "just-a-string" {
		t.Errorf("keploy appended into the user's anchored sequence: %v\n%s", frag, data)
	}
}

// TestAddTopLevelVolume_DoesNotCorruptAFragmentSharedElsewhere is the reason the
// alias is resolved by COPY rather than by appending into the anchored node.
//
// An `x-*` fragment is shared: appending into it hands keploy's volume to every
// other reference. The shape below is the sharp case — the same fragment names
// the volume set AND a service's labels — so mutating it in place gives the app
// a `keploy-tls-certs` LABEL. Corrupting an unrelated part of the user's file to
// fix our own is a worse outcome than the bug, and it leaves no trace.
func TestAddTopLevelVolume_DoesNotCorruptAFragmentSharedElsewhere(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `x-common: &common
  foo: bar
volumes: *common
services:
  app:
    image: alpine:3.21
    labels: *common
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	idc.addTopLevelVolume(&compose, "keploy-tls-certs")

	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}

	// keploy's volume is declared...
	vols, _ := reloaded["volumes"].(map[string]interface{})
	if _, ok := vols["keploy-tls-certs"]; !ok {
		t.Errorf("the volume was not declared:\n%s", data)
	}
	// ...and has NOT leaked into the shared fragment or the service's labels.
	frag, _ := reloaded["x-common"].(map[string]interface{})
	if _, leaked := frag["keploy-tls-certs"]; leaked {
		t.Errorf("keploy's volume leaked into the shared `x-common` fragment:\n%s", data)
	}
	svcs, _ := reloaded["services"].(map[string]interface{})
	app, _ := svcs["app"].(map[string]interface{})
	labels, _ := app["labels"].(map[string]interface{})
	if _, leaked := labels["keploy-tls-certs"]; leaked {
		t.Errorf("keploy's volume became a LABEL on the user's service — the shared "+
			"fragment was mutated in place:\n%s", data)
	}
	if len(labels) != 1 {
		t.Errorf("the service's labels changed: %v", labels)
	}
}

// TestAddTopLevelVolume_ResolvedCopiesAreIndependent pins that the clone in
// sectionForAppend is load-bearing rather than defensive.
//
// A mapping node's Content holds two entries per key, so a THREE-key fragment
// sits at len 6 / cap 8 — exactly the two spare slots one append consumes. Share
// the slice instead of cloning it and the append writes into the fragment's
// backing array without reallocating; a second section resolved through the same
// fragment then OVERWRITES the first's entry instead of appending after it.
//
// Three keys is not incidental: one- and two-key fragments have zero spare
// capacity, so Go reallocates and the bug cannot be observed.
func TestAddTopLevelVolume_ResolvedCopiesAreIndependent(t *testing.T) {
	const in = `x-shared: &shared
  a: {}
  b: {}
  c: {}
volumes: *shared
networks: *shared
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Resolve BOTH sections through the same fragment and append to each, which
	// is what extending this to networks would do.
	vols := sectionForAppend(&compose.Volumes)
	nets := sectionForAppend(&compose.Networks)
	vols.Content = append(vols.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "keploy-tls-certs"},
		&yaml.Node{Kind: yaml.MappingNode})
	nets.Content = append(nets.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "keploy-net"},
		&yaml.Node{Kind: yaml.MappingNode})

	idc := &Impl{logger: zap.NewNop()}
	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("reload: %v\n%s", err, data)
	}

	gotVols, _ := reloaded["volumes"].(map[string]interface{})
	if _, ok := gotVols["keploy-tls-certs"]; !ok {
		t.Errorf("volumes lost its own entry — a second section resolved through the "+
			"same fragment overwrote it through a shared backing array: %v\n%s",
			gotVols, data)
	}
	if _, wrong := gotVols["keploy-net"]; wrong {
		t.Errorf("volumes gained the networks entry: %v", gotVols)
	}
	gotNets, _ := reloaded["networks"].(map[string]interface{})
	if _, ok := gotNets["keploy-net"]; !ok {
		t.Errorf("networks lost its own entry: %v", gotNets)
	}
	// And the user's fragment is untouched by either.
	frag, _ := reloaded["x-shared"].(map[string]interface{})
	if len(frag) != 3 {
		t.Errorf("the shared fragment gained entries: %v", frag)
	}
}

// TestAddTopLevelVolume_AliasToAnEmptyFragment pins the isEmptySection arm of
// sectionForAppend, which nothing else reaches.
//
// `x-vols: &vols` with nothing under it is a null scalar, so an alias to it is
// not a mapping — declining there would regress this shape straight back to the
// bug. The fragment itself must NOT be reshaped: rewriting a user's `&vols` into
// a mapping is how the earlier, abandoned approach broke a service whose own
// volumes list aliased the same fragment.
func TestAddTopLevelVolume_AliasToAnEmptyFragment(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `x-vols: &vols
volumes: *vols
services:
  app:
    image: alpine:3.21
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	idc.addTopLevelVolume(&compose, "keploy-tls-certs")

	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	vols, ok := reloaded["volumes"].(map[string]interface{})
	if !ok {
		t.Fatalf("volumes is %T, not a mapping — an alias to an empty fragment was "+
			"declined, leaving the volume undeclared:\n%s", reloaded["volumes"], data)
	}
	if _, ok := vols["keploy-tls-certs"]; !ok {
		t.Errorf("the volume was not declared: %v\n%s", vols, data)
	}
	// The user's fragment stays exactly as written.
	if v, present := reloaded["x-vols"]; !present || v != nil {
		t.Errorf("the empty anchored fragment was reshaped to %#v; it must be left "+
			"as the user wrote it:\n%s", v, data)
	}
}
