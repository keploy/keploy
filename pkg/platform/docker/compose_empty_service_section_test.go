package docker

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"go.keploy.io/server/v3/config"
)

// keployEnvVars are the variables modifyAppServiceForKeploy writes into the app
// service. Losing them is not a cosmetic problem: NODE_EXTRA_CA_CERTS,
// REQUESTS_CA_BUNDLE, SSL_CERT_FILE and CARGO_HTTP_CAINFO are how Node, Python,
// Requests and Cargo are told to trust keploy's CA, and without them every
// outbound TLS call fails verification against a proxy the app now talks to.
var keployEnvVars = []string{
	"NODE_EXTRA_CA_CERTS",
	"REQUESTS_CA_BUNDLE",
	"SSL_CERT_FILE",
	"CARGO_HTTP_CAINFO",
	"JAVA_TOOL_OPTIONS",
}

// runModifyForAgent drives the real generation path end to end and returns the
// reloaded output. Asserting on a hand-rolled simulation of what keploy does to
// a service cannot notice the simulation drifting from the code.
func runModifyForAgent(t *testing.T, in string) map[string]interface{} {
	t.Helper()
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_, ports, _, _ := idc.findContainerInServices(&compose, "app")
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
	return out
}

func appService(t *testing.T, out map[string]interface{}) map[string]interface{} {
	t.Helper()
	svcs, _ := out["services"].(map[string]interface{})
	app, _ := svcs["app"].(map[string]interface{})
	if app == nil {
		t.Fatalf("app service missing from generated compose: %v", out)
	}
	return app
}

// envKeys returns the variable names present on a service, accepting either the
// list form (`- K=V`) or the mapping form (`K: V`). The writers emit whichever
// shape the user's file already used, so a test that understands only one shape
// silently passes on the other.
func envKeys(t *testing.T, app map[string]interface{}) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	switch env := app["environment"].(type) {
	case []interface{}:
		for _, e := range env {
			s, _ := e.(string)
			for i := 0; i < len(s); i++ {
				if s[i] == '=' {
					got[s[:i]] = true
					break
				}
			}
		}
	case map[string]interface{}:
		for k := range env {
			got[k] = true
		}
	case nil:
		// Left as the caller found it: no variables at all.
	default:
		t.Fatalf("environment came out as %T, which docker compose rejects: %v", env, env)
	}
	return got
}

// TestEmptyServiceSection_NullEnvironmentStillTakesKeploysVars is the regression
// test for the silent half of the empty-section bug.
//
// `environment:` with nothing under it — what commenting a block out leaves
// behind — decodes to a `!!null` SCALAR, not to a zero node. getOrCreateEnvNode
// handed that scalar straight back, and addServiceEnvVar/appendServiceEnvVar
// test for SequenceNode then MappingNode and fall through with no log, so every
// variable keploy writes was dropped. docker compose accepts the result, the app
// starts, and TLS capture fails later for reasons that point nowhere near
// compose generation.
func TestEmptyServiceSection_NullEnvironmentStillTakesKeploysVars(t *testing.T) {
	out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    environment:
    ports:
      - "8080:8080"
`)
	got := envKeys(t, appService(t, out))
	for _, k := range keployEnvVars {
		if !got[k] {
			t.Errorf("%s was dropped; the app runs without keploy's CA and its "+
				"outbound TLS fails verification at replay. got=%v", k, got)
		}
	}
}

// TestEmptyServiceSection_NullVolumesStillTakesTheTLSMount covers the same hole
// in addServiceListProperty: the append landed in a null scalar the encoder
// never emits, so the cert directory the four variables above point at was never
// mounted.
func TestEmptyServiceSection_NullVolumesStillTakesTheTLSMount(t *testing.T) {
	app := appService(t, runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    volumes:
    ports:
      - "8080:8080"
`))
	vols, ok := app["volumes"].([]interface{})
	if !ok {
		t.Fatalf("volumes came out as %T, not a list: %v", app["volumes"], app["volumes"])
	}
	var found bool
	for _, v := range vols {
		if s, _ := v.(string); s == KeployTLSVolumeName+":"+KeployTLSMountPath+":ro" {
			found = true
		}
	}
	if !found {
		t.Errorf("the TLS cert mount was dropped, so the CA paths keploy exports "+
			"point at a directory that does not exist in the container: %v", vols)
	}
}

