package record

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// TestSaveHold_UnnamedPostStopMappingsAfterQuietFrameDrainDoNotHoldSaveOpen
// verifies that when a recording's app keeps serving after stop and no
// post-stop test case reaches the incoming drain before framesDrained closes,
// unnamed (empty or whitespace-only) post-stop mappings do not match
// recordedTests, do not restart mappingIdleGrace, and do not stream into
// consumeMappingsFor as unmapped entries.
func TestSaveHold_UnnamedPostStopMappingsAfterQuietFrameDrainDoNotHoldSaveOpen(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		label string
		name  string
	}{
		{label: "empty", name: ""},
		{label: "whitespace", name: "   "},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			core, logs := observer.New(zap.DebugLevel)
			logger := zap.New(core)

			f := &fakeInstr{
				mappings: make(chan models.TestMockMapping),
				incoming: make(chan *models.TestCase),
				outgoing: make(chan *models.Mock),
			}
			cfg := &config.Config{}
			cfg.CommandType = string(utils.DockerCompose)
			r := &Recorder{
				logger:          logger,
				instrumentation: f,
				config:          cfg,
				mappingDb:       &recMappingDB{},
				frameQuiet:      20 * time.Millisecond,
			}

			g, gctx := errgroup.WithContext(context.Background())
			ctx, cancel := context.WithCancel(gctx)
			defer cancel()
			ctx = context.WithValue(ctx, models.ErrGroupKey, g)

			frames, err := r.GetTestAndMockChans(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer frames.Abandon()

			incomingDone := make(chan struct{})
			preTCReceived := make(chan struct{}, 1)
			go func() {
				defer close(incomingDone)
				for range frames.Incoming {
					select {
					case preTCReceived <- struct{}{}:
					default:
					}
				}
			}()

			outgoingDone := make(chan struct{})
			go func() {
				defer close(outgoingDone)
				for range frames.Outgoing {
				}
			}()

			waits := newMappingWaits()
			var correlationMap, asyncMockIDs, shortPool sync.Map
			var dropped droppedMockSet
			correlationMap.Store("pre-mock-1", models.MockEntry{Name: "mock-0", Kind: string(models.HTTP)})

			mappingsDone := make(chan struct{})
			preMapReceived := make(chan struct{}, 1)
			go func() {
				defer close(mappingsDone)
				_ = r.consumeMappingsFor(context.Background(), "set-0", frames.Mappings,
					&correlationMap, &asyncMockIDs, &dropped, waits,
					func(string, []time.Time) {}, &shortPool,
					func(string, []models.MockEntry) {
						select {
						case preMapReceived <- struct{}{}:
						default:
						}
					}, nil, nil)
			}()

			// 1. Pre-stop unnamed test case and its mapping.
			before := time.Now().Add(-time.Second)
			f.incoming <- &models.TestCase{Name: tc.name, HTTPReq: models.HTTPReq{Timestamp: before}}
			<-preTCReceived
			f.mappings <- models.TestMockMapping{TestName: tc.name, MockIDs: []string{"pre-mock-1"}}
			<-preMapReceived

			// 2. Stop the recording; send NO post-stop test cases during the frame drain.
			cancel()
			stopNotice := r.reportDrainProgress(250 * time.Millisecond)
			defer stopNotice()

			<-incomingDone
			<-outgoingDone
			waits.end()

			// 3. App continues serving after framesDrained: send unnamed mappings
			// faster than mappingIdleGrace. They must be skipped and the mapping
			// drain must end at mappingIdleGrace, not be held open past 2*mappingIdleGrace.
			framesDrainedAt := time.Now()
			maxWait := 2 * mappingIdleGrace
			tick := time.NewTicker(mappingIdleGrace / 6)
			defer tick.Stop()

			postMappingsSent := 0
			ended := false
			for !ended && time.Since(framesDrainedAt) < maxWait {
				select {
				case <-mappingsDone:
					ended = true
				case <-tick.C:
					postMappingsSent++
					m := models.TestMockMapping{
						TestName: tc.name,
						MockIDs:  []string{fmt.Sprintf("post-mock-%d", postMappingsSent)},
					}
					select {
					case <-mappingsDone:
						ended = true
					case f.mappings <- m:
					case <-time.After(time.Second):
						t.Fatal("mapping channel wedged while drain was still open")
					}
				}
			}

			unmappedLogs := logs.FilterMessage(unmappedEntryLog).All()
			unmappedTotal, _ := r.unmapped.take()
			streamed := r.mappingsStreamed.Load()
			if !ended {
				stillSaving := logs.FilterMessageSnippet("Still saving").All()
				var lastSaved uint64
				if len(stillSaving) > 0 {
					if v, ok := stillSaving[len(stillSaving)-1].ContextMap()["savedSinceTheStop"].(uint64); ok {
						lastSaved = v
					}
				}
				t.Fatalf("mapping drain held open for %v (>= 2*mappingIdleGrace) after framesDrained: "+
					"postMappingsSent=%d, StillSavingLines=%d (last savedSinceTheStop=%d), streamed=%d, unmapped=%d (logs=%d)",
					time.Since(framesDrainedAt).Round(time.Millisecond),
					postMappingsSent, len(stillSaving), lastSaved, streamed, unmappedTotal, len(unmappedLogs))
			}
			if unmappedTotal != 0 || len(unmappedLogs) != 0 {
				t.Fatalf("post-stop unnamed mappings produced unmapped=%d (logs=%d), want 0", unmappedTotal, len(unmappedLogs))
			}
			if streamed != 1 {
				t.Fatalf("post-stop unnamed mappings streamed into consumer: mappingsStreamed=%d, want 1 (pre-stop only)", streamed)
			}
		})
	}
}

