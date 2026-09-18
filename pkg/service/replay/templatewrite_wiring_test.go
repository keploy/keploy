package replay

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

/*
Pins that the templatizing writes actually go through WriteTemplatedConfig.

THE BEHAVIOUR IS TESTED; THE WIRING WAS NOT. templatewrite_test.go covers
the helper thoroughly — a dropped field fails a whole-struct comparison —
and that is worth nothing while a caller can simply not call it. Both
shipped call sites were reverted to their original struct literals,
reintroducing the data loss verbatim, and the entire 66-package suite
stayed green.

That is the SAME defect one level out: round 5's test re-typed replay.go's
literal by hand, round 6's tested a helper replay.go was not pinned to
call. The pattern here is depresult_wiring_test.go's, for the same reason
it exists: RunTestSet is ~2000 lines and no test executes it.

WHY A LITERAL IS THE THING TO BAN. Db.Write marshals the whole
models.TestSet over config.yaml, so a composite literal deletes every
field it omits. Three separate sites built one; between them they erased
`metadata:`, `appCommand:`, `preScript:` and `postScript:`. Banning the
SHAPE catches the fourth site before it ships, which naming the fields
never did.

WHY THIS RESOLVES TYPES INSTEAD OF MATCHING TEXT. The first version
looked for a call whose receiver PRINTED as something containing the
string "testSetConf" — a property of today's field name, not of the type.
Its own docstring claimed "a helper variable holding the literal, a
struct built field by field, a fourth site in a new file — all of them
are direct writes, and all of them fail here." An independent reviewer
compiled three evasions into this very package and all three passed:

	store := r.testSetConf              // receiver prints as "store"
	store.Write(ctx, id, ts)

	w := r.testSetConf.Write            // Fun is an Ident, not a Selector
	w(ctx, id, ts)

	func f(cfg TestSetConfig) { ... }   // receiver never says "testSetConf"
	cfg.Write(ctx, id, ts)

So the scan now asks the type checker which method a selector actually
resolves to. A reference to the Write method of any type implementing
TestSetConfig is a finding wherever it appears — including a bare method
value, which is why this looks at every selector rather than only at
calls. Renaming the field, aliasing it, or passing it as a parameter all
resolve to the same method and are all caught.
*/

// loadPackagesForWiring type-checks the packages this invariant covers.
//
// record.go is excluded by package: it creates a brand-new test-set,
// where there is nothing on disk to preserve.
/*
Every GOOS this project ships a binary for.

ONE LOAD PER PLATFORM, unioned. packages.Load runs under the default
build context, so `p.Syntax` holds only the files that build for the host
— and a file the build constraints exclude is invisible to the scan:

	// replay/config_windows.go       (filename constraint alone)
	func windowsOnlyWrite(...) error {
		return r.testSetConf.Write(ctx, id, ts)   // walked past the ban
	}

That is not contrived. This repo already carries utils/appstart_windows.go,
utils/signal_windows.go, utils/reexec_darwin.go, pkg/neterr/neterr_windows.go
and pkg/platform/docker/util_windows.go; keploy ships Windows and macOS
binaries; and per-OS config-path handling is exactly the sort of code
that would touch a test-set config. Someone adds replay/config_windows.go,
writes the literal, Linux CI is green, and Windows users lose `metadata:`
and `appCommand:` — the original bug, on the one platform nothing checks.

It is also cheaper to hit than the third-package helper this docstring
already names: it needs no new package and no deliberate act, just a
filename.

SUBPACKAGES TOO — see the `./...` note on the load below.

GOARCH TOO, not just GOOS. Iterating GOOS alone still ran every load at
the host's architecture, so `zz_x_arm64.go` was invisible — and that is
the MORE likely hole, not a weaker one: darwin/arm64 is where most
keploy Mac users are, linux/arm64 is the container image, and this repo
already carries a `//go:build arm` file.

WHAT IS STILL NOT COVERED, second item: the ban is pinned to
`Db[*models.TestSet]`. `storeWriteMethod` matches the Write SIGNATURE,
and for a generic method the instantiation IS the signature — so
`Db[*enterpriseTestSet].Write` takes `*enterpriseTestSet` as its third
parameter and does not match. That is the anticipated extension point,
not a contrived one: db_test.go ships `extendedTestSet` precisely because
"Db[T] is deliberately generic … so other T's plug in", and there is an
enterprise/ sibling repo. Keying on `fn.Origin()` — which returns the
uninstantiated `testset.Db.Write` — would close it and would also remove
the hardcoded module path below. Not done here because no second
instantiation exists in this repo and the change deserves its own
change-set; it is a stated gap, not a silent one.

WHAT IS STILL NOT COVERED: a file behind a CUSTOM build tag. This repo
has `//go:build dockerlive` (only in pkg/client/app/, not in either
scanned package). Listing the project's tags here would be a second list
to drift, so it is named as a gap rather than half-closed.

`//go:build !cgo` is covered BY ACCIDENT and worth saying so: cross-
compiling to darwin and windows implicitly disables cgo, so at least one
of the five loads sees those files. Nobody chose that — if the platform
list were ever narrowed back to host-native builds it would silently go
dark, and keploy ships CGO_ENABLED=0 static binaries.
*/
var wiringPlatforms = []struct{ GOOS, GOARCH string }{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
	{"windows", "amd64"},
}