// TestEmptyServiceSection_EveryEmptyShape walks the shapes YAML gives an empty
// key. Each one reached the writers as something their type switch did not
// match, and each was silently dropped. A single-shape fixture is how the
// top-level version of this bug survived its first fix.
func TestEmptyServiceSection_EveryEmptyShape(t *testing.T) {
	for _, tc := range []struct{ name, env, vols string }{
		{"null", "environment:", "volumes:"},
		{"empty string", `environment: ""`, `volumes: ""`},
		{"explicit null tag", "environment: !!null", "volumes: !!null"},
		{"tilde", "environment: ~", "volumes: ~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := appService(t, runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    `+tc.env+`
    `+tc.vols+`
    ports:
      - "8080:8080"
`))
			got := envKeys(t, app)
			for _, k := range keployEnvVars {
				if !got[k] {
					t.Errorf("%s dropped for %s environment: %v", k, tc.name, got)
				}
			}
			if _, ok := app["volumes"].([]interface{}); !ok {
				t.Errorf("volumes came out as %T for %s: %v",
					app["volumes"], tc.name, app["volumes"])
			}
		})
	}
}

// TestEmptyServiceSection_AliasToAnEmptyFragment covers the same shapes reached
// through an alias. `x-env: &appenv` with nothing under it is a null scalar, so
// resolveAliasByCopy declines it — correctly, since it must not reshape a
// fragment the user wrote — and the alias arrived at the writers holding
// nothing.
//
// The reference is reshaped; the anchored fragment must NOT be. Rewriting a
// user's `&appenv` in place is how the earlier, abandoned approach to the
// top-level bug corrupted every other service sharing it.
func TestEmptyServiceSection_AliasToAnEmptyFragment(t *testing.T) {
	out := runModifyForAgent(t, `x-env: &appenv
x-vols: &appvols
services:
  app:
    image: alpine:3.21
    environment: *appenv
    volumes: *appvols
    ports:
      - "8080:8080"
  sidecar:
    image: alpine:3.21
    environment: *appenv
`)
	app := appService(t, out)
	got := envKeys(t, app)
	for _, k := range keployEnvVars {
		if !got[k] {
			t.Errorf("%s dropped through an aliased empty fragment: %v", k, got)
		}
	}
	if _, ok := app["volumes"].([]interface{}); !ok {
		t.Errorf("volumes came out as %T through an alias: %v", app["volumes"], app["volumes"])
	}

	// The user's fragments stay exactly as written.
	for _, frag := range []string{"x-env", "x-vols"} {
		if v, present := out[frag]; !present || v != nil {
			t.Errorf("%s was reshaped to %#v; the anchored fragment must be left "+
				"as the user wrote it", frag, v)
		}
	}
	// And the sibling, which is not in the agent's network namespace, must not
	// have inherited keploy's CA paths.
	svcs, _ := out["services"].(map[string]interface{})
	sidecar, _ := svcs["sidecar"].(map[string]interface{})
	if sidecar == nil {
		t.Fatalf("sidecar disappeared: %v", svcs)
	}
	if leaked := envKeys(t, sidecar); len(leaked) != 0 {
		t.Errorf("sidecar inherited keploy's variables (%v) while living outside "+
			"the agent's netns, so its TLS fails against a CA path it cannot see", leaked)
	}
}

// TestEmptyServiceSection_EmptyMappingEnvironmentKeepsItsShape pins the one
// carve-out. `environment: {}` already holds entries fine, so it is left alone;
// reshaping it into a list would rewrite the user's chosen style for no gain.
func TestEmptyServiceSection_EmptyMappingEnvironmentKeepsItsShape(t *testing.T) {
	app := appService(t, runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    environment: {}
    ports:
      - "8080:8080"
`))
	if _, ok := app["environment"].(map[string]interface{}); !ok {
		t.Errorf("environment: {} was reshaped to %T; an empty mapping is a usable "+
			"shape and must keep it", app["environment"])
	}
	got := envKeys(t, app)
	for _, k := range keployEnvVars {
		if !got[k] {
			t.Errorf("%s dropped from an empty mapping environment: %v", k, got)
		}
	}
}

