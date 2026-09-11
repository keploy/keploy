package docker

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"go.keploy.io/server/v3/config"
)

// runWithGate drives the real path in the order the caller uses it: the gate
// first, whose ports become opts.AppPorts, then the rewrite.
func runWithGate(t *testing.T, in string) (map[string]interface{}, []string) {
	t.Helper()
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_, ports, _, found := idc.findContainerInServices(&compose, "app")
	if !found {
		t.Fatalf("gate did not find the app service")
	}
	opts := buildAgentOpts()
	opts.AppPorts = ports
	if err := idc.ModifyComposeForAgent(&compose, opts, "app"); err != nil {
		t.Fatalf("ModifyComposeForAgent: %v", err)
	}
	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	t.Logf("generated:\n%s", data)
	return out, ports
}

func mergeApp(t *testing.T, out map[string]interface{}) map[string]interface{} {
	t.Helper()
	svcs, _ := out["services"].(map[string]interface{})
	app, _ := svcs["app"].(map[string]interface{})
	if app == nil {
		t.Fatalf("app service missing: %v", svcs)
	}
	return app
}

// TestMergeKey_InheritedKeysAreEditedLikeAnyOther is the regression test for all
// three ways a `<<:` key went wrong. The fixture is the shape the compose spec
// recommends — reusable config in an `x-*` fragment, pulled in with a merge key.
func TestMergeKey_InheritedKeysAreEditedLikeAnyOther(t *testing.T) {
	out, ports := runWithGate(t, `x-common: &common
  environment:
    - MINE=1
  networks:
    - mynet
  ports:
    - "8080:8080"
networks:
  mynet:
services:
  app:
    <<: *common
    image: alpine:3.21
`)
	app := mergeApp(t, out)

	// 1. The gate.
	if len(ports) != 1 || ports[0] != "8080:8080" {
		t.Errorf("the gate read %#v through the merge key; keploy-agent publishes "+
			"on the app's behalf, so the app would be unreachable from the host", ports)
	}

	// 2. The user's own variables.
	env, _ := app["environment"].([]interface{})
	var keptMine, gotCA bool
	for _, e := range env {
		s, _ := e.(string)
		if s == "MINE=1" {
			keptMine = true
		}
		if strings.HasPrefix(s, "SSL_CERT_FILE=") {
			gotCA = true
		}
	}
	if !keptMine {
		t.Errorf("the inherited variable was lost: keploy added an explicit "+
			"environment: that overrides the merged one: %v", env)
	}
	if !gotCA {
		t.Errorf("keploy's CA was not added: %v", env)
	}

	// 3. The keys keploy has to DELETE. An inherited key cannot be unset in
	//    YAML, so leaving the merge in place left both of these standing next to
	//    the network_mode keploy adds.
	if app["networks"] != nil {
		t.Errorf("networks survived alongside network_mode=%v; docker compose "+
			"rejects that pair as mutually exclusive: %v",
			app["network_mode"], app["networks"])
	}
	if app["ports"] != nil {
		t.Errorf("ports survived on a service using network_mode=%v: %v",
			app["network_mode"], app["ports"])
	}
	if app["network_mode"] != "service:keploy-agent" {
		t.Errorf("the app was not put in the agent's netns: %v", app["network_mode"])
	}
}

