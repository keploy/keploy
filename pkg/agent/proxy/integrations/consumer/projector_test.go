package consumer_test

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer/consumerfake"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A PROJECTOR PANIC MUST NOT TAKE THE RUN DOWN.
//
// This test exists because the obvious wrapper does exactly that. The design
// says to wrap projector calls in utils.Recover, and utils.Recover calls
// utils.Stop, which calls the process-wide cancel function and tears down the
// whole keploy run — so a decoder bug in ONE mock would abort an entire
// recording or an entire test run. The test installs its own cancel function
// and asserts it was never called, which is what pins the deviation: swapping
// this package's containment for utils.Recover turns this red.
func TestAProjectorPanicIsContainedAndNamedWithoutCancellingTheRun(t *testing.T) {
	defer consumerfake.Register()()

	var cancelled atomic.Bool
	utils.SetCancel(func() { cancelled.Store(true) })
	defer utils.SetCancel(func() {})

	m := consumerfake.Mock(consumerfake.MockOptions{
		Name:  "mock-1",
		Role:  models.RoleEffect,
		Panic: "decoder walked off the end of the buffer",
	})

	views, err := consumer.Project(zap.NewNop(), consumerfake.Protocol, m)
	if err == nil {
		t.Fatal("a panicking projector must produce an error, not a silent empty decode")
	}
	if views != nil {
		t.Fatalf("a panicking projector must produce no views, got %d", len(views))
	}
	var panicked *consumer.ErrProjectorPanic
	if !errors.As(err, &panicked) {
		t.Fatalf("the failure must be NAMED as a projector panic, got %T: %v", err, err)
	}
	if panicked.Protocol != consumerfake.Protocol {
		t.Fatalf("the error must name the protocol, got %q", panicked.Protocol)
	}
	if cancelled.Load() {
		t.Fatal("containing a projector panic cancelled the process-wide context; a decoder bug in one mock must not abort the whole run (this is why utils.Recover is not the wrapper here)")
	}
}

// No projector is a REFUSAL, never a fallback. Guessing at a payload we have
// no decoder for is how a misparse becomes a silent pass.
func TestAMissingProjectorIsARefusal(t *testing.T) {
	m := consumerfake.Mock(consumerfake.MockOptions{Name: "mock-1", Role: models.RoleEffect})
	_, err := consumer.Project(zap.NewNop(), "no-such-protocol", m)
	var missing *consumer.ErrNoProjector
	if !errors.As(err, &missing) {
		t.Fatalf("want ErrNoProjector, got %T: %v", err, err)
	}
	if missing.Protocol != "no-such-protocol" {
		t.Fatalf("protocol %q", missing.Protocol)
	}
}

// A projector that RETURNS an error is the refuse-don't-guess contract working
// as intended; the error must reach the caller unwrapped into a panic report.
func TestAProjectorErrorIsNotMistakenForAPanic(t *testing.T) {
	defer consumerfake.Register()()
	m := consumerfake.Mock(consumerfake.MockOptions{
		Name: "mock-1",
		Role: models.RoleEffect,
		Err:  "Produce v9 is flexible and this decoder does not model it",
	})
	_, err := consumer.Project(zap.NewNop(), consumerfake.Protocol, m)
	if err == nil {
		t.Fatal("want an error")
	}
	var panicked *consumer.ErrProjectorPanic
	if errors.As(err, &panicked) {
		t.Fatal("a declined decode must not be reported as a panic; the two need different remedies")
	}
}

// Two parsers registering for one protocol is a coin flip deciding what a test
// asserts. It fails at process start instead.
func TestDuplicateProjectorRegistrationPanics(t *testing.T) {
	unregister := consumerfake.Register()
	defer unregister()
	defer func() {
		if recover() == nil {
			t.Fatal("registering a second projector for one protocol must panic; silently keeping either one makes the decode depend on package init order")
		}
	}()
	consumer.RegisterProjector(consumerfake.Protocol, consumerfake.Projector{})
}

func TestUnregisterRemovesTheProjector(t *testing.T) {
	unregister := consumerfake.Register()
	if _, ok := consumer.ProjectorFor(consumerfake.Protocol); !ok {
		t.Fatal("Register did not register")
	}
	unregister()
	if _, ok := consumer.ProjectorFor(consumerfake.Protocol); ok {
		t.Fatal("unregister did not unregister; a leaked fake would decode a later test's payloads")
	}
}

// THE FAKE PROTOCOL MUST NEVER REACH A SHIPPED BINARY.
//
// consumerfake is a normal (non-_test) package so that tests in OTHER packages
// — the comparator's table in pkg/service/replay — can import it. That is the
// httptest arrangement, and it is only safe while nothing in production
// imports it. This walks the tree and fails if anything does.
func TestNoProductionFileImportsTheFakeProtocol(t *testing.T) {
	root := repoRoot(t)
	const fakePkg = "go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer/consumerfake"

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The fake's own files obviously reference it.
		if strings.Contains(filepath.ToSlash(path), "/consumerfake/") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil // not our problem; the build catches it
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == fakePkg {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s imports the fake consumer protocol; it is test support and must never be linked into a shipped binary", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the module root")
		}
		dir = parent
	}
}

// THE ADAPTER MUST SURVIVE ITS OWN UNREGISTER.
//
// ProjectorFunc exists so a parser can register a plain function, and the
// registry hands every caller an unregister closure. The obvious registry
// stores the Projector interface and has that closure compare
// `projectors[protocol] == p` — which panics at run time ("comparing
// uncomparable type"), because a func type is not comparable and comparing two
// interface values with an uncomparable dynamic type is a run-time panic, not a
// compile error. The registry therefore keys on the identity of a
// per-registration struct instead. Without this test the whole ProjectorFunc
// path is exercised by nothing and the panic ships.
func TestUnregisteringAFuncProjectorDoesNotPanic(t *testing.T) {
	called := false
	unregister := consumer.RegisterProjector("func-proto", consumer.ProjectorFunc(
		func(_ *models.Mock) ([]models.EffectView, error) {
			called = true
			return nil, nil
		}))
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unregistering a ProjectorFunc panicked: %v", r)
		}
	}()
	if _, ok := consumer.ProjectorFor("func-proto"); !ok {
		t.Fatal("ProjectorFunc did not register")
	}
	if _, err := consumer.Project(zap.NewNop(), "func-proto", consumerfake.Mock(consumerfake.MockOptions{Name: "m"})); err != nil {
		t.Fatalf("Project through a ProjectorFunc: %v", err)
	}
	if !called {
		t.Fatal("the registered function was never called")
	}
	unregister()
	if _, ok := consumer.ProjectorFor("func-proto"); ok {
		t.Fatal("unregister did not unregister")
	}
}

// Unregistering MY registration must not delete a registration that REPLACED
// it. Nothing replaces a projector (a duplicate panics), but the identity rule
// is shared with the deliverer registry, where a reconnect does exactly that,
// so it is pinned on both sides.
func TestUnregisterOnlyRemovesItsOwnRegistration(t *testing.T) {
	first := consumer.RegisterProjector("swap-proto", consumerfake.Projector{})
	// Simulate a replacement the way the deliverer registry allows one.
	first()
	second := consumer.RegisterProjector("swap-proto", consumerfake.Projector{})
	defer second()
	first() // stale closure from the previous registration
	if _, ok := consumer.ProjectorFor("swap-proto"); !ok {
		t.Fatal("a stale unregister closure removed the live registration")
	}
}