func loadPackagesForWiring(t *testing.T) []*packages.Package {
	return loadPackagesFor(t, wiringPlatforms)
}

func loadPackagesFor(
	t *testing.T,
	platforms []struct{ GOOS, GOARCH string },
) []*packages.Package {
	t.Helper()
	var all []*packages.Package
	for _, p := range platforms {
		cfg := &packages.Config{
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
				packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
				packages.NeedImports,
			Tests: false,
			Env:   append(os.Environ(), "GOOS="+p.GOOS, "GOARCH="+p.GOARCH),
		}
		goos := p.GOOS + "/" + p.GOARCH
		// `./...`, NOT `.`. Loading the two package DIRECTORIES missed
		// any subpackage under them — and a subpackage is what "let me
		// pull the config writing into its own package" produces, landing
		// inside the very directory the ban is about:
		//
		//   replay/cfgwriter/w.go:  func Persist(s Store, ...) { s.Write(...) }
		//   replay/x.go:            cfgwriter.Persist(ctx, r.testSetConf, ...)
		//
		// More plausible than the third-package helper this docstring
		// names, which needs editing an unrelated existing package.
		// Neither tree has subpackages today, so the widening costs
		// nothing now and covers the one that would appear.
		pkgs, err := packages.Load(cfg, "./...", "../tools/...")
		if err != nil {
			t.Fatalf("load packages for %s: %v", goos, err)
		}
		var fatal []string
		for _, p := range pkgs {
			for _, e := range p.Errors {
				fatal = append(fatal, e.Error())
			}
		}
		if len(fatal) > 0 {
			// A package that did not type-check yields no selections, and
			// "no selections" is indistinguishable from "no violations".
			t.Fatalf("packages did not type-check for %s, so this test "+
				"would assert nothing:\n  %s", goos, strings.Join(fatal, "\n  "))
		}
		if len(pkgs) == 0 {
			t.Fatalf("no packages loaded for %s; this test would assert nothing", goos)
		}
		all = append(all, pkgs...)
	}
	return all
}

