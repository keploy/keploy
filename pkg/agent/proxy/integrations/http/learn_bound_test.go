package http

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// parkedBig is the i-th of a set's recordings, at: a GET whose answer, of
// size bytes of words no other recording holds, the agent parked on disk,
// made only when read back (sample runs at each read-back), as keploy's
// DiskMocks reads one back (its hydrator): no recording holds it in memory.
func parkedBig(i int, at time.Duration, size int, sample func()) *models.Mock {
	m := learnRecorded(fmt.Sprintf("big-%04d", i), at, "GET", "/v1/blobs/"+strconv.Itoa(i), "", nil, "")
	m.Spec.HTTPResp = nil
	m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		sample()
		var b strings.Builder
		b.Grow(size + 64)
		b.WriteString(`{"blob":"`)
		var word []byte
		for j := 0; b.Len() < size; j++ {
			word = strconv.AppendInt(append(word[:0], 'w'), int64(i), 10)
			word = strconv.AppendInt(append(word, 'x'), int64(j), 10)
			b.Write(append(word, ' '))
		}
		b.WriteString(`"}`)
		return &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: b.String()}, nil, nil
	})
	return m
}

// streamedHTTPDisk is learnDb over an on-disk store of n recordings, each a
// parkedBig, then last, made one at a time as keploy's DiskMocks decodes
// them for RangeRecordedBefore: the store itself holds none of them.
type streamedHTTPDisk struct {
	*learnDb
	n, size int
	last    *models.Mock
	sample  func()
}

func (d *streamedHTTPDisk) RangeRecordedBefore(t time.Time, visit func(*models.Mock) bool) error {
	for i := 0; i < d.n; i++ {
		d.sample()
		if m := parkedBig(i, time.Duration(i)*time.Microsecond, d.size, d.sample); !m.Spec.ReqTimestampMock.After(t) && !visit(m) {
			return nil
		}
	}
	if !d.last.Spec.ReqTimestampMock.After(t) {
		visit(d.last)
	}
	return nil
}

// heapPeak samples the heap above where it stood at start, every 16th call
// (each after a collection), into the peak it returns.
type heapPeak struct {
	start, peak uint64
	calls       int
}

// reset starts h over from the heap as it stands now.
func (h *heapPeak) reset() {
	runtime.GC()
	var st runtime.MemStats
	runtime.ReadMemStats(&st)
	h.start, h.peak, h.calls = st.HeapAlloc, 0, 0
}

func (h *heapPeak) sample() {
	h.calls++
	if h.calls%16 != 0 {
		return
	}
	h.now()
}

func (h *heapPeak) now() {
	runtime.GC()
	var st runtime.MemStats
	runtime.ReadMemStats(&st)
	if st.HeapAlloc > h.start && st.HeapAlloc-h.start > h.peak {
		h.peak = st.HeapAlloc - h.start
	}
}

// largeStreamMiB is how many recordings of 1 MiB a bound row reads: 256 by
// default, KEPLOY_LARGE_STREAM_MIB where it is set (CI's step for the bound
// row's higher limits, or the 1 GB+ run on a test box). set says whether it
// is.
func largeStreamMiB(t *testing.T) (mib int, set bool) {
	t.Helper()
	v := os.Getenv("KEPLOY_LARGE_STREAM_MIB")
	if v == "" {
		return 256, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("KEPLOY_LARGE_STREAM_MIB=%q: want a positive number of MiB", v)
	}
	return n, true
}

// boundLimits are the agent memory limits the bound row runs at, each with
// the set budget it gives (learn's setBudget: a quarter of the limit), and
// boundOverhead is what the row lets the heap pass that budget by: the
// recording read in flight (1 MiB of text, its decoded forms and words) and
// the tiers' bookkeeping, measured at 3, 4 and 6 MiB at these limits. A
// large limit's legs (large) run only with KEPLOY_LARGE_STREAM_MIB set:
// each takes 40-50 s on 4 CPUs, and the least limit's already streams four
// times its budget. CI runs them in a step of their own
// (.github/workflows/go-test.yaml).
var boundLimits = []struct {
	limit, budget int64
	large         bool
}{
	{256 << 20, 64 << 20, false},
	{512 << 20, 128 << 20, true},
	{1 << 30, 256 << 20, true},
}