// TestEmptyServiceSection_EmptyMappingVolumesBecomesASequence pins the other
// side of that carve-out. A service's volumes list has exactly one legal shape,
// so an empty mapping cannot hold the mount and IS reshaped — appending a lone
// scalar to a mapping node leaves odd Content, which go-yaml emits as an empty
// mapping, dropping the mount without an error.
func TestEmptyServiceSection_EmptyMappingVolumesBecomesASequence(t *testing.T) {
	app := appService(t, runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    volumes: {}
    ports:
      - "8080:8080"
`))
	vols, ok := app["volumes"].([]interface{})
	if !ok {
		t.Fatalf("volumes: {} came out as %T, not a list: %v",
			app["volumes"], app["volumes"])
	}
	if len(vols) == 0 {
		t.Errorf("the TLS mount was dropped into an empty mapping")
	}
}

// TestEmptyServiceSection_PopulatedNodesAreLeftAlone is the guard against the
// fix overreaching. Nothing a user actually wrote may be reshaped or discarded:
// a populated list keeps its entries, a populated mapping keeps its keys, and a
// populated scalar — which docker compose rejects on its own terms — is left
// exactly as written rather than silently replaced.
func TestEmptyServiceSection_PopulatedNodesAreLeftAlone(t *testing.T) {
	t.Run("list and mapping keep their entries", func(t *testing.T) {
		app := appService(t, runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    environment:
      - MINE=1
    volumes:
      - ./data:/data
    ports:
      - "8080:8080"
`))
		if !envKeys(t, app)["MINE"] {
			t.Errorf("the user's own variable was lost: %v", app["environment"])
		}
		vols, _ := app["volumes"].([]interface{})
		var kept bool
		for _, v := range vols {
			if s, _ := v.(string); s == "./data:/data" {
				kept = true
			}
		}
		if !kept {
			t.Errorf("the user's own mount was lost: %v", vols)
		}
	})

	t.Run("populated scalar is not replaced", func(t *testing.T) {
		app := appService(t, runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    environment: "MINE=1"
    ports:
      - "8080:8080"
`))
		if app["environment"] != "MINE=1" {
			t.Errorf("a populated scalar environment was rewritten to %#v; keploy "+
				"must not discard what the user wrote, even on a file compose "+
				"rejects", app["environment"])
		}
	})
}

// TestEmptyServiceSection_ReshapedNodeCarriesNoStaleShape pins the two fields
// the reshape clears that are actually observable in the output. Both are
// cosmetic rather than correctness — docker compose accepts either — but a
// generated file an operator reads while debugging should not carry a `!!null`
// tag on a populated list, and a block-style file should not sprout one flow
// entry.
func TestEmptyServiceSection_ReshapedNodeCarriesNoStaleShape(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	const in = `services:
  app:
    image: alpine:3.21
    environment: !!null
    volumes: {}
    ports:
      - "8080:8080"
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	opts := buildAgentOpts()
	if err := idc.ModifyComposeForAgent(&compose, opts, "app"); err != nil {
		t.Fatalf("ModifyComposeForAgent: %v", err)
	}
	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "!!null") {
		t.Errorf("the reshaped node kept its null tag, which is emitted onto the "+
			"populated list:\n%s", got)
	}
	if strings.Contains(got, "volumes: [") {
		t.Errorf("the reshaped volumes node kept the flow style of the `{}` it "+
			"replaced:\n%s", got)
	}
}

// svcField reads one key off one service in the generated output.
func svcField(t *testing.T, out map[string]interface{}, service, key string) interface{} {
	t.Helper()
	svcs, _ := out["services"].(map[string]interface{})
	s, _ := svcs[service].(map[string]interface{})
	if s == nil {
		t.Fatalf("service %q missing from generated compose: %v", service, svcs)
	}
	return s[key]
}

// TestEmptyServiceSection_DependsOnStillGatesOnTheAgent covers the writer whose
// silent no-op costs the most. Without `keploy-agent: {condition:
// service_healthy}` the app container starts before the agent's proxy is
// listening and races it, which shows up as a few unrecorded calls at the head
// of a session rather than as an error — the hardest possible thing to trace
// back to a compose shape.
func TestEmptyServiceSection_DependsOnStillGatesOnTheAgent(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"null", `services:
  app:
    image: alpine:3.21
    depends_on:
`},
		{"empty sequence", `services:
  app:
    image: alpine:3.21
    depends_on: []
`},
		{"empty mapping", `services:
  app:
    image: alpine:3.21
    depends_on: {}
`},
		{"alias to a shared fragment", `x-deps: &deps
  db:
    condition: service_started
services:
  app:
    image: alpine:3.21
    depends_on: *deps
  db:
    image: alpine:3.21
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runModifyForAgent(t, tc.in)
			deps, ok := svcField(t, out, "app", "depends_on").(map[string]interface{})
			if !ok {
				t.Fatalf("depends_on came out as %T: %v",
					svcField(t, out, "app", "depends_on"),
					svcField(t, out, "app", "depends_on"))
			}
			if deps["keploy-agent"] == nil {
				t.Errorf("the app does not wait for keploy-agent to become healthy, "+
					"so it races the proxy at startup: %v", deps)
			}
			// The user's own dependency must survive the rewrite.
			if tc.name == "alias to a shared fragment" && deps["db"] == nil {
				t.Errorf("the user's own dependency was dropped: %v", deps)
			}
		})
	}
}

// TestEmptyServiceSection_ScalarPropertiesAreSetNotPoked covers addServiceProperty,
// which wrote through `.Value` on whatever node it found. That only works if the
// node is already a scalar:
//
//	network_mode:        -> `network_mode: !!null service:keploy-agent`, which
//	                        does not parse at all
//	network_mode: {}     -> write ignored; interception silently off, the app
//	                        never joins the agent's netns
//	network_mode: *nm    -> overwrites the alias NAME, and marshalling then fails
//	                        with "alias value must contain alphanumerical
//	                        characters only"
func TestEmptyServiceSection_ScalarPropertiesAreSetNotPoked(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"null", `services:
  app:
    image: alpine:3.21
    network_mode:
    pid:
`},
		{"empty mapping", `services:
  app:
    image: alpine:3.21
    network_mode: {}
    pid: {}
`},
		{"empty sequence", `services:
  app:
    image: alpine:3.21
    network_mode: []
    pid: []
`},
		{"alias", `x-nm: &nm host
services:
  app:
    image: alpine:3.21
    network_mode: *nm
`},
		{"populated scalar", `services:
  app:
    image: alpine:3.21
    network_mode: host
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runModifyForAgent(t, tc.in)
			if got := svcField(t, out, "app", "network_mode"); got != "service:keploy-agent" {
				t.Errorf("network_mode is %#v, so the app never joins the agent's "+
					"network namespace and nothing is intercepted", got)
			}
		})
	}
}