// TestSaveHold_PostStopUnnamedTestCaseDoesNotDropPreStopUnnamedMapping
// verifies that a post-stop unnamed (empty or whitespace-only) test case
// rejected during the frame drain does not key postStopTests and therefore
// does not drop legitimate pre-stop unnamed mappings still in flight before
// framesDrained closes.
func TestSaveHold_PostStopUnnamedTestCaseDoesNotDropPreStopUnnamedMapping(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		label string
		name  string
	}{
		{label: "empty", name: ""},
		{label: "whitespace", name: "   "},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			f := &fakeInstr{
				mappings: make(chan models.TestMockMapping),
				incoming: make(chan *models.TestCase),
				outgoing: make(chan *models.Mock),
			}
			cfg := &config.Config{}
			cfg.CommandType = string(utils.DockerCompose)
			r := &Recorder{
				logger:          zap.NewNop(),
				instrumentation: f,
				config:          cfg,
				frameQuiet:      500 * time.Millisecond,
			}

			g, gctx := errgroup.WithContext(context.Background())
			ctx, cancel := context.WithCancel(gctx)
			defer cancel()
			ctx = context.WithValue(ctx, models.ErrGroupKey, g)

			frames, err := r.GetTestAndMockChans(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer frames.Abandon()

			preTCReceived := make(chan struct{}, 1)
			go func() {
				for range frames.Incoming {
					select {
					case preTCReceived <- struct{}{}:
					default:
					}
				}
			}()
			go func() {
				for range frames.Outgoing {
				}
			}()

			var gotMu sync.Mutex
			var gotMappings []models.TestMockMapping
			mappingsDone := make(chan struct{})
			go func() {
				defer close(mappingsDone)
				for m := range frames.Mappings {
					gotMu.Lock()
					gotMappings = append(gotMappings, m)
					gotMu.Unlock()
				}
			}()

			before := time.Now().Add(-time.Second)
			f.incoming <- &models.TestCase{Name: tc.name, HTTPReq: models.HTTPReq{Timestamp: before}}
			<-preTCReceived

			cancel()

			// A post-stop unnamed test case reaches the incoming drain before framesDrained.
			// Send a second one to guarantee the first was processed by forwardUntilDrained.
			f.incoming <- &models.TestCase{Name: tc.name, HTTPReq: models.HTTPReq{Timestamp: time.Now().Add(time.Second)}}
			f.incoming <- &models.TestCase{Name: tc.name, HTTPReq: models.HTTPReq{Timestamp: time.Now().Add(2 * time.Second)}}

			// Legitimate pre-stop unnamed mapping arrives before framesDrained closes.
			f.mappings <- models.TestMockMapping{TestName: tc.name, MockIDs: []string{"pre-stop-mock"}}

			select {
			case <-mappingsDone:
			case <-time.After(mappingIdleGrace + 5*time.Second):
				t.Fatal("mapping drain did not finish")
			}

			gotMu.Lock()
			defer gotMu.Unlock()
			if len(gotMappings) != 1 || len(gotMappings[0].MockIDs) != 1 || gotMappings[0].MockIDs[0] != "pre-stop-mock" {
				t.Fatalf("pre-stop unnamed mapping (%s) was dropped: got %+v", tc.label, gotMappings)
			}
		})
	}
}