/*
storeWriteMethod reports whether fn is the destructive test-set config
write, identified by its SIGNATURE.

	Write(context.Context, string, *models.TestSet) error

NOT by what else the receiver has. The first version required the
receiver to carry both Write and ReadForUpdate, reasoning that this was
broader than naming replay.TestSetConfig — and it was narrower in the one
direction that matters, because the destructive operation does not need
ReadForUpdate. Every evasion was then "narrow the static type", and an
interface with exactly the wrong shape already exists, exported, in this
repo (record.TestSetConfig: Read + Write). All of these compiled in this
package and passed:

	var w interface {
		Write(context.Context, string, *models.TestSet) error
	} = r.testSetConf
	w.Write(ctx, id, ts)

	func f[T interface{ Write(...) error }](s T) { s.Write(ctx, id, ts) }

	func f(c record.TestSetConfig) { c.Write(ctx, id, ts) }

The signature is what cannot be narrowed away BY AN INTERFACE: any
interface you can call this through declares a method of this exact
shape, whatever else it does or does not have, and that method replaces
the whole config.yaml. (A generic INSTANTIATION does narrow it — see the
second "not covered" item below.)

WHAT THIS DOES NOT COVER, stated rather than implied. The scan reads two
packages FOR EACH SHIPPED GOOS, so a build-constrained file is covered —
but a helper in a THIRD package that hands back the method as a value —

	// in package testset
	func WriterFor(s ConfigStore) func(context.Context, string, *models.TestSet) error {
		return s.Write
	}

— arrives here as a package-qualified ident with no Selections entry and
is invisible. So is reflection. Both need deliberate action in a place
this does not look; neither is reachable by editing replay or tools,
which is where the four real writes lived and where a fifth would go.
The ban is package-scoped, and that is a choice, not an oversight.
*/
func storeWriteMethod(fn *types.Func) bool {
	if fn == nil || fn.Name() != "Write" {
		return false
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil || sig.Variadic() {
		return false
	}
	if sig.Params().Len() != 3 || sig.Results().Len() != 1 {
		return false
	}
	return isContext(sig.Params().At(0).Type()) &&
		isBasic(sig.Params().At(1).Type(), types.String) &&
		isPointerTo(sig.Params().At(2).Type(), "go.keploy.io/server/v3/pkg/models", "TestSet") &&
		isError(sig.Results().At(0).Type())
}

/*
UNALIAS EVERYWHERE.

Since Go 1.23 with gotypesalias=1 — the default on the toolchain this
builds with — go/types materialises a type alias as *types.Alias, so a
bare `t.(*types.Named)` assertion fails for it. Every predicate below
missed any Write whose signature is spelled through one:

	type ctxAlias = context.Context
	type aliasWriter interface {
		Write(ctx ctxAlias, id string, ts *models.TestSet) error
	}
	var w aliasWriter = r.testSetConf
	w.Write(ctx, id, ts)   // walked straight past the ban

Each of the three aliases evades on its own. And this is worse than the
sibling-method bug it replaced, because `type Ctx = context.Context` in a
shared util package is an ordinary thing to write — no one has to be
trying to evade anything.
*/
func isContext(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	if !ok || named.Obj().Name() != "Context" {
		return false
	}
	pkg := named.Obj().Pkg()
	return pkg != nil && pkg.Path() == "context"
}

func isBasic(t types.Type, kind types.BasicKind) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Kind() == kind
}

func isPointerTo(t types.Type, pkgPath, name string) bool {
	ptr, ok := types.Unalias(t).(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := types.Unalias(ptr.Elem()).(*types.Named)
	if !ok || named.Obj().Name() != name {
		return false
	}
	pkg := named.Obj().Pkg()
	return pkg != nil && pkg.Path() == pkgPath
}

func isError(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Name() == "error" && named.Obj().Pkg() == nil
}

type wiringViolation struct {
	Pos  string
	Expr string
}

// directStoreWrites finds every reference to a store's Write method.
func directStoreWrites(pkgs []*packages.Package) []wiringViolation {
	var found []wiringViolation
	for _, p := range pkgs {
		for _, file := range p.Syntax {
			// No _test.go skip: packages.Config sets Tests:false, so
			// p.Syntax never holds one. An explicit skip here read as a
			// deliberate policy decision when it was unreachable code —
			// and a violation in a test file cannot ship anyway.
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				selection, ok := p.TypesInfo.Selections[sel]
				if !ok {
					// A package-qualified identifier, not a method on a
					// value: testset.WriteTemplatedConfig lands here.
					return true
				}
				fn, ok := selection.Obj().(*types.Func)
				if !ok || !storeWriteMethod(fn) {
					return true
				}
				found = append(found, wiringViolation{
					Pos:  p.Fset.Position(sel.Pos()).String(),
					Expr: renderNode(p.Fset, sel),
				})
				return true
			})
		}
	}
	return found
}

func renderNode(fset *token.FileSet, node ast.Node) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, node); err != nil {
		return "<unprintable>"
	}
	return b.String()
}

/*
TestNoDirectTestSetConfigWrite is the invariant, stated positively.

Every templatizing path goes through WriteTemplatedConfig, which reads
the config itself and copies it — so there should be NO reference to a
store's Write method in either package at all.
*/
func TestNoDirectTestSetConfigWrite(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range directStoreWrites(loadPackagesForWiring(t)) {
		// One finding per site. A file that builds for every GOOS is
		// loaded three times and would otherwise be reported three times.
		if seen[v.Pos] {
			continue
		}
		seen[v.Pos] = true
		t.Errorf(
			"%s references the test-set config store's Write method directly:\n  %s\n\n"+
				"Db.Write REPLACES the whole config.yaml, so any value passed to it "+
				"deletes every field it does not carry — that erased metadata, "+
				"appCommand and both scripts across three sites. Use "+
				"testset.WriteTemplatedConfig, which reads the config itself and "+
				"cannot drop a field or be passed the wrong one.",
			v.Pos, v.Expr,
		)
	}
}