// TestMergeKey_PrecedenceFollowsTheSpec pins the two rules that are easy to get
// backwards. An explicit key beats a merged one, and within `<<: [*a, *b]` the
// EARLIER entry wins — which reads like the opposite of "later overrides".
func TestMergeKey_PrecedenceFollowsTheSpec(t *testing.T) {
	t.Run("explicit beats merged", func(t *testing.T) {
		app := mergeApp(t, mustRun(t, `x-common: &common
  image: alpine:3.20
  working_dir: /from-fragment
services:
  app:
    <<: *common
    image: alpine:3.21
`))
		if app["image"] != "alpine:3.21" {
			t.Errorf("the merged value overrode the explicit one: %v", app["image"])
		}
		if app["working_dir"] != "/from-fragment" {
			t.Errorf("the inherited key was lost: %v", app["working_dir"])
		}
	})

	t.Run("earlier entry wins in a sequence", func(t *testing.T) {
		app := mergeApp(t, mustRun(t, `x-first: &first
  working_dir: /first
x-second: &second
  working_dir: /second
  user: someone
services:
  app:
    <<: [*first, *second]
    image: alpine:3.21
`))
		if app["working_dir"] != "/first" {
			t.Errorf("precedence is backwards: got %v, the spec gives the earlier "+
				"entry priority", app["working_dir"])
		}
		if app["user"] != "someone" {
			t.Errorf("a key only the later entry defines was dropped: %v", app["user"])
		}
	})

	t.Run("a merge nested inside a fragment is followed", func(t *testing.T) {
		app := mergeApp(t, mustRun(t, `x-base: &base
  working_dir: /base
  user: someone
x-mid: &mid
  <<: *base
  working_dir: /mid
services:
  app:
    <<: *mid
    image: alpine:3.21
`))
		if app["working_dir"] != "/mid" {
			t.Errorf("the nested merge overrode its own parent: %v", app["working_dir"])
		}
		if app["user"] != "someone" {
			t.Errorf("a key reached only through the nested merge was lost: %v", app["user"])
		}
	})
}

// TestMergeKey_TheFragmentIsNotEdited is the guard that matters most. The
// fragment is the user's, and it is shared: keploy deletes keys from the app
// service and appends to its environment, so taking the fragment's nodes by
// reference would carry every one of those edits into each sibling that merges
// the same fragment.
func TestMergeKey_TheFragmentIsNotEdited(t *testing.T) {
	out := mustRun(t, `x-common: &common
  environment:
    - MINE=1
  networks:
    - mynet
networks:
  mynet:
services:
  app:
    <<: *common
    image: alpine:3.21
  sidecar:
    <<: *common
    image: alpine:3.21
`)
	// The sibling must be exactly what the user wrote.
	svcs, _ := out["services"].(map[string]interface{})
	sidecar, _ := svcs["sidecar"].(map[string]interface{})
	if sidecar == nil {
		t.Fatalf("sidecar disappeared: %v", svcs)
	}
	env, _ := sidecar["environment"].([]interface{})
	if len(env) != 1 || env[0] != "MINE=1" {
		t.Errorf("the sidecar's environment is %v; it is not in the agent's netns, "+
			"so keploy's CA paths would point at a file it does not have", env)
	}
	if nets, _ := sidecar["networks"].([]interface{}); len(nets) != 1 {
		t.Errorf("keploy's deletion reached the sidecar: %v", sidecar["networks"])
	}
	// And the fragment itself.
	frag, _ := out["x-common"].(map[string]interface{})
	if frag == nil {
		t.Fatalf("the fragment disappeared: %v", out["x-common"])
	}
	if fenv, _ := frag["environment"].([]interface{}); len(fenv) != 1 {
		t.Errorf("the user's fragment gained keploy's variables: %v", frag["environment"])
	}
	if _, present := frag["networks"]; !present {
		t.Errorf("keploy deleted a key from the user's fragment: %v", frag)
	}
}

// TestMergeKey_NoMergeKeyIsUntouched pins that a file without `<<:` is not
// reshaped. flattenMergeKeys runs on every app service, so a no-op has to be a
// real no-op.
func TestMergeKey_NoMergeKeyIsUntouched(t *testing.T) {
	app := mergeApp(t, mustRun(t, `services:
  app:
    image: alpine:3.21
    environment:
      - MINE=1
`))
	env, _ := app["environment"].([]interface{})
	if len(env) == 0 || env[0] != "MINE=1" {
		t.Errorf("the user's own entry moved or was lost: %v", env)
	}
}

func mustRun(t *testing.T, in string) map[string]interface{} {
	t.Helper()
	out, _ := runWithGate(t, in)
	return out
}

