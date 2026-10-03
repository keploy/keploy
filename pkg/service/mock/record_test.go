package mock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// failsRecord is failsGroup for the stages only a recording has: a dying
// agent fails a goroutine in the run's errgroup, and the stage in flight
// fails with it.
type failsRecord struct {
	*failsGroup
}

func (f failsRecord) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	if f.stage == "capture" {
		killGroup(ctx)
		return nil, errors.New("could not start capturing outgoing calls")
	}
	return f.composeInstr.GetOutgoing(ctx, opts)
}

func (f failsRecord) MakeAgentReadyForDockerCompose(ctx context.Context) error {
	if f.stage == "release" {
		killGroup(ctx)
		return errors.New("could not release the app")
	}
	return f.composeInstr.MakeAgentReadyForDockerCompose(ctx)
}

// recordWith runs one recording against instr and returns its error.
func recordWith(t *testing.T, ctx context.Context, instr Instrumentation, base *composeInstr, cmdType utils.CmdType) error {
	t.Helper()
	cfg := instrConfig(base, cmdType, "go test ./...")
	cfg.Path = t.TempDir()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	return New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg).Record(ctx)
}

// keploy's own failure is not the user pressing Ctrl+C. The errgroup cancels
// its context whenever anything in it fails -- the agent process exiting is
// one of those -- so testing that context turned a native agent that could
// not start into `return nil`: exit 0, the test command never run, and the CI
// step the VS Code extension generates went green. Every stage guards itself,
// so every stage is failed.
func TestARecordingKeployFailedIsNotAnInterrupt(t *testing.T) {
	for _, tc := range []struct {
		stage    string
		lifetime agentLifetime
		cmdType  utils.CmdType
	}{
		{"setup", agentUpFromSetup, utils.Native},
		{"compose", agentNeverUp, utils.DockerCompose},
		{"capture", agentUpFromSetup, utils.Native},
		{"release", agentUpAfterRun, utils.DockerCompose},
		{"run", agentUpFromSetup, utils.Native},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			shortAgentBudget(t)
			base := newInstr(t, tc.lifetime, tc.cmdType == utils.DockerCompose, models.AppError{AppErrorType: models.ErrAppStopped})
			instr := failsRecord{&failsGroup{composeInstr: base, stage: tc.stage}}
			if err := recordWith(t, context.Background(), instr, base, tc.cmdType); err == nil {
				t.Fatalf("a recording that failed at %s reported success", tc.stage)
			}
		})
	}
}

// Why it failed has to survive to the exit: the agent's reason is what
// decides the exit code (utils.ExitCodeFor) and the message a tool shows, and
// the recording used to replace it with its own bare stop reason.
func TestARecordingThatCouldNotStartSaysWhy(t *testing.T) {
	why := fmt.Errorf("%w: the keploy agent could not start (exit status 3)", utils.ErrPrivilegeRequired)
	base := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	err := recordWith(t, context.Background(), &runWrites{composeInstr: base, setupErr: why}, base, utils.Native)
	if !errors.Is(err, utils.ErrPrivilegeRequired) {
		t.Fatalf("Record returned %v; the agent's reason is gone", err)
	}
}

// The user's Ctrl+C is still not a failure, wherever it lands.
func TestAnInterruptedRecordingIsNotAFailure(t *testing.T) {
	for _, stage := range []string{"setup", "run"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			base := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrCtxCanceled})
			var instr Instrumentation = &runWrites{composeInstr: base, setupErr: context.Canceled}
			if stage == "run" {
				instr = &runWrites{composeInstr: base, onRun: cancel}
			} else {
				cancel()
			}
			defer cancel()
			if err := recordWith(t, ctx, instr, base, utils.Native); err != nil {
				t.Fatalf("an interrupted recording failed: %v", err)
			}
		})
	}
}

// sendsInstrumentation is a runner whose run emits mocks, the way the agent's
// stream delivers a recording's dependency calls.
type sendsInstrumentation struct {
	Instrumentation // embedded: nil, so any unexpected call panics loudly

	out   chan *models.Mock
	mocks []*models.Mock
}

