package replayer_test

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// noiseEngines are the schema-noise settings a replay runs a command with:
// none, strict enforcement (keploy test and exact serving), detection
// (auto-replay learning request-body noise), and both.
var noiseEngines = []struct {
	name string
	eng  func() *mocknoise.Engine
}{
	{"none", func() *mocknoise.Engine { return nil }},
	{"off", func() *mocknoise.Engine { return replayer.NoiseEngineForTest(false, false) }},
	{"strict", replayer.StrictEngineForTest},
	{"detect", func() *mocknoise.Engine { return replayer.NoiseEngineForTest(true, false) }},
	{"detect+strict", func() *mocknoise.Engine { return replayer.NoiseEngineForTest(true, true) }},
}

// learnDb is the mock manager, noting the request-body noise each command
// learns on the mock it serves: updateMock hands it over on the delete of a
// per-test mock or the update of a reusable one.
type learnDb struct {
	*proxy.MockManager
	learned *strings.Builder
}

func (d learnDb) DeleteFilteredMock(m models.Mock) bool {
	d.note(&m)
	return d.MockManager.DeleteFilteredMock(m)
}

func (d learnDb) UpdateUnFilteredMock(old, m *models.Mock) bool {
	d.note(m)
	return d.MockManager.UpdateUnFilteredMock(old, m)
}

func (d learnDb) note(m *models.Mock) {
	if len(m.Spec.ReqBodyNoise) > 0 {
		fmt.Fprintf(d.learned, " %s:%v", m.Name, m.Spec.ReqBodyNoise)
	}
}

// matchBody is match with the live request body built before the scan (eager)
// or on first read, and also says what the command learned and how many live
// bodies it built.
func (s *diffSide) matchBody(ctx context.Context, req mysql.Request, eng *mocknoise.Engine, eager bool) (string, int) {
	var learned strings.Builder
	resp, ok, miss, built, err := replayer.MatchCommandBodiesForTest(ctx, zap.NewNop(), req, learnDb{s.mm, &learned}, s.dctx, eng, eager)
	return s.describe(resp, ok, miss, err) + "; learned" + learned.String(), built
}

// TestLazyBodyServesReportsAndLearnsWhatTheEagerBodyDoes replays each
// recording twice, the live request body built before the scan on one side
// and on its first read on the other, under every noise setting, and requires
// the two to serve, consume, report and learn the same at every command
// (replayCorpora).
func TestLazyBodyServesReportsAndLearnsWhatTheEagerBodyDoes(t *testing.T) {
	seeds := int64(30)
	sizes := []int{100, 1000, 10000}
	if testing.Short() || raceEnabled {
		seeds, sizes = 6, sizes[:2]
	}
	for _, ne := range noiseEngines {
		learned, rejected := replayCorpora(t, ne.name, ne.eng, seeds, sizes, [2]string{"eager body", "lazy body"},
			func(s *diffSide, ctx context.Context, req mysql.Request, eng *mocknoise.Engine, ref bool) string {
				got, _ := s.matchBody(ctx, req, eng, ref)
				return got
			})
		// Under strict, the drift detection learns from is a miss instead.
		detects, stricts := ne.name == "detect", strings.Contains(ne.name, "strict")
		if detects != (learned > 0) || stricts != (rejected > 0) {
			t.Errorf("noise %s: %d commands learned noise and strict rejected candidates on %d", ne.name, learned, rejected)
		}
	}
}