// TestMergeKey_RewriteAloneFlattensToo pins the call on the write path. The gate
// normally runs first and flattens the same node, so this is the only thing that
// distinguishes the two call sites — and ModifyComposeForAgent is exported and
// called directly, including from enterprise, without the gate in front of it.
func TestMergeKey_RewriteAloneFlattensToo(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(`x-common: &common
  networks:
    - mynet
networks:
  mynet:
services:
  app:
    <<: *common
    image: alpine:3.21
`), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Deliberately no findContainerInServices call.
	if err := idc.ModifyComposeForAgent(&compose, buildAgentOpts(), "app"); err != nil {
		t.Fatalf("ModifyComposeForAgent: %v", err)
	}
	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	app := mergeApp(t, out)
	if app["networks"] != nil {
		t.Errorf("the inherited networks survived next to network_mode=%v; compose "+
			"rejects that pair:\n%s", app["network_mode"], data)
	}
}

// TestMergeKey_MaterialisedValuesCarryNoAnchor pins the order of the two passes.
//
// A fragment may anchor a value inside itself, and the materialised copy carries
// that anchor — a SECOND definition of the same name, where a later `*ref` binds
// to whichever the parser saw last. flattenMergeKeys does not strip it itself;
// detachSubtree clears every anchor in the subtree and runs immediately after,
// which is precisely why flattening comes first. Swap the two and this fails.
func TestMergeKey_MaterialisedValuesCarryNoAnchor(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(`x-common: &common
  environment: &shared
    - MINE=1
services:
  app:
    <<: *common
    image: alpine:3.21
`), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := idc.ModifyComposeForAgent(&compose, buildAgentOpts(), "app"); err != nil {
		t.Fatalf("ModifyComposeForAgent: %v", err)
	}
	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if n := strings.Count(string(data), "&shared"); n != 1 {
		t.Errorf("the anchor is defined %d times; the materialised copy kept the "+
			"one the fragment owns:\n%s", n, data)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
}

// TestMergeKey_EveryMergeKeyIsConsumed pins that a second `<<` is not skipped.
//
// Two `<<` keys in one mapping is not legal YAML and docker compose rejects the
// file — but decoding into a yaml.Node accepts it, which is the only reason
// keploy sees it at all. Consuming only the first leaves whatever the second
// brings in invisible to keploy's writers, which is this function's entire bug
// class reappearing in a rarer file: here the inherited `networks:` would
// survive next to the `network_mode:` keploy adds, and compose rejects that pair.
func TestMergeKey_EveryMergeKeyIsConsumed(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(`x-first: &first
  working_dir: /first
x-second: &second
  working_dir: /second
  networks:
    - mynet
  user: someone
networks:
  mynet:
services:
  app:
    <<: *first
    <<: *second
    image: alpine:3.21
`), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := idc.ModifyComposeForAgent(&compose, buildAgentOpts(), "app"); err != nil {
		t.Fatalf("ModifyComposeForAgent: %v", err)
	}
	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	app := mergeApp(t, out)
	// Both define working_dir, so this also pins the precedence BETWEEN merge
	// keys: the earlier one wins, matching the rule inside a single
	// `<<: [*a, *b]`. With a disjoint fixture the order is unobservable.
	if app["working_dir"] != "/first" {
		t.Errorf("working_dir is %v; the earlier merge key must win", app["working_dir"])
	}
	if app["user"] != "someone" {
		t.Errorf("the second merge key was skipped: %v", app["user"])
	}
	if app["networks"] != nil {
		t.Errorf("networks inherited through the second merge key survived next to "+
			"network_mode=%v; compose rejects that pair: %v",
			app["network_mode"], app["networks"])
	}
	if strings.Contains(string(data), "<<:") {
		t.Errorf("a merge key was left in the generated file:\n%s", data)
	}
}

// TestMergeKey_NestedMergeIsNotLeftOnTheService pins the skip that keeps a `<<`
// from a nested fragment out of the materialised keys.
//
// Without it the app gains a `<<` of its own, and compose re-merges that
// fragment when it loads the generated file — AFTER keploy's deletes — bringing
// `networks:` back next to the `network_mode:` keploy added. That is this
// function's entire bug class, reintroduced by the fix for it.
//
// It has to run without the gate: the gate flattens too, and a second pass
// silently cleans up the stray key, which is why every other fixture misses this.
func TestMergeKey_NestedMergeIsNotLeftOnTheService(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(`x-base: &base
  networks:
    - mynet
x-mid: &mid
  <<: *base
  working_dir: /mid
networks:
  mynet:
services:
  app:
    <<: *mid
    image: alpine:3.21
`), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Deliberately no findContainerInServices call — one flatten only.
	if err := idc.ModifyComposeForAgent(&compose, buildAgentOpts(), "app"); err != nil {
		t.Fatalf("ModifyComposeForAgent: %v", err)
	}
	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	app := mergeApp(t, out)
	if app["networks"] != nil {
		t.Errorf("networks came back through a merge key left on the app, next to "+
			"network_mode=%v: %v", app["network_mode"], app["networks"])
	}
	if app["working_dir"] != "/mid" {
		t.Errorf("the nested fragment's own key was lost: %v", app["working_dir"])
	}
}

// TestMergeKey_UnmergeableValueIsLeftForComposeToReject covers a `<<` naming
// something that is not a mapping. go-yaml rejects it with "map merge requires
// map or sequence of maps as the value", so docker compose does too.
//
// Consuming it would delete the key and hand back a file that loads, hiding an
// error the user needs to see — the same principle ensureMapping states for a
// populated section it declines to reshape.
func TestMergeKey_UnmergeableValueIsLeftForComposeToReject(t *testing.T) {
	for _, tc := range []struct{ name, frag string }{
		{"null fragment", "x-bad: &bad"},
		{"scalar fragment", "x-bad: &bad just-a-string"},
		{"sequence of scalars", "x-bad: &bad [1, 2]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
			var compose Compose
			if err := yaml.Unmarshal([]byte(tc.frag+`
services:
  app:
    <<: *bad
    image: alpine:3.21
`), &compose); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := idc.ModifyComposeForAgent(&compose, buildAgentOpts(), "app"); err != nil {
				t.Fatalf("ModifyComposeForAgent: %v", err)
			}
			data, err := idc.MarshalCompose(&compose)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var out map[string]interface{}
			err = yaml.Unmarshal(data, &out)
			if err == nil {
				t.Errorf("keploy consumed a merge key docker compose rejects, so the "+
					"generated file loads and the user never sees the error:\n%s", data)
			} else if !strings.Contains(err.Error(), "map merge requires") {
				t.Errorf("unexpected failure: %v", err)
			}
		})
	}
}