const boundOverhead = 16 << 20

// The HTTP learn step reads the recordings before a call within the set's
// budget of memory, however many there are and however large their
// answers: a recording's answer the agent parked on disk is read back only
// where a rule reads it, and what is built of it is kept in the set's store
// (learn.DriftCache.LazyRawOf) within one budget, a quarter of the agent's
// memory limit (learn's setBudget), never every one at once. The row runs
// at three limits, the Go runtime's (GOMEMLIMIT, as the agent's memory
// guard sets it; a box's or a CI runner's cgroup sets a higher one), and
// the peak heap follows the budget each gives, within boundOverhead. Both
// ways a pool call reaches the recordings: the store's on disk, handed
// over one at a time (integrations.RecordedBeforeReader), and those it
// stages in memory with their answers parked (integrations.StagedReader).
// Either set (256 MiB of answers by default, four times the least budget;
// KEPLOY_LARGE_STREAM_MIB=1100 for the 1 GB+ run on a test box) is learned
// within the bound with the verdict a small set gives: refused, the token
// answered earlier, by mock-0. The 512 MiB and 1 GiB limits run only with
// KEPLOY_LARGE_STREAM_MIB set (boundLimits).
func TestLearnMatchReadsTheRecordingsBeforeACallWithinTheSetsBudget(t *testing.T) {
	if raceOn {
		t.Skip("a heap bound is the plain run's to check: the race detector's own allocations are no part of the learn step's")
	}
	const size = 1 << 20
	mib, set := largeStreamMiB(t)
	for _, l := range boundLimits {
		for _, way := range []string{"on disk", "staged"} {
			t.Run(fmt.Sprintf("%d MiB limit %s", l.limit>>20, way), func(t *testing.T) {
				if l.large && !set {
					t.Skip("a large limit's leg runs with KEPLOY_LARGE_STREAM_MIB set (CI's go-test step for it, or a test box)")
				}
				prev := debug.SetMemoryLimit(l.limit)
				defer debug.SetMemoryLimit(prev)
				bound := l.budget + boundOverhead
				h, n, opts := learnOn(t)
				opts.LearnPool = true
				// The set starts (and reads its budget) here, under the limit.
				ctx := learn.WithTracker(context.Background(), learn.NewTracker())
				login := learnRecorded("mock-0", time.Second, "POST", "/v1/login", `{"user":"u-7"}`, nil, `{"token":"tok-0c41b2"}`)
				refresh := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/sessions/refresh", `{"token":"tok-0c41b2"}`, nil, `{}`)
				refresh.TestModeInfo.Lifetime = models.LifetimeSession
				base := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{refresh}}}
				hp := &heapPeak{}
				var db integrations.MockMemDb = base
				switch way {
				case "on disk":
					db = &streamedHTTPDisk{learnDb: base, n: mib, size: size, last: login, sample: hp.sample}
				case "staged":
					// The staged recordings are in memory as the store keeps
					// them, their answers parked: the heap is measured from
					// after they are made.
					for i := 0; i < mib; i++ {
						base.staged = append(base.staged, parkedBig(i, time.Duration(i)*time.Microsecond, size, hp.sample))
					}
					base.staged = append(base.staged, login)
				}
				hp.reset()
				served, miss, err := h.learnMatch(ctx, live("POST", "/v1/sessions/refresh", `{"token":"tok-9d8e7f"}`, nil), db, n, opts)
				hp.now()
				t.Logf("%s at a %d MiB limit: %d recordings of %d bytes read; peak heap above the start %d MiB (budget %d MiB, bound %d MiB)", way, l.limit>>20, mib, size, hp.peak>>20, l.budget>>20, bound>>20)
				if err != nil || served != nil || miss == nil || !strings.Contains(miss.res.Why, "holds earlier (in mock-0)") {
					t.Fatalf("served %v miss %+v err %v; want refused, the token answered earlier by mock-0", served, miss, err)
				}
				if hp.peak > uint64(bound) {
					t.Fatalf("the learn step's heap grew %d MiB while reading %d MiB of recordings before the call, past the set's budget of %d MiB at a %d MiB limit and the %d MiB it may pass it by: it holds them at once, not within the budget", hp.peak>>20, mib, l.budget>>20, l.limit>>20, boundOverhead>>20)
				}
			})
		}
	}
}