// TestEmptyServiceSection_ScalarPropertyKeepsItsComments pins the one thing
// replacing the node could lose that poking .Value did not.
func TestEmptyServiceSection_ScalarPropertyKeepsItsComments(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(`services:
  app:
    image: alpine:3.21
    network_mode: host # and this one
    pid:
      # and a note above the value
      host
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
	// Both comments here sit on the VALUE node, so both are genuinely at risk.
	// A comment on the line above `network_mode:` would attach to the KEY node
	// instead, which nothing replaces — asserting on that would pass no matter
	// what this function does.
	for _, want := range []string{"# and this one", "# and a note above the value"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("replacing the value node dropped %q:\n%s", want, data)
		}
	}
}

// TestTopLevelSection_AnchoredEmptyVolumesStillGetsTheDeclaration pins the
// decision NOT to give ensureMapping the anchor guard ensureSequence has.
//
// `volumes: &v` at the top level with the app's own list aliasing it: the
// service list is detached first, so by the time the top-level append runs
// nothing reads through the anchor any more and reshaping it is a local edit.
// Declining here would leave keploy's volume undeclared while the app mounts it,
// and docker compose rejects that with "refers to undefined volume".
func TestTopLevelSection_AnchoredEmptyVolumesStillGetsTheDeclaration(t *testing.T) {
	out := runModifyForAgent(t, `volumes: &v
services:
  app:
    image: alpine:3.21
    volumes: *v
`)
	vols, ok := out["volumes"].(map[string]interface{})
	if !ok {
		t.Fatalf("top-level volumes came out as %T: %v", out["volumes"], out["volumes"])
	}
	if _, declared := vols[KeployTLSVolumeName]; !declared {
		t.Errorf("keploy's volume was not declared, so the mount it adds to the app "+
			"refers to an undefined volume and compose rejects the file: %v", vols)
	}
}

// TestSharedAnchor_AppIsEditedAndSiblingsAreNot is the regression test for the
// whole anchor family, and for the trap the first two attempts at this fix fell
// into.
//
// keploy does not merely append to the app service: it deletes keys, moves keys
// onto keploy-agent and overwrites values in place. Doing any of that to a node
// the user's file reads through an alias corrupts a service keploy was never
// asked to touch. Reshaping in place leaks; declining leaves the app half-wired
// and records nothing. detachSubtree does neither: it hands every reader an
// inline copy of what it named, then edits freely.
func TestSharedAnchor_AppIsEditedAndSiblingsAreNot(t *testing.T) {
	t.Run("populated environment", func(t *testing.T) {
		out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    environment: &appenv
      - MINE=1
  sidecar:
    image: alpine:3.21
    environment: *appenv
`)
		if got := envKeys(t, appService(t, out)); !got["MINE"] || !got["SSL_CERT_FILE"] {
			t.Errorf("the app must keep its own variable AND gain keploy's: %v", got)
		}
		sib := envKeys(t, mustService(t, out, "sidecar"))
		if !sib["MINE"] {
			t.Errorf("the sidecar lost the variable it actually declared: %v", sib)
		}
		for _, k := range keployEnvVars {
			if sib[k] {
				t.Errorf("the sidecar inherited %s through the shared anchor; it is "+
					"not in the agent's netns, so its TLS now fails against a CA "+
					"path it cannot see: %v", k, sib)
			}
		}
	})

	t.Run("empty environment", func(t *testing.T) {
		out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    environment: &appenv
  sidecar:
    image: alpine:3.21
    environment: *appenv
`)
		got := envKeys(t, appService(t, out))
		for _, k := range keployEnvVars {
			if !got[k] {
				t.Errorf("%s was dropped; an anchor on the app's own key is not a "+
					"reason to leave the app uninstrumented: %v", k, got)
			}
		}
		if sib := mustService(t, out, "sidecar")["environment"]; sib != nil {
			t.Errorf("the sidecar's environment became %v; it named an empty "+
				"fragment and must stay empty", sib)
		}
	})

	t.Run("volumes shared with the top-level section", func(t *testing.T) {
		out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    volumes: &appvols
volumes: *appvols
`)
		vols, ok := out["volumes"].(map[string]interface{})
		if !ok {
			t.Fatalf("top-level volumes came out as %T; compose requires a mapping "+
				"and rejects the file: %v", out["volumes"], out["volumes"])
		}
		if _, declared := vols[KeployTLSVolumeName]; !declared {
			t.Errorf("keploy's volume was not declared: %v", vols)
		}
		if _, ok := appService(t, out)["volumes"].([]interface{}); !ok {
			t.Errorf("the app's volumes must be a list, got %T",
				appService(t, out)["volumes"])
		}
	})

	t.Run("a scalar property the sibling reads", func(t *testing.T) {
		out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    network_mode: &nm host
  other:
    image: alpine:3.21
    network_mode: *nm
`)
		if got := appService(t, out)["network_mode"]; got != "service:keploy-agent" {
			t.Errorf("network_mode is %#v, so the app never joins the agent's "+
				"netns and a record run captures nothing", got)
		}
		if got := mustService(t, out, "other")["network_mode"]; got != "host" {
			t.Errorf("the sibling's network_mode became %#v", got)
		}
	})

	t.Run("a scalar property nobody reads", func(t *testing.T) {
		// The shape an earlier version of this fix broke: an anchor with no
		// alias anywhere. Declining left `pid` set and `network_mode` not, which
		// docker compose accepts and which intercepts nothing.
		out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    network_mode: &nm host
`)
		app := appService(t, out)
		if app["network_mode"] != "service:keploy-agent" || app["pid"] != "service:keploy-agent" {
			t.Errorf("the app was left half-wired: network_mode=%#v pid=%#v",
				app["network_mode"], app["pid"])
		}
	})
}

