package http

import (
	"context"
	"fmt"
	"os"
	"runtime"
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
		for j := 0; b.Len() < size; j++ {
			fmt.Fprintf(&b, "w%dx%d ", i, j)
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
// default, KEPLOY_LARGE_STREAM_MIB for the 1 GB+ run on a test box.
func largeStreamMiB(t *testing.T) int {
	t.Helper()
	v := os.Getenv("KEPLOY_LARGE_STREAM_MIB")
	if v == "" {
		return 256
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("KEPLOY_LARGE_STREAM_MIB=%q: want a positive number of MiB", v)
	}
	return n
}

// The HTTP learn step reads the recordings before a call within a fixed
// bound of memory, however many there are and however large their answers:
// a recording's answer the agent parked on disk is read back only where a
// rule reads it, and what is built of it is kept in the set's bounded store
// (learn.DriftCache.LazyRawOf), never every one at once. Both ways a pool
// call reaches them: the store's recordings on disk, handed over one at a
// time (integrations.RecordedBeforeReader), and the recordings it stages in
// memory with their answers parked (integrations.StagedReader). Either set
// (256 MiB of answers by default; KEPLOY_LARGE_STREAM_MIB=1100 for the
// 1 GB+ run on a test box) is learned within the bound with the verdict a
// small set gives: refused, the token answered earlier, by mock-0.
func TestLearnMatchReadsTheRecordingsBeforeACallWithinAFixedBound(t *testing.T) {
	if raceOn {
		t.Skip("a heap bound is the plain run's to check: the race detector's own allocations are no part of the learn step's")
	}
	const size = 1 << 20
	const bound = 128 << 20
	mib := largeStreamMiB(t)
	for _, way := range []string{"on disk", "staged"} {
		t.Run(way, func(t *testing.T) {
			h, n, opts := learnOn(t)
			opts.LearnPool = true
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
			t.Logf("%s: %d recordings of %d bytes read; peak heap above the start %d MiB (bound %d MiB)", way, mib, size, hp.peak>>20, bound>>20)
			if err != nil || served != nil || miss == nil || !strings.Contains(miss.res.Why, "holds earlier (in mock-0)") {
				t.Fatalf("served %v miss %+v err %v; want refused, the token answered earlier by mock-0", served, miss, err)
			}
			if hp.peak > bound {
				t.Fatalf("the learn step's heap grew %d MiB while reading %d MiB of recordings before the call: it holds them at once, not within the %d MiB bound", hp.peak>>20, mib, bound>>20)
			}
		})
	}
}