// TestMergeKey_AQuotedKeyIsNotAMergeKey pins the tag check. `"<<": value` is a
// plain string key that go-yaml keeps verbatim; matching on Value alone would
// silently delete it.
func TestMergeKey_AQuotedKeyIsNotAMergeKey(t *testing.T) {
	// The value must be a MAPPING to discriminate. With a scalar, dropping the
	// tag check still leaves the key in place — it takes the unmergeable path
	// and is kept for compose to reject — so the fixture proves nothing.
	app := mergeApp(t, mustRun(t, `services:
  app:
    "<<":
      kept: verbatim
    image: alpine:3.21
`))
	held, _ := app["<<"].(map[string]interface{})
	if held == nil || held["kept"] != "verbatim" {
		t.Errorf("a quoted key that merely reads like a merge key was consumed as "+
			"one, and its contents were spliced onto the service: %v", app)
	}
	if app["kept"] != nil {
		t.Errorf("the quoted key's contents leaked onto the service: %v", app["kept"])
	}
}

// TestMergeKey_InheritedContainerNameIsFound covers the one member of this bug
// class that fails loudly: selection runs before anything can be flattened, so
// an inherited container_name was invisible and keploy aborted with "failed to
// find target service" on a file docker compose accepts.
func TestMergeKey_InheritedContainerNameIsFound(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	const in = `x-naming: &naming
  container_name: myapp
services:
  svc:
    <<: *naming
    image: alpine:3.21
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, _, _, found := idc.findContainerInServices(&compose, "myapp"); !found {
		t.Errorf("the gate did not find the service by its inherited container_name")
	}
	var second Compose
	if err := yaml.Unmarshal([]byte(in), &second); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, _, err := idc.findServiceNodeAndName(&second, "myapp"); err != nil {
		t.Errorf("findServiceNodeAndName: %v", err)
	}
}

// TestMergeKey_InheritedDNSAndDependsOnAreHandled covers the two keys keploy
// MOVES and REWRITES rather than appends to, reached through a merge.
func TestMergeKey_InheritedDNSAndDependsOnAreHandled(t *testing.T) {
	out := mustRun(t, `x-common: &common
  dns:
    - 1.1.1.1
  depends_on:
    - db
services:
  app:
    <<: *common
    image: alpine:3.21
  db:
    image: alpine:3.21
`)
	app := mergeApp(t, out)
	if app["dns"] != nil {
		t.Errorf("the inherited dns stayed on the app; it shares keploy-agent's "+
			"netns, so compose rejects dns next to network_mode: %v", app["dns"])
	}
	svcs, _ := out["services"].(map[string]interface{})
	agent, _ := svcs["keploy-agent"].(map[string]interface{})
	if agent["dns"] == nil {
		t.Errorf("dns was not moved onto keploy-agent, which owns the namespace: %v", agent)
	}
	deps, _ := app["depends_on"].(map[string]interface{})
	if deps["db"] == nil {
		t.Errorf("the inherited dependency was lost: %v", deps)
	}
	if deps["keploy-agent"] == nil {
		t.Errorf("the keploy-agent gate is missing: %v", deps)
	}
}

// TestMergeKey_EveryMergeInsideAFragmentIsFollowed is the nested counterpart of
// TestMergeKey_EveryMergeKeyIsConsumed. A fragment carrying two `<<` keys is the
// same invalid-but-parseable shape, and stopping at the first silently drops
// everything the second brings in.
func TestMergeKey_EveryMergeInsideAFragmentIsFollowed(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(`x-one: &one
  working_dir: /one
x-two: &two
  user: someone
x-mid: &mid
  <<: *one
  <<: *two
services:
  app:
    <<: *mid
    image: alpine:3.21
`), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := idc.ModifyComposeForAgent(&compose, buildAgentOpts(), "app"); err != nil {
		t.Fatalf("ModifyComposeForAgent: %v", err)
	}
	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The document cannot be decoded into a map: `x-mid` still carries the
	// duplicate `<<` the user wrote, and keploy does not repair the user's
	// fragment. Read the app service off the node tree instead.
	keys := generatedServiceKeys(t, data, "app")
	if keys["working_dir"] != "/one" {
		t.Errorf("the first nested merge was lost: %q", keys["working_dir"])
	}
	// The discriminating assertion: `user` is reachable ONLY through the second
	// nested merge. Stopping at the first drops it silently — the app then runs
	// as a different user than the compose file says.
	if keys["user"] != "someone" {
		t.Errorf("a key reached only through the SECOND nested merge was lost: %q",
			keys["user"])
	}
	if keys["network_mode"] != "service:keploy-agent" {
		t.Errorf("the app was not wired to the agent: %q", keys["network_mode"])
	}
}

// generatedServiceKeys reads one service's explicit keys straight off the node
// tree of a generated file, for fixtures whose document cannot be decoded into a
// map because the USER's own fragment is invalid.
func generatedServiceKeys(t *testing.T, data []byte, service string) map[string]string {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("generated compose does not parse: %v\n%s", err, data)
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "services" {
			continue
		}
		svcs := root.Content[i+1]
		for j := 0; j+1 < len(svcs.Content); j += 2 {
			if svcs.Content[j].Value != service {
				continue
			}
			out := map[string]string{}
			svc := svcs.Content[j+1]
			for k := 0; k+1 < len(svc.Content); k += 2 {
				out[svc.Content[k].Value] = svc.Content[k+1].Value
			}
			return out
		}
	}
	t.Fatalf("service %q not found in the generated file:\n%s", service, data)
	return nil
}