// TestSharedAnchor_DeletedAndMovedKeysKeepTheFileLoadable covers the two writers
// that did not merely corrupt the file but made it unloadable.
//
// keploy DELETES the app's `networks:` and `ports:`, and MOVES its `dns*` onto
// keploy-agent. When the app owns the anchor definition, deleting it dangles
// every `*ref` — `yaml: unknown anchor 'n' referenced` — and moving it puts the
// definition AFTER the reference, which fails the same way. Both produced a file
// that does not parse at all, from a file docker compose accepted.
func TestSharedAnchor_DeletedAndMovedKeysKeepTheFileLoadable(t *testing.T) {
	for _, tc := range []struct{ name, key, in string }{
		{"deleted networks", "networks", `services:
  app:
    image: alpine:3.21
    networks: &shared
      - default
  sidecar:
    image: alpine:3.21
    networks: *shared
`},
		{"moved dns", "dns", `services:
  app:
    image: alpine:3.21
    dns: &shared
      - 1.1.1.1
  sidecar:
    image: alpine:3.21
    dns: *shared
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// runModifyForAgent fails the test if the output does not reload,
			// which is the failure this guards against.
			out := runModifyForAgent(t, tc.in)
			got, _ := mustService(t, out, "sidecar")[tc.key].([]interface{})
			if len(got) != 1 {
				t.Errorf("the sidecar's %s is %v; it must still name exactly what "+
					"the user wrote", tc.key, mustService(t, out, "sidecar")[tc.key])
			}
		})
	}
}

// TestSharedAnchor_TopLevelSectionIsNotCarriedIntoAnotherKey covers the section
// append rather than the service. An anchored top-level `volumes:` shared with a
// service's `networks:` is valid compose; appending keploy's volume into the
// shared node made it invalid — "refers to undefined network keploy-tls-certs".
func TestSharedAnchor_TopLevelSectionIsNotCarriedIntoAnotherKey(t *testing.T) {
	out := runModifyForAgent(t, `volumes: &shared {}
networks:
  mynet:
services:
  app:
    image: alpine:3.21
  other:
    image: alpine:3.21
    networks: *shared
`)
	nets, _ := mustService(t, out, "other")["networks"].(map[string]interface{})
	if _, leaked := nets[KeployTLSVolumeName]; leaked {
		t.Errorf("keploy's VOLUME was appended into another service's networks: %v", nets)
	}
	vols, ok := out["volumes"].(map[string]interface{})
	if !ok {
		t.Fatalf("top-level volumes came out as %T", out["volumes"])
	}
	if _, declared := vols[KeployTLSVolumeName]; !declared {
		t.Errorf("keploy's volume was not declared: %v", vols)
	}
}

func mustService(t *testing.T, out map[string]interface{}, name string) map[string]interface{} {
	t.Helper()
	svcs, _ := out["services"].(map[string]interface{})
	s, _ := svcs[name].(map[string]interface{})
	if s == nil {
		t.Fatalf("service %q missing from generated compose: %v", name, svcs)
	}
	return s
}

// TestGate_PortsAndNetworksReadThroughEmptyAndAliasedNodes covers the read path
// that runs BEFORE anything is modified. Its results become opts.AppPorts and
// the agent's network aliases, so what it gets wrong is wrong on keploy-agent,
// not on the app.
func TestGate_PortsAndNetworksReadThroughEmptyAndAliasedNodes(t *testing.T) {
	parse := func(t *testing.T, in string) ([]string, []string) {
		t.Helper()
		idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
		var compose Compose
		if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		nets, ports, _, found := idc.findContainerInServices(&compose, "app")
		if !found {
			t.Fatalf("gate did not find the app service")
		}
		return nets, ports
	}

	t.Run("aliased ports still reach keploy-agent", func(t *testing.T) {
		// keploy-agent publishes on the app's behalf under
		// `network_mode: service:keploy-agent`. Read empty, the app has no
		// published port at all and is unreachable from the host.
		_, ports := parse(t, `x-ports: &appports
  - "8080:8080"
services:
  app:
    image: alpine:3.21
    ports: *appports
`)
		if len(ports) != 1 || ports[0] != "8080:8080" {
			t.Errorf("ports read through an alias came back %#v", ports)
		}
	})

	t.Run("aliased networks still reach keploy-agent", func(t *testing.T) {
		nets, _ := parse(t, `x-nets: &appnets
  - mynet
services:
  app:
    image: alpine:3.21
    networks: *appnets
`)
		if len(nets) != 1 || nets[0] != "mynet" {
			t.Errorf("networks read through an alias came back %#v", nets)
		}
	})

	t.Run("empty ports and networks yield nothing, not an empty name", func(t *testing.T) {
		// `[""]` was copied onto keploy-agent, where compose rejects it with
		// `invalid proto: ` and `additional properties '' not allowed`.
		nets, ports := parse(t, `services:
  app:
    image: alpine:3.21
    ports:
    networks:
`)
		for _, p := range ports {
			if p == "" {
				t.Errorf("an empty port name reached keploy-agent: %#v", ports)
			}
		}
		for _, n := range nets {
			if n == "" {
				t.Errorf("an empty network name reached keploy-agent: %#v", nets)
			}
		}
		if len(nets) != 1 || nets[0] != "default" {
			t.Errorf("an empty `networks:` must fall back to default, got %#v", nets)
		}
	})
}

// TestDetach_ExpansionIsDeepAndIndependent pins the properties of detachSubtree
// that a single obvious implementation gets wrong.
//
// It does NOT pin the retry loop: measured, every case here passes with a single
// pass, because references are collected up front and expanded in walk order, so
// the inner one is always reached first. detachSubtree's own comment says the
// same. What the nested case below does pin is that aliasRefsInto recurses at
// all.
func TestDetach_ExpansionIsDeepAndIndependent(t *testing.T) {
	t.Run("copies are independent, not slice-sharing", func(t *testing.T) {
		// keploy APPENDS to JAVA_TOOL_OPTIONS by rewriting the scalar in place.
		// A copy that shares children shows that rewrite to the sibling, and an
		// inherited -javaagent aborts the JVM at initialisation — the sibling
		// does not start at all.
		out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    environment: &appenv
      - JAVA_TOOL_OPTIONS=-Xmx1g
  sidecar:
    image: alpine:3.21
    environment: *appenv
`)
		sib, _ := mustService(t, out, "sidecar")["environment"].([]interface{})
		if len(sib) != 1 || sib[0] != "JAVA_TOOL_OPTIONS=-Xmx1g" {
			t.Errorf("the sidecar's own entry was rewritten in place to %v; the copy "+
				"shared children with the node keploy edits", sib)
		}
	})

	t.Run("a reference nested inside a fragment is expanded", func(t *testing.T) {
		// `sidecar.labels` names a fragment that itself names another anchor
		// inside the app service. A search that stops at the top level of each
		// node never reaches the inner reference, which then points at an anchor
		// about to be cleared — and the generated file fails to load.
		// runModifyForAgent fails the test if it does not reload.
		out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    environment: &appenv
      - MINE=1
    labels: &applabels
      inherited: *appenv
  sidecar:
    image: alpine:3.21
    labels: *applabels
`)
		labels, _ := mustService(t, out, "sidecar")["labels"].(map[string]interface{})
		got, _ := labels["inherited"].([]interface{})
		if len(got) != 1 || got[0] != "MINE=1" {
			t.Errorf("the nested reference did not survive expansion: %v", labels)
		}
	})

	t.Run("a reference from an x- fragment is expanded", func(t *testing.T) {
		// `x-*` is where the compose spec puts reusable fragments, and it is not
		// one of the sections the Compose struct names — so it is only reachable
		// through raw. Missing it dangles the reference.
		out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    environment: &appenv
      - MINE=1
x-audit: *appenv
`)
		got, _ := out["x-audit"].([]interface{})
		if len(got) != 1 || got[0] != "MINE=1" {
			t.Errorf("x-audit came out as %v; it must still name what the user "+
				"wrote, not keploy's edited copy", out["x-audit"])
		}
	})
}