func (s *sendsInstrumentation) Setup(context.Context, string, models.SetupOptions) error { return nil }
func (s *sendsInstrumentation) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	return s.out, nil
}
func (s *sendsInstrumentation) Run(context.Context, models.RunOptions) models.AppError {
	for _, m := range s.mocks {
		s.out <- m
	}
	close(s.out)
	return models.AppError{}
}
func (s *sendsInstrumentation) NotifyGracefulShutdown(context.Context) error { return nil }

// countingMockDB wraps a MockYaml the way k8s-proxy's does: it embeds it and
// overrides InsertMock only.
type countingMockDB struct {
	*mockdb.MockYaml
	inserted atomic.Int64
}

func (c *countingMockDB) InsertMock(ctx context.Context, m *models.Mock, set string) error {
	c.inserted.Add(1)
	return c.MockYaml.InsertMock(ctx, m, set)
}

// encodedHooks keeps a copy of every document AfterMockInsert is handed.
type encodedHooks struct {
	record.BaseRecordHooks
	mu      sync.Mutex
	docs    [][]byte
	formats []string
}

func (h *encodedHooks) AfterMockInsert(_ context.Context, info *record.MockContext) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.docs = append(h.docs, append([]byte(nil), info.Encoded...))
	h.formats = append(h.formats, info.EncodedFormat)
	return nil
}

// `keploy mock record` hands the AfterMockInsert hooks the document the MockDB
// wrote for each mock, as the test recorder does: byte for byte what is in
// mocks.yaml, through a MockDB wrapped the way k8s-proxy wraps one.
func TestRecord_AfterMockInsertGetsTheDocumentInMocksYAML(t *testing.T) {
	ts := time.Date(2026, 9, 30, 8, 42, 54, 0, time.UTC)
	var mocks []*models.Mock
	for i, body := range []string{`{"ok":true}`, "\tleading tab\nx", "line one\nline two\n"} {
		mocks = append(mocks, &models.Mock{
			Version: models.GetVersion(), Kind: models.HTTP, Name: fmt.Sprintf("temp-%d", i),
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "config"},
				HTTPReq:          &models.HTTPReq{Method: "GET", ProtoMajor: 1, ProtoMinor: 1, URL: "http://orders.shop/x", Timestamp: ts},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: body, Timestamp: ts},
				ReqTimestampMock: ts, ResTimestampMock: ts,
			},
		})
	}
	inst := &sendsInstrumentation{out: make(chan *models.Mock, len(mocks)), mocks: mocks}
	dir := t.TempDir()
	db := &countingMockDB{MockYaml: mockdb.New(zap.NewNop(), dir, "mocks")}
	hooks := &encodedHooks{}
	cfg := &config.Config{}
	cfg.Mock.Name = "handed"
	cfg.Path = dir
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

	if err := New(zap.NewNop(), inst, db, nil, FileStore{}, hooks, cfg).Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := db.inserted.Load(); got != int64(len(mocks)) {
		t.Fatalf("the wrapper's InsertMock ran for %d of %d mocks: Record went around it", got, len(mocks))
	}
	file, err := os.ReadFile(filepath.Join(dir, "handed", "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if len(hooks.docs) != len(mocks) {
		t.Fatalf("AfterMockInsert ran for %d of %d mocks", len(hooks.docs), len(mocks))
	}
	for i, doc := range hooks.docs {
		if len(doc) == 0 || hooks.formats[i] != "yaml" {
			t.Fatalf("AfterMockInsert got %d bytes as %q for mock %d, want its YAML document", len(doc), hooks.formats[i], i)
		}
	}
	want := append([]byte(utils.GetVersionAsComment()), bytes.Join(hooks.docs, []byte("---\n"))...)
	if !bytes.Equal(want, file) {
		t.Fatalf("the documents AfterMockInsert got are not what is in mocks.yaml\ngot:\n%s\nmocks.yaml:\n%s", want, file)
	}
}