/*
The probe file this control compiles into the package.

Removed by TestMain as well as by each subtest's defer: the defer covers
t.Fatalf and a panic, but NOT a `go test -timeout` expiry (which panics
the process), a SIGINT, or a killed CI job. A leftover probe IS a
violation, so TestNoDirectTestSetConfigWrite would then fail for ever and
`git add -A` would commit it — self-inflicted permanent red.
*/
const probeFileName = "zz_wiring_probe.go"

// Every probe this file can leave behind. A leftover IS a violation, so
// TestNoDirectTestSetConfigWrite would fail for ever and `git add -A`
// would commit it.
var probeFiles = []string{
	probeFileName,
	"zz_wiring_probe_windows.go",
	"zz_wiring_probe_darwin.go",
	"zz_wiring_probe_nonlinux.go",
	"zz_wiring_probe_arm64.go",
	"zz_wiring_probe_armtag.go",
}

func TestMain(m *testing.M) {
	removeProbes()
	code := m.Run()
	removeProbes()
	os.Exit(code)
}

func removeProbes() {
	for _, f := range probeFiles {
		_ = os.Remove(f)
	}
	// The subpackage probe is a DIRECTORY. A leftover one is a violation
	// that makes TestNoDirectTestSetConfigWrite fail for ever.
	_ = os.RemoveAll("zzprobepkg")
}

/*
TestTheScanWorks is the positive control, and it exercises the REAL scan
against source that genuinely compiles.

The previous control asserted against a hardcoded string fixture that
spelled `r.testSetConf.Write`, which made it independent of the code it
was controlling for: rename the field and the production scan goes
vacuous while the control still passes. This one writes each evasion into
the package, type-checks it, and requires the scan to see it — so the
control fails for the same reasons the real scan would.
*/
func TestTheScanWorks(t *testing.T) {
	// Each of these evaded the name-matching scan. They are compiled
	// into the package under test, then removed.
	evasions := map[string]string{
		"a direct call":  "_ = r.testSetConf.Write(ctx, id, ts)",
		"a local alias":  "store := r.testSetConf\n\t_ = store.Write(ctx, id, ts)",
		"a method value": "w := r.testSetConf.Write\n\t_ = w(ctx, id, ts)",
		"an interface param": `_ = func(cfg TestSetConfig) error {
		return cfg.Write(ctx, id, ts)
	}(r.testSetConf)`,
		"a type alias in the signature": `type ctxAlias = context.Context
	type tsAlias = models.TestSet
	type errAlias = error
	var w interface {
		Write(ctx ctxAlias, testSetID string, testSet *tsAlias) errAlias
	} = r.testSetConf
	_ = w.Write(ctx, id, ts)`,
	}
	for name, body := range evasions {
		t.Run(name, func(t *testing.T) {
			// ONLY THE BODY UNDER TEST. The previous version emitted a
			// `viaParam` helper unconditionally — and its body is itself
			// a direct Write, in the probe file, in every subtest. So
			// "did the scan see a violation in the probe file?" was
			// satisfied by the helper whether or not the case under test
			// was seen at all: a scan blinded to the direct call, the
			// alias and the method value still passed all four. That is
			// the same vacuity the round before this one diagnosed,
			// re-created one level in.
			src := `package replay

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

func wiringProbe(ctx context.Context, r *Replayer, id string, ts *models.TestSet) {
	` + body + `
}
`
			if err := os.WriteFile(probeFileName, []byte(src), 0o600); err != nil {
				t.Fatalf("write probe: %v", err)
			}
			defer func() { _ = os.Remove(probeFileName) }()

			// ONE PLATFORM for these five. They prove the TYPE
			// RESOLUTION works, not the build context — that is what
			// TestTheScanSeesOtherPlatforms is for — and running each
			// against all five loads costs 20 of the suite's 55
			// package loads for no additional assurance.
			found := directStoreWrites(loadPackagesFor(t, wiringPlatforms[:1]))
			// THE PROBE, not merely "something". Asserting len > 0 made
			// this pass vacuously: with a violation present anywhere else
			// in the package — which is exactly the state whenever
			// TestNoDirectTestSetConfigWrite is red — a scan that was
			//100% blind to all four evasions still satisfied it. The
			// control stopped proving anything in the one situation it
			// exists for.
			var sawProbe bool
			for _, v := range found {
				if strings.Contains(v.Pos, probeFileName) {
					sawProbe = true
				}
			}
			if !sawProbe {
				t.Fatalf("the scan did not see %q in %s — it is not catching "+
					"what it claims to (it reported %d other violation(s))",
					name, probeFileName, len(found))
			}
		})
	}
}