// TestDetach_ExpandedCopiesCarryNoAnchor pins stripAnchors on the copy. Without
// it the expansion emits a SECOND definition of the same name, and a later `*nm`
// binds to whichever the parser saw last. The node keploy overwrote here is
// replaced wholesale rather than edited, so this fixture says nothing about the
// anchor-clearing pass — TestDetach_EditedNodesDoNotKeepTheirAnchor covers that.
func TestDetach_ExpandedCopiesCarryNoAnchor(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(`services:
  app:
    image: alpine:3.21
    network_mode: &nm host
  other:
    image: alpine:3.21
    network_mode: *nm
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
	if n := strings.Count(string(data), "&nm"); n != 0 {
		t.Errorf("the generated file carries %d definition(s) of a detached anchor:\n%s",
			n, data)
	}
	// And it must still load: an expansion that drops the definition while a
	// reference survives is the failure this whole area exists to prevent.
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
}

// TestDetach_EditedNodesDoNotKeepTheirAnchor is the same tidy-up for the nodes
// keploy edits IN PLACE rather than replaces — the app's `environment:`, and the
// top-level `volumes:` section. Both keep their node, so an anchor left on
// either ends up naming a list or mapping that is now half keploy's.
func TestDetach_EditedNodesDoNotKeepTheirAnchor(t *testing.T) {
	for _, tc := range []struct{ name, anchor, in string }{
		{"service environment", "&appenv", `services:
  app:
    image: alpine:3.21
    environment: &appenv
      - MINE=1
  sidecar:
    image: alpine:3.21
    environment: *appenv
`},
		{"top-level volumes", "&shared", `volumes: &shared {}
services:
  app:
    image: alpine:3.21
  other:
    image: alpine:3.21
    networks: *shared
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
			var compose Compose
			if err := yaml.Unmarshal([]byte(tc.in), &compose); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := idc.ModifyComposeForAgent(&compose, buildAgentOpts(), "app"); err != nil {
				t.Fatalf("ModifyComposeForAgent: %v", err)
			}
			data, err := idc.MarshalCompose(&compose)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if n := strings.Count(string(data), tc.anchor); n != 0 {
				t.Errorf("%s survived on a node keploy edited (%d occurrence(s)); it "+
					"now names content that is partly keploy's:\n%s", tc.anchor, n, data)
			}
		})
	}
}