// replayCorpora replays, under the noise setting name (eng), each recording
// twice, on a reference side and on another, matching every command on each
// with match (ref true on the reference side), and fails at the first command
// the two serve, consume, report or learn differently; sides name the two in
// that message. The recordings are the windowed and between-test replays of
// TestFirstPassesServeWhatTheFullScanServes (seeds of them), and sets of
// sizes mocks with no window whose commands read recorded rows, rows never
// recorded and EXECUTEs whose parameters drifted. It returns how many
// commands learned noise and how many were misses strict enforcement made,
// so a row cannot pass by never reading the body.
func replayCorpora(t *testing.T, name string, eng func() *mocknoise.Engine, seeds int64, sizes []int, sides [2]string, match func(s *diffSide, ctx context.Context, req mysql.Request, eng *mocknoise.Engine, ref bool) string) (learned, rejected int) {
	t.Helper()
	compare := func(want, got string) bool {
		if !strings.HasSuffix(want, "; learned") {
			learned++
		}
		if strings.Contains(want, "strictRejected:") && !strings.Contains(want, "strictRejected:0") {
			rejected++
		}
		return want == got
	}
	for seed := int64(1); seed <= seeds; seed++ {
		r := rand.New(rand.NewSource(seed))
		windows := 4 + r.Intn(5)
		pool := diffPool(r, windows)
		ref, other := newDiffSide(pool), newDiffSide(pool)
		for _, s := range []*diffSide{ref, other} {
			s.stage(models.BaseTime, time.Now())
		}
		for w := 0; w < windows; w++ {
			start, end := diffWindow(w)
			for _, s := range []*diffSide{ref, other} {
				s.stage(start, end)
			}
			for c := 0; c < 5+r.Intn(25); c++ {
				between := r.Intn(6) == 0
				if between {
					ref.mm.SetCurrentTestWindow(time.Time{}, time.Time{})
					other.mm.SetCurrentTestWindow(time.Time{}, time.Time{})
				}
				req, desc := diffCommand(r)
				ctx := context.Background()
				if conn := []string{"", "c1", "c2"}[r.Intn(3)]; conn != "" {
					ctx = context.WithValue(ctx, models.ClientConnectionIDKey, conn)
				}
				want := match(ref, ctx, req, eng(), true)
				got := match(other, ctx, req, eng(), false)
				if !compare(want, got) {
					t.Fatalf("noise %s seed %d window %d command %d (%s), between tests=%v:\n %s: %s\n %s: %s", name, seed, w, c, desc, between, sides[0], want, sides[1], got)
				}
				if between {
					ref.mm.SetCurrentTestWindow(start, end)
					other.mm.SetCurrentTestWindow(start, end)
				}
			}
		}
		ref.mm.Close()
		other.mm.Close()
	}
	for _, n := range sizes {
		pool := keyedSet(n, models.LifetimePerTest)
		ref, other := newDiffSide(pool), newDiffSide(pool)
		ref.stageNoWindow()
		other.stageNoWindow()
		for i := 0; i < n; i += n / 100 {
			var reqs []mysql.Request
			switch i % 4 {
			case 0, 1:
				reqs = []mysql.Request{queryReq(keyedSetText(i)), queryReq(keyedSetText(i) + " LIMIT 1")}
			case 2:
				reqs = []mysql.Request{executeReq(7, fmt.Sprintf("k%d", i%500)), executeReq(7, "never-recorded")}
			default:
				reqs = []mysql.Request{executeReq(8, fmt.Sprintf("k%d", i%500)), executeReq(8, "never-recorded")}
			}
			for _, req := range reqs {
				want := match(ref, context.Background(), req, eng(), true)
				got := match(other, context.Background(), req, eng(), false)
				if !compare(want, got) {
					t.Fatalf("noise %s set of %d command for mock %d:\n %s: %s\n %s: %s", name, n, i, sides[0], want, sides[1], got)
				}
			}
		}
		ref.mm.Close()
		other.mm.Close()
	}
	return learned, rejected
}

// TestCommandBuildsItsBodyOnlyForStrictOrDetection: a command builds the
// canonical JSON body of its live request only when strict enforcement or
// detection reads it, and then once, however many candidates strict compares
// it with.
func TestCommandBuildsItsBodyOnlyForStrictOrDetection(t *testing.T) {
	s := newDiffSide(keyedSet(80, models.LifetimeSession))
	s.stageNoWindow()
	defer s.mm.Close()
	for _, ne := range noiseEngines {
		want := 1
		if ne.name == "none" || ne.name == "off" {
			want = 0
		}
		for _, tc := range []struct {
			name string
			req  mysql.Request
		}{
			{"COM_QUERY", queryReq(keyedSetText(77))},
			{"COM_STMT_EXECUTE of a statement with 20 recordings", executeReq(8, "k3")},
			{"COM_STMT_EXECUTE whose parameter drifted", executeReq(8, "never-recorded")},
		} {
			_, built := s.matchBody(context.Background(), tc.req, ne.eng(), false)
			if built != want {
				t.Errorf("noise %s, %s: built the live body %d times, want %d", ne.name, tc.name, built, want)
			}
		}
	}
}

// TestCommandAllocationsDoNotGrowWithItsParametersWithoutNoise: with no
// schema-noise engine, an EXECUTE of 41 parameters allocates what one of a
// single parameter does. The matcher compares parameters without allocating;
// what used to grow was the JSON body of the live request, built for every
// command although only strict enforcement and detection read it.
func TestCommandAllocationsDoNotGrowWithItsParametersWithoutNoise(t *testing.T) {
	allocs := func(params int) float64 {
		req := executeReq(8, "k0")
		exec := req.Message.(*mysql.StmtExecutePacket)
		exec.ParameterCount, exec.Parameters = params, nil
		for i := 0; i < params; i++ {
			exec.Parameters = append(exec.Parameters, mysql.Parameter{Type: 254, Value: fmt.Sprintf("k%d", i)})
		}
		rec := req
		rec.SetStoredQuery(diffQueryB)
		s := newDiffSide([]*models.Mock{diffMock("m-0", "c1", []mysql.Request{rec}, diffBase, models.LifetimeSession, "config")})
		s.mm.SetMocksWithWindow(nil, []*models.Mock{s.pool[0].DeepCopy()}, time.Time{}, time.Time{})
		s.mm.SetCurrentTestWindow(time.Time{}, time.Time{})
		defer s.mm.Close()
		return testing.AllocsPerRun(20, func() {
			if _, ok, _, err := replayer.MatchCommandForTest(context.Background(), zap.NewNop(), req, s.mm, s.dctx, nil, false); !ok || err != nil {
				t.Fatalf("not served: ok=%v err=%v", ok, err)
			}
		})
	}
	if one, many := allocs(1), allocs(41); many > one+2 {
		t.Errorf("an EXECUTE allocates %v times with 1 parameter, %v with 41", one, many)
	}
}