/*
TestTheScanSeesOtherPlatforms is the GOOS half of the control.

A file the build constraints exclude is invisible to a single-context
load, and this repo already carries five `_windows.go`/`_darwin.go` files
— so the cheapest evasion of the whole ban was a filename. Probes here
carry a real constraint, so they compile for their target and not for the
host running the test.
*/
func TestTheScanSeesOtherPlatforms(t *testing.T) {
	body := `package replay

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

func platformOnlyWrite(ctx context.Context, r *Replayer, id string, ts *models.TestSet) error {
	return r.testSetConf.Write(ctx, id, ts)
}
`
	for name, probe := range map[string]struct{ file, src string }{
		"a _windows.go filename constraint": {"zz_wiring_probe_windows.go", body},
		"a _darwin.go filename constraint":  {"zz_wiring_probe_darwin.go", body},
		"an explicit //go:build !linux":     {"zz_wiring_probe_nonlinux.go", "//go:build !linux\n\n" + body},
		"an _arm64.go filename constraint":  {"zz_wiring_probe_arm64.go", body},
		"an explicit //go:build arm64":      {"zz_wiring_probe_armtag.go", "//go:build arm64\n\n" + body},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(probe.file, []byte(probe.src), 0o600); err != nil {
				t.Fatalf("write probe: %v", err)
			}
			defer func() { _ = os.Remove(probe.file) }()

			var sawProbe bool
			for _, v := range directStoreWrites(loadPackagesForWiring(t)) {
				if strings.Contains(v.Pos, probe.file) {
					sawProbe = true
				}
			}
			if !sawProbe {
				t.Fatalf("a write in %s was invisible to the scan — the ban is "+
					"defeated by a filename", probe.file)
			}
		})
	}
}

/*
TestTheScanSeesASubpackage is the `./...` half of the control.

The build-constraint widening got permanent probes; the subpackage
widening got nothing, so `packages.Load(cfg, "./...", "../tools/...")`
could be narrowed back to `"."` — by a revert, a merge resolution, or
someone simplifying it — and the whole suite stayed green. An untested
fix in a test file is the thing this file exists to argue against.

A subpackage is what "let me pull the config writing into its own
package" produces, and it lands inside the very directory the ban is
about.
*/
func TestTheScanSeesASubpackage(t *testing.T) {
	dir := "zzprobepkg"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir probe package: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	pkgSrc := `package zzprobepkg

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

type Store interface {
	Write(ctx context.Context, testSetID string, testSet *models.TestSet) error
}

func Persist(ctx context.Context, s Store, id string, ts *models.TestSet) error {
	return s.Write(ctx, id, ts)
}
`
	if err := os.WriteFile(filepath.Join(dir, "w.go"), []byte(pkgSrc), 0o600); err != nil {
		t.Fatalf("write probe package: %v", err)
	}

	var sawProbe bool
	for _, v := range directStoreWrites(loadPackagesFor(t, wiringPlatforms[:1])) {
		if strings.Contains(v.Pos, filepath.Join(dir, "w.go")) {
			sawProbe = true
		}
	}
	if !sawProbe {
		t.Fatalf("a write in %s/w.go was invisible to the scan — the ban does "+
			"not reach subpackages, so moving the write one directory down "+
			"defeats it", dir)
	}
}

func TestEachTemplatizingCallerUsesTheHelper(t *testing.T) {
	// Banning the write is not enough on its own: a caller could drop
	// the write entirely and silently stop persisting templates.
	for path, wantCalls := range map[string]int{
		// RunTestSet's templatized write, and UpdateTestSetTemplate.
		"replay.go": 2,
		// ProcessTestCasesV2's.
		"../tools/templatize.go": 1,
	} {
		t.Run(path, func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			calls := 0
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if strings.Contains(renderNode(fset, call.Fun), "WriteTemplatedConfig") {
					calls++
				}
				return true
			})
			if calls != wantCalls {
				t.Errorf(
					"%s calls WriteTemplatedConfig %d time(s), expected %d. "+
						"If a templatizing write was added or removed, update this "+
						"count deliberately — the point is that the change is noticed.",
					path, calls, wantCalls,
				)
			}
		})
	}
}
