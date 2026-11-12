package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type nopComposeStack struct{ spec ComposeStackSpec }

func (s *nopComposeStack) ProjectName() string { return s.spec.ProjectName }
func (*nopComposeStack) Up(context.Context, ComposeUpOptions, io.Writer, io.Writer) error {
	return nil
}
func (*nopComposeStack) Down(context.Context, time.Duration) error  { return nil }
func (*nopComposeStack) Ps(context.Context) ([]ServiceState, error) { return nil, nil }
func (*nopComposeStack) ContainerIDsForService(context.Context, string) ([]string, error) {
	return nil, nil
}

// A build that registers no runner keeps the shell-out: nothing is
// registered, and asking for a stack is an error, never a nil stack.
func TestNoComposeStackIsRegisteredByDefault(t *testing.T) {
	if ComposeStackRegistered() {
		t.Fatal("a compose runner is registered in a build that registers none")
	}
	s, err := NewComposeStack(context.Background(), ComposeStackSpec{Content: []byte("services: {}\n")})
	if err == nil || s != nil {
		t.Fatalf("NewComposeStack with nothing registered = (%v, %v); want (nil, an error)", s, err)
	}
}

// The registered builder gets the spec it is asked for, and nil unregisters.
func TestARegisteredComposeStackBuildsEachStack(t *testing.T) {
	RegisterComposeStack(func(_ context.Context, spec ComposeStackSpec) (ComposeStack, error) {
		return &nopComposeStack{spec: spec}, nil
	})
	t.Cleanup(func() { RegisterComposeStack(nil) })

	if !ComposeStackRegistered() {
		t.Fatal("ComposeStackRegistered is false after RegisterComposeStack")
	}
	spec := ComposeStackSpec{Content: []byte("services: {}\n"), ProjectName: "p1", WorkingDir: "/w", EnvFiles: []string{"a.env"}}
	s, err := NewComposeStack(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.(*nopComposeStack).spec; got.ProjectName != "p1" || got.WorkingDir != "/w" || len(got.EnvFiles) != 1 || string(got.Content) != string(spec.Content) {
		t.Fatalf("the builder got %+v; want %+v", got, spec)
	}

	RegisterComposeStack(nil)
	if ComposeStackRegistered() {
		t.Fatal("ComposeStackRegistered is true after RegisterComposeStack(nil)")
	}
}

// Options built without OnExit must not stop the stack at a one-shot
// container's exit: the zero value is the CLI's no-flag behaviour.
func TestComposeOnExitZeroValueIsIgnore(t *testing.T) {
	var opts ComposeUpOptions
	if opts.OnExit != ComposeOnExitIgnore {
		t.Fatalf("zero OnExit is %v; want ignore", opts.OnExit)
	}
	for c, want := range map[ComposeOnExit]string{ComposeOnExitIgnore: "ignore", ComposeOnExitStop: "stop", ComposeOnExitFail: "fail", ComposeOnExit(9): "unknown"} {
		if got := c.String(); got != want {
			t.Errorf("ComposeOnExit(%d).String() = %q; want %q", int(c), got, want)
		}
	}
}

func TestComposeExitErrorCarriesItsCause(t *testing.T) {
	cause := errors.New("service app exited with code 3")
	err := fmt.Errorf("up: %w", &ComposeExitError{Code: 3, Err: cause})
	var exitErr *ComposeExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 {
		t.Fatalf("errors.As found %v; want code 3", exitErr)
	}
	if !errors.Is(err, cause) {
		t.Fatal("the cause is not reachable through ComposeExitError")
	}
	if got := (&ComposeExitError{Code: 3}).Error(); !strings.Contains(got, "3") {
		t.Fatalf("Error() without a cause = %q; want the status in it", got)
	}
	if got := (&ComposeExitError{Code: 3, Err: cause}).Error(); got != cause.Error() {
		t.Fatalf("Error() = %q; want the cause's %q", got, cause.Error())
	}
}

// A line is held only for the project's current run: one test set's Up never
// shows the previous set's lines, and the returned line is the cut one.
func TestAppOutputRunsKeepOnlyTheCurrentRunsLines(t *testing.T) {
	t.Cleanup(ForgetAppOutput)
	first := StartAppOutputRun("stack-run")
	if got := RecordAppOutputOfRun("stack-run", first, "app-1", "app", "first set"); got != "first set" {
		t.Fatalf("RecordAppOutputOfRun returned %q; want the line", got)
	}
	second := StartAppOutputRun("stack-run")
	if second == first {
		t.Fatal("a new run has the previous run's id")
	}
	if tail := string(AppOutputTail("stack-run", "app-1")); strings.Contains(tail, "first set") {
		t.Fatalf("a new run still holds the previous run's line: %q", tail)
	}
	RecordAppOutputOfRun("stack-run", first, "app-1", "app", "late line of the first set")
	RecordAppOutputOfRun("stack-run", second, "app-1", "app", "second set")
	tail := string(AppOutputTail("stack-run", "app-1"))
	if strings.Contains(tail, "late line") || !strings.Contains(tail, "second set") {
		t.Fatalf("tail %q; want the second run's line alone", tail)
	}
	if svc := string(ServiceOutputTail("stack-run", "app")); !strings.Contains(svc, "second set") {
		t.Fatalf("service tail %q; want the line recorded under its service", svc)
	}
}