// TestDetach_ReferencesFromEverySectionAreExpanded pins the sections in
// documentRoots that keploy never writes to. They are still load-bearing: a
// reference to a node keploy edits can sit under any top-level key, and one that
// is missed dangles when the anchor is cleared — the generated file then does
// not load at all.
func TestDetach_ReferencesFromEverySectionAreExpanded(t *testing.T) {
	for _, section := range []string{"networks", "configs", "secrets"} {
		t.Run(section, func(t *testing.T) {
			// runModifyForAgent fails the test if the output does not reload.
			out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    volumes: &appvols
`+section+`: *appvols
`)
			// The reload inside runModifyForAgent is the assertion here: a
			// missed reference dangles once the anchor is cleared, and the file
			// does not parse. The expanded section itself is a copy of the empty
			// fragment, so there is nothing else meaningful to assert on it —
			// checking its type would pass in every state, fixed or broken.
			if _, present := out[section]; !present {
				t.Errorf("%s disappeared from the generated file", section)
			}
		})
	}
}

// TestEnvMapping_AliasedValuesAreReplacedNotOverwritten covers the mapping-style
// environment writers. `SSL_CERT_FILE: *ca` is an alias, and assigning to its
// Value overwrites the anchor NAME — marshalling then fails outright and keploy
// aborts on a compose file docker accepts. detachSubtree does not cover it: the
// anchor lives in an `x-*` fragment, outside the app service.
func TestEnvMapping_AliasedValuesAreReplacedNotOverwritten(t *testing.T) {
	t.Run("addServiceEnvVar replaces an aliased value", func(t *testing.T) {
		out := runModifyForAgent(t, `x-ca: &ca /etc/ssl/mine.pem
services:
  app:
    image: alpine:3.21
    environment:
      SSL_CERT_FILE: *ca
`)
		env, _ := appService(t, out)["environment"].(map[string]interface{})
		if env["SSL_CERT_FILE"] != "/tmp/keploy-tls/ca.crt" {
			t.Errorf("SSL_CERT_FILE is %#v, not keploy's CA", env["SSL_CERT_FILE"])
		}
		// The user's fragment is theirs; keploy must not have edited it.
		if out["x-ca"] != "/etc/ssl/mine.pem" {
			t.Errorf("the anchored fragment was rewritten to %#v", out["x-ca"])
		}
	})

	t.Run("appendServiceEnvVar reads through an aliased value", func(t *testing.T) {
		out := runModifyForAgent(t, `x-jopts: &jopts -Xmx1g
services:
  app:
    image: alpine:3.21
    environment:
      JAVA_TOOL_OPTIONS: *jopts
`)
		env, _ := appService(t, out)["environment"].(map[string]interface{})
		got, _ := env["JAVA_TOOL_OPTIONS"].(string)
		if !strings.Contains(got, "-Xmx1g") {
			t.Errorf("the user's own JVM options were lost: %#v", got)
		}
		if !strings.Contains(got, "trustStore") {
			t.Errorf("keploy's trust store was not appended: %#v", got)
		}
		if strings.Contains(got, "jopts") {
			t.Errorf("the ANCHOR NAME was treated as the existing value: %#v", got)
		}
	})
}

// TestGate_AliasElementsInsideAListAreRead covers the shape one level below the
// list itself. An aliased ELEMENT was dropped, and for networks that is worse
// than reading nothing: the `default` fallback then fires, so keploy-agent joins
// `default` while the app — which shares the agent's netns — silently loses the
// network its database is on.
func TestGate_AliasElementsInsideAListAreRead(t *testing.T) {
	parse := func(t *testing.T, in string) ([]string, []string) {
		t.Helper()
		idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
		var compose Compose
		if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		nets, ports, _, found := idc.findContainerInServices(&compose, "app")
		if !found {
			t.Fatalf("gate did not find the app service")
		}
		return nets, ports
	}

	t.Run("networks", func(t *testing.T) {
		nets, _ := parse(t, `x-net: &appnet mynet
services:
  app:
    image: alpine:3.21
    networks:
      - *appnet
`)
		if len(nets) != 1 || nets[0] != "mynet" {
			t.Errorf("aliased network element came back %#v; keploy-agent would be "+
				"put on the wrong network and the app could not reach its database", nets)
		}
	})

	t.Run("ports", func(t *testing.T) {
		_, ports := parse(t, `x-port: &appport "8080:8080"
services:
  app:
    image: alpine:3.21
    ports:
      - *appport
`)
		if len(ports) != 1 || ports[0] != "8080:8080" {
			t.Errorf("aliased port element came back %#v; the app would publish "+
				"nothing and be unreachable from the host", ports)
		}
	})

	t.Run("an empty element is skipped, not named \"\"", func(t *testing.T) {
		// A blank entry in the list yields an empty scalar. Copied onto
		// keploy-agent it becomes `additional properties '' not allowed` for
		// networks and `invalid proto: ` for ports.
		nets, ports := parse(t, `services:
  app:
    image: alpine:3.21
    networks:
      -
      - mynet
    ports:
      -
      - "8080:8080"
`)
		for _, n := range nets {
			if n == "" {
				t.Errorf("an empty network name reached keploy-agent: %#v", nets)
			}
		}
		for _, p := range ports {
			if p == "" {
				t.Errorf("an empty port reached keploy-agent: %#v", ports)
			}
		}
		if len(nets) != 1 || len(ports) != 1 {
			t.Errorf("the real entries were lost: nets=%#v ports=%#v", nets, ports)
		}
	})

	t.Run("extended port form with an aliased value", func(t *testing.T) {
		_, ports := parse(t, `x-pub: &pub 8080
services:
  app:
    image: alpine:3.21
    ports:
      - target: 80
        published: *pub
`)
		if len(ports) != 1 || ports[0] != "8080:80" {
			t.Errorf("published read through an alias came back %#v; the host port "+
				"is dropped and only the container port is published", ports)
		}
	})
}

// TestEnvMapping_ReplacedValueKeepsItsComments pins what replacing a value node
// could lose that writing through it did not. Both shapes here put the comment
// on the VALUE node rather than the key, so both are genuinely at risk.
func TestEnvMapping_ReplacedValueKeepsItsComments(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(`services:
  app:
    image: alpine:3.21
    environment:
      SSL_CERT_FILE: /etc/mine/ca.pem # trailing note
      JAVA_TOOL_OPTIONS:
        # note above the value
        -Xmx1g
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
	for _, want := range []string{"# trailing note", "# note above the value"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("replacing the value node dropped %q:\n%s", want, data)
		}
	}
}

// TestListElements_AliasesAreReadThroughAndReplaced covers the sequence-style
// writers, the sibling branch of the mapping-style ones. An element can be an
// alias, and an alias node's own Value is the anchor NAME — so the "does this
// key already exist" scan matched nothing.
func TestListElements_AliasesAreReadThroughAndReplaced(t *testing.T) {
	t.Run("environment does not gain a duplicate key", func(t *testing.T) {
		// Unmatched, keploy APPENDED a second JAVA_TOOL_OPTIONS entry. docker
		// compose takes the last one, so the user's -Xmx1g is silently gone and
		// the container starts with a heap it was never configured for.
		out := runModifyForAgent(t, `x-jopts: &jopts "JAVA_TOOL_OPTIONS=-Xmx1g"
services:
  app:
    image: alpine:3.21
    environment:
      - *jopts
`)
		env, _ := appService(t, out)["environment"].([]interface{})
		var seen int
		var value string
		for _, e := range env {
			s, _ := e.(string)
			if strings.HasPrefix(s, "JAVA_TOOL_OPTIONS=") {
				seen++
				value = s
			}
		}
		if seen != 1 {
			t.Fatalf("JAVA_TOOL_OPTIONS appears %d times; compose keeps the last, "+
				"so one of them is silently discarded: %v", seen, env)
		}
		if !strings.Contains(value, "-Xmx1g") {
			t.Errorf("the user's own JVM options were lost: %q", value)
		}
		if !strings.Contains(value, "trustStore") {
			t.Errorf("keploy's trust store was not appended: %q", value)
		}
	})

	t.Run("an aliased CA variable is replaced, not duplicated", func(t *testing.T) {
		// addServiceEnvVar's sequence branch, the sibling of the append case
		// above. Unmatched, keploy adds a second SSL_CERT_FILE entry; compose
		// keeps the last, so which CA the app trusts depends on ordering.
		out := runModifyForAgent(t, `x-ca: &ca "SSL_CERT_FILE=/etc/mine/ca.pem"
services:
  app:
    image: alpine:3.21
    environment:
      - *ca
`)
		env, _ := appService(t, out)["environment"].([]interface{})
		var seen int
		var value string
		for _, e := range env {
			str, _ := e.(string)
			if strings.HasPrefix(str, "SSL_CERT_FILE=") {
				seen++
				value = str
			}
		}
		if seen != 1 {
			t.Fatalf("SSL_CERT_FILE appears %d times; which CA the app trusts then "+
				"depends on entry order: %v", seen, env)
		}
		if value != "SSL_CERT_FILE=/tmp/keploy-tls/ca.crt" {
			t.Errorf("SSL_CERT_FILE is %q, not keploy's CA", value)
		}
	})

	t.Run("an empty depends_on element is skipped", func(t *testing.T) {
		// A blank entry would become a dependency named "", which compose
		// rejects — and it is keploy that put it there.
		out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    depends_on:
      -
      - db
  db:
    image: alpine:3.21
`)
		deps, _ := appService(t, out)["depends_on"].(map[string]interface{})
		if _, blank := deps[""]; blank {
			t.Errorf("an empty dependency name reached the generated file: %v", deps)
		}
		if deps["db"] == nil || deps["keploy-agent"] == nil {
			t.Errorf("a real dependency was lost: %v", deps)
		}
	})

	t.Run("depends_on keeps an aliased dependency", func(t *testing.T) {
		out := runModifyForAgent(t, `x-db: &db db
services:
  app:
    image: alpine:3.21
    depends_on:
      - *db
  db:
    image: alpine:3.21
`)
		deps, _ := appService(t, out)["depends_on"].(map[string]interface{})
		if deps["db"] == nil {
			t.Errorf("the app's own dependency was dropped during the seq→map "+
				"rewrite, so it no longer waits for its database: %v", deps)
		}
		if deps["keploy-agent"] == nil {
			t.Errorf("the keploy-agent gate is missing: %v", deps)
		}
	})

	t.Run("an aliased container_name is matched", func(t *testing.T) {
		// The lookup compared against the alias node's own Value — the anchor
		// name — so keploy aborted with "failed to find target service" on a file
		// docker compose accepts.
		idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
		var compose Compose
		if err := yaml.Unmarshal([]byte(`x-name: &appname myapp
services:
  svc:
    image: alpine:3.21
    container_name: *appname
`), &compose); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, _, name, found := idc.findContainerInServices(&compose, "myapp"); !found {
			t.Errorf("the gate did not find the service (name=%q)", name)
		}
		if _, _, err := idc.findServiceNodeAndName(&compose, "myapp"); err != nil {
			t.Errorf("findServiceNodeAndName: %v", err)
		}
	})
}
