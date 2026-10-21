package cli

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven model keploy mock's --help gives only what keploy runs: a
// Go suite, natively, and this build's replay with KEPLOY_PROVEN=0, as its
// refusal says (this build does not judge each test, so under the proven
// model it refuses every replay). No pytest suite, which has no keploy
// integration in this release, and no --on-miss but fail. A build that judges
// each test (keploy enterprise) gives MockExampleJudged's replay, without the
// prefix. Without the switch the help is as it was. Mutation: keep the pytest
// examples under the switch; drop the replay's prefix; change them without
// the switch.
func TestMockHelpGivesOnlyWhatTheSwitchRuns(t *testing.T) {
	build := func() (parent, record, replay string) {
		ctx := context.Background()
		cmd := Mock(ctx, zap.NewNop(), nil, agentSvcFactory{}, noFlags{})
		for _, sub := range cmd.Commands() {
			switch sub.Name() {
			case "record":
				record = sub.Example
			case "replay":
				replay = sub.Example
			}
		}
		return cmd.Long, record, replay
	}

	t.Setenv(provenswitch.Env, "1")
	long, record, replay := build()
	for _, text := range []string{long, record, replay} {
		if strings.Contains(text, "pytest") || strings.Contains(text, "--on-miss record") || strings.Contains(text, "--on-miss passthrough") {
			t.Errorf("under the proven model keploy mock's help gives what it does not run:\n%s", text)
		}
	}
	if want := `keploy mock record -c "go test -p 1 -json ./..." --name default`; record != want {
		t.Errorf("keploy mock record's example under the proven model is %q, want %q", record, want)
	}
	if want := provenswitch.Env + `=0 keploy mock replay -c "go test -p 1 -json ./..." --name default`; replay != want {
		t.Errorf("keploy mock replay's example under the proven model is %q, want %q: this build refuses a replay under it", replay, want)
	}
	for _, line := range strings.Split(long, "\n") {
		if strings.Contains(line, "keploy mock replay") && !strings.HasPrefix(strings.TrimSpace(line), provenswitch.Env+"=0 ") {
			t.Errorf("keploy mock's help replays without %s=0, which this build refuses under the proven model: %q", provenswitch.Env, line)
		}
	}
	if got, want := MockExampleJudged("replay"), `keploy mock replay -c "go test -p 1 -json ./..." --name default`; got != want {
		t.Errorf("MockExampleJudged(replay) = %q, want %q", got, want)
	}

	t.Setenv(provenswitch.Env, "0")
	long, record, replay = build()
	if want := `keploy mock record -c "pytest" --name default`; record != want {
		t.Errorf("without the switch keploy mock record's example is %q, want it as it was, %q", record, want)
	}
	if want := `keploy mock replay -c "pytest" --name default --on-miss fail`; replay != want {
		t.Errorf("without the switch keploy mock replay's example is %q, want it as it was, %q", replay, want)
	}
	if !strings.Contains(long, `keploy mock replay -c "go test ./..." --name orders --on-miss record`) || !strings.Contains(long, "a language-agnostic VCR/WireMock") {
		t.Errorf("without the switch keploy mock's help is not as it was:\n%s", long)
	}
}