// TestSaveHold_NamedTestCasesStillTrackedAcrossStopDrain pins that guarding
// blank names leaves named test-case correlation intact in both directions:
//   - a pre-stop named test's mapping arriving after framesDrained is kept;
//   - a post-stop named test's mapping arriving before framesDrained is dropped.
func TestSaveHold_NamedTestCasesStillTrackedAcrossStopDrain(t *testing.T) {
	t.Parallel()

	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	cfg := &config.Config{}
	cfg.CommandType = string(utils.DockerCompose)
	r := &Recorder{
		logger:          zap.NewNop(),
		instrumentation: f,
		config:          cfg,
		frameQuiet:      200 * time.Millisecond,
	}

	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	defer cancel()
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)

	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Abandon()

	preTCReceived := make(chan struct{}, 1)
	incomingDone := make(chan struct{})
	go func() {
		defer close(incomingDone)
		for range frames.Incoming {
			select {
			case preTCReceived <- struct{}{}:
			default:
			}
		}
	}()
	outgoingDone := make(chan struct{})
	go func() {
		defer close(outgoingDone)
		for range frames.Outgoing {
		}
	}()

	var gotMu sync.Mutex
	var gotMappings []string
	mappingsDone := make(chan struct{})
	go func() {
		defer close(mappingsDone)
		for m := range frames.Mappings {
			gotMu.Lock()
			gotMappings = append(gotMappings, m.TestName)
			gotMu.Unlock()
		}
	}()

	before := time.Now().Add(-time.Second)
	f.incoming <- tcAt("test-pre", before)
	<-preTCReceived

	cancel()

	// Post-stop named test case arrives before framesDrained closes.
	f.incoming <- tcAt("test-post", time.Now().Add(time.Second))
	f.incoming <- tcAt("test-post-2", time.Now().Add(2*time.Second))

	// Mapping for the post-stop named test arrives before framesDrained: must be dropped.
	f.mappings <- models.TestMockMapping{TestName: "test-post", MockIDs: []string{"m-post"}}

	<-incomingDone
	<-outgoingDone

	// Mapping for the pre-stop named test arrives after framesDrained: must be kept.
	f.mappings <- models.TestMockMapping{TestName: "test-pre", MockIDs: []string{"m-pre"}}

	select {
	case <-mappingsDone:
	case <-time.After(mappingIdleGrace + 5*time.Second):
		t.Fatal("mapping drain did not finish")
	}

	gotMu.Lock()
	defer gotMu.Unlock()
	if len(gotMappings) != 1 || gotMappings[0] != "test-pre" {
		t.Fatalf("got mappings %v, want [test-pre]", gotMappings)
	}
}
