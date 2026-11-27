package replay

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func kafkaProduceMock(name, topic, dest string, reqAt time.Time) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.KAFKA,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":           "mocks",
				"operation":      "Produce " + topic,
				"kafka_api_name": "Produce",
				"apiKey":         "Produce",
				"destAddr":       dest,
				"lifetime":       "perTest",
			},
			GenericRequests: []models.Payload{{
				Origin: models.FromClient,
				Message: []models.OutputBinary{{
					Type: "binary",
					Data: `{"header":{"apiKey":0,"apiVersion":10},"apiKeyName":"Produce","body":{"topics":[{"topic":"` + topic + `"}]}}`,
				}},
			}},
			ReqTimestampMock: reqAt,
			ResTimestampMock: reqAt.Add(2 * time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{IsFiltered: true, Lifetime: models.LifetimePerTest},
	}
}

func kafkaCoordMock(name, apiName string, apiKey int, topic, dest string, reqAt time.Time) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.KAFKA,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":           "mocks",
				"operation":      apiName + " " + topic,
				"kafka_api_name": apiName,
				"apiKey":         apiName,
				"destAddr":       dest,
				"lifetime":       "perTest",
			},
			GenericRequests: []models.Payload{{
				Origin: models.FromClient,
				Message: []models.OutputBinary{{
					Type: "binary",
					Data: `{"header":{"apiKey":` + string(rune('0'+apiKey)) + `,"apiVersion":5},"apiKeyName":"` + apiName + `","body":{"topics":[{"topic":"` + topic + `"}]}}`,
				}},
			}},
			ReqTimestampMock: reqAt,
			ResTimestampMock: reqAt.Add(2 * time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{IsFiltered: true, Lifetime: models.LifetimePerTest},
	}
}

// Default mode (KEPLOY_PROVEN=0) must surface an unmade recorded outgoing call
// (e.g. Kafka Produce in kafka-ecommerce auto-replay where dataMocksConsumed == 0)
// ONCE in the OUTGOING CALL DIFFS table with EXACTLY ONE WARN line across the
// entire run (no duplicate warnUnexercisedDependencies + run-end WARN), while
// keeping the test PASSED (exit 0) for both unmapped and mapped sets.
func TestDefaultModePrintsUnmadeRecordedOutgoingCallOnce(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		label := "unmapped-auto-replay"
		if mapped {
			label = "mapped-replay"
		}
		t.Run(label, func(t *testing.T) {
			provenOn(t, false)
			h := newPartialRunHarness(t, 1, 0)
			h.cases[0].Name = "post-api-v1-orders-3"
			pinWindows(h)
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1

			produce := kafkaProduceMock("mock-kafka-produce-3", "order-events", "kafka:9092", attrAt(1040))
			h.mocks.filtered = []*models.Mock{produce}
			if mapped {
				h.mappings.exists = true
				h.mappings.onDisk = map[string][]models.MockEntry{
					"post-api-v1-orders-3": {{Name: "mock-kafka-produce-3", Kind: string(models.KAFKA)}},
				}
			}
			// App returns matching HTTP 201, but NEVER produces to Kafka (0 consumed mocks).
			h.replayer.hookImpl = prHooks{}

			var consoleBuf bytes.Buffer
			h.replayer.console = &consoleBuf
			core, logs := observer.New(zapcore.InfoLevel)
			h.replayer.logger = zap.New(core)

			saved := utils.ErrCode
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = saved })

			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			_ = h.replayer.Start(ctx)

			if utils.ErrCode != 0 {
				t.Fatalf("default mode unmade outgoing call must not fail run, got exit code %d", utils.ErrCode)
			}
			out := consoleBuf.String()
			for _, want := range []string{
				"OUTGOING CALL DIFFS",
				"test-set-0 / post-api-v1-orders-3",
				"[Kafka] Produce order-events -> kafka:9092",
				"x1",
				unmadeCallFieldsText,
			} {
				if !strings.Contains(out, want) {
					t.Errorf("console output missing %q:\n%s", want, out)
				}
			}
			if n := strings.Count(out, "[Kafka] Produce order-events -> kafka:9092"); n != 1 {
				t.Errorf("unmade Kafka Produce printed %d times, want 1:\n%s", n, out)
			}
			var warnHits []string
			for _, e := range logs.All() {
				if e.Level < zapcore.WarnLevel {
					continue
				}
				m := e.Message
				if strings.Contains(m, "did not exercise a dependency") || strings.Contains(m, "not seen in their test window") || strings.Contains(m, "not seen against") {
					warnHits = append(warnHits, m)
				}
			}
			if len(warnHits) != 1 {
				t.Fatalf("expected exactly 1 Warn line reporting the unmade dependency (mapped=%v), got %d: %q", mapped, len(warnHits), warnHits)
			}
		})
	}
}

// Kafka consumer-group and broker coordination calls (e.g. OffsetFetch,
// OffsetCommit, Heartbeat, JoinGroup, SyncGroup, Metadata) recorded inside a
// test's window purely by timing must NEVER produce a false unmade-call row
// in either unmapped or mapped default-mode replays.
func TestKafkaCoordinationCallsNeverTriggerFalseUnmadeRow(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		label := "unmapped"
		if mapped {
			label = "mapped"
		}
		t.Run(label, func(t *testing.T) {
			provenOn(t, false)
			h := newPartialRunHarness(t, 1, 0)
			h.cases[0].Name = "post-api-v1-orders-3"
			pinWindows(h)
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1

			offsetFetch := kafkaCoordMock("mock-kafka-offset-fetch", "OffsetFetch", 9, "order-events", "172.19.0.2:9092", attrAt(1040))
			h.mocks.filtered = []*models.Mock{offsetFetch}
			if mapped {
				h.mappings.exists = true
				h.mappings.onDisk = map[string][]models.MockEntry{
					"post-api-v1-orders-3": {{Name: "mock-kafka-offset-fetch", Kind: string(models.KAFKA)}},
				}
			}
			h.replayer.hookImpl = prHooks{}

			var consoleBuf bytes.Buffer
			h.replayer.console = &consoleBuf
			core, logs := observer.New(zapcore.InfoLevel)
			h.replayer.logger = zap.New(core)

			saved := utils.ErrCode
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = saved })

			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			_ = h.replayer.Start(ctx)

			if out := consoleBuf.String(); strings.Contains(out, "OUTGOING CALL DIFFS") || strings.Contains(out, "OffsetFetch") {
				t.Fatalf("Kafka OffsetFetch must not produce an unmade OUTGOING CALL DIFFS row:\n%s", out)
			}
			for _, e := range logs.All() {
				if e.Level >= zapcore.WarnLevel && strings.Contains(e.Message, "not seen in their test window") {
					t.Fatalf("unexpected unmade-call Warn for Kafka OffsetFetch: %s", e.Message)
				}
			}
		})
	}

	t.Run("json GenericRequests fallback without metadata", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			data string
			want bool
		}{
			{name: "apiKeyName Produce", data: `{"apiKeyName":"Produce","header":{"apiKey":0}}`, want: true},
			{name: "header apiKey 0 only", data: `{"header":{"apiKey":0}}`, want: true},
			{name: "apiKeyName OffsetFetch", data: `{"apiKeyName":"OffsetFetch","header":{"apiKey":9}}`, want: false},
			{name: "header apiKey 9 only", data: `{"header":{"apiKey":9}}`, want: false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				mk := &models.Mock{
					Name: "kafka-json-" + tc.name,
					Kind: models.KAFKA,
					Spec: models.MockSpec{
						GenericRequests: []models.Payload{{Message: []models.OutputBinary{{Data: tc.data}}}},
					},
				}
				if got := isKafkaProduceMock(mk); got != tc.want {
					t.Fatalf("isKafkaProduceMock(%s) = %v, want %v", tc.name, got, tc.want)
				}
			})
		}
	})
}

// A passing unmapped test that DID make its recorded call prints no unmade row,
// and an async/linger-batched Produce flushed during a later test in the same
// set is pruned at end-of-set rather than falsely reported as unmade.
func TestMadeAndAsyncConsumedCallsPrintNoUnmadeRow(t *testing.T) {
	t.Run("unmapped made call prints nothing", func(t *testing.T) {
		provenOn(t, false)
		h := newPartialRunHarness(t, 1, 0)
		h.cases[0].Name = "post-api-v1-orders-3"
		pinWindows(h)
		h.replayer.config.Command = "./app"
		h.replayer.config.CommandType = string(utils.Native)
		h.replayer.config.Test.MaxFlakyChecks = 1

		produce := kafkaProduceMock("mock-kafka-produce-3", "order-events", "kafka:9092", attrAt(1040))
		h.mocks.filtered = []*models.Mock{produce}
		h.replayer.hookImpl = prHooks{consumedFor: func() []models.MockState {
			return []models.MockState{{
				Name:             "mock-kafka-produce-3",
				Kind:             models.KAFKA,
				Usage:            models.Deleted,
				Lifetime:         models.LifetimePerTest,
				ReqTimestampMock: models.FormatMockTimestamp(attrAt(1040)),
				ResTimestampMock: models.FormatMockTimestamp(attrAt(1042)),
			}}
		}}

		var consoleBuf bytes.Buffer
		h.replayer.console = &consoleBuf
		core, _ := observer.New(zapcore.InfoLevel)
		h.replayer.logger = zap.New(core)

		saved := utils.ErrCode
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = saved })

		ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
		defer cancel()
		_ = h.replayer.Start(ctx)

		if out := consoleBuf.String(); strings.Contains(out, unmadeCallFieldsText) || strings.Contains(out, "OUTGOING CALL DIFFS") {
			t.Fatalf("a made call must print no unmade row:\n%s", out)
		}
	})

	t.Run("async call consumed later in set is pruned", func(t *testing.T) {
		store := NewTestFailureStore()
		store.SetUnmadeOutgoingCalls("test-set-0", "test-1", []TestFailure{{
			TestSetID:     "test-set-0",
			TestID:        "test-1",
			FailureReason: unmadeCallReason,
			MismatchReport: &models.MockMismatchReport{
				Protocol:      "Kafka",
				ActualSummary: "Produce order-events",
				Destination:   "kafka:9092",
				ClosestMock:   "mock-kafka-async-1",
			},
		}})
		store.PruneConsumedUnmadeOutgoingCalls("test-set-0", map[string]models.MockState{
			"mock-kafka-async-1": {Name: "mock-kafka-async-1", Kind: models.KAFKA, Usage: models.Deleted},
		})
		if got := store.GetUnmadeOutgoingCalls(); len(got) != 0 {
			t.Fatalf("expected async-consumed mock to be pruned at end of set, got %+v", got)
		}
	})

	t.Run("async call consumed during a later test in set is pruned through Start", func(t *testing.T) {
		provenOn(t, false)
		h := newPartialRunHarness(t, 2, 0)
		h.cases[0].Name = "post-api-v1-orders-1"
		h.cases[1].Name = "get-api-v1-orders-2"
		pinWindows(h)
		h.replayer.config.Command = "./app"
		h.replayer.config.CommandType = string(utils.Native)
		h.replayer.config.Test.MaxFlakyChecks = 1

		asyncProduce := kafkaProduceMock("mock-kafka-async-1", "order-events", "kafka:9092", attrAt(1040))
		h.mocks.filtered = []*models.Mock{asyncProduce}
		h.mappings.exists = true
		h.mappings.onDisk = map[string][]models.MockEntry{
			"post-api-v1-orders-1": {{Name: "mock-kafka-async-1", Kind: string(models.KAFKA)}},
		}
		h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{
			"get-api-v1-orders-2": {{Name: "mock-kafka-async-1", Kind: models.KAFKA, Usage: models.Deleted}},
		})}

		var consoleBuf bytes.Buffer
		h.replayer.console = &consoleBuf
		core, logs := observer.New(zapcore.InfoLevel)
		h.replayer.logger = zap.New(core)

		saved := utils.ErrCode
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = saved })

		ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
		defer cancel()
		_ = h.replayer.Start(ctx)

		if out := consoleBuf.String(); strings.Contains(out, unmadeCallFieldsText) || strings.Contains(out, "OUTGOING CALL DIFFS") {
			t.Fatalf("async-consumed mock flushed during later test must be pruned through Start:\n%s", out)
		}
		for _, entry := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
			if strings.Contains(entry.Message, "unmade") || strings.Contains(entry.Message, "unexercised") {
				t.Fatalf("unexpected WARN after async mock was pruned: %q", entry.Message)
			}
		}
	})
}

// Edge-case coverage for unmade-call reporting:
//  1. A call that WAS made with a drifted body (ClosestMock matches) is NOT
//     also printed as "not seen in this test's window".
//  2. A retry cycle that recovers clears the unmade row so nothing is printed.
//  3. Strict mode (KEPLOY_PROVEN=1) still fails the test with non-zero exit.
func TestUnmadeRecordedOutgoingCallEdgeCases(t *testing.T) {
	t.Run("drifted call with ClosestMock is not duplicated as unmade", func(t *testing.T) {
		provenOn(t, false)
		h := newPartialRunHarness(t, 1, 0)
		h.cases[0].Name = "post-api-v1-orders-3"
		pinWindows(h)
		h.replayer.config.Command = "./app"
		h.replayer.config.CommandType = string(utils.Native)
		h.replayer.config.Test.MaxFlakyChecks = 1

		produce := kafkaProduceMock("mock-kafka-produce-3", "order-events", "kafka:9092", attrAt(1040))
		h.mocks.filtered = []*models.Mock{produce}
		h.mappings.exists = true
		h.mappings.onDisk = map[string][]models.MockEntry{
			"post-api-v1-orders-3": {{Name: "mock-kafka-produce-3", Kind: string(models.KAFKA)}},
		}
		recorded := false
		h.replayer.hookImpl = prHooks{
			consumedFor: func() []models.MockState {
				if !recorded {
					recorded = true
					h.replayer.mockMismatchFailures.AddUnmatchedCallForTest(
						"test-set-0",
						"post-api-v1-orders-3",
						models.UnmatchedCall{
							Protocol:      "Kafka",
							ActualSummary: "Produce order-events",
							Destination:   "kafka:9092",
							ClosestMock:   "mock-kafka-produce-3",
							MatchPhase:    models.MatchPhaseBody,
						},
					)
				}
				return nil
			},
		}

		var consoleBuf bytes.Buffer
		h.replayer.console = &consoleBuf
		core, _ := observer.New(zapcore.InfoLevel)
		h.replayer.logger = zap.New(core)

		saved := utils.ErrCode
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = saved })

		ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
		defer cancel()
		_ = h.replayer.Start(ctx)

		out := consoleBuf.String()
		if strings.Contains(out, unmadeCallFieldsText) {
			t.Fatalf("drifted call with ClosestMock must not also be printed as %q:\n%s", unmadeCallFieldsText, out)
		}
		if !strings.Contains(out, "[Kafka] Produce order-events -> kafka:9092") {
			t.Fatalf("expected drifted call row in OUTGOING CALL DIFFS table:\n%s", out)
		}
	})

	t.Run("retry recovery clears unmade row", func(t *testing.T) {
		store := NewTestFailureStore()
		store.SetUnmadeOutgoingCalls("test-set-0", "tc-1", []TestFailure{{
			TestSetID:     "test-set-0",
			TestID:        "tc-1",
			FailureReason: unmadeCallReason,
			MismatchReport: &models.MockMismatchReport{
				Protocol:      "Kafka",
				ActualSummary: "Produce order-events",
				Destination:   "kafka:9092",
				ClosestMock:   "mock-kafka-1",
			},
		}})
		store.SetUnmadeOutgoingCalls("test-set-0", "tc-1", nil)
		if got := store.GetUnmadeOutgoingCalls(); len(got) != 0 {
			t.Fatalf("expected 0 unmade rows after clean retry, got %+v", got)
		}
	})

	t.Run("strict mode KEPLOY_PROVEN=1 still fails on unmade Kafka Produce", func(t *testing.T) {
		provenOn(t, true)
		h := newPartialRunHarness(t, 1, 0)
		h.cases[0].Name = "post-api-v1-orders-3"
		pinWindows(h)
		h.replayer.config.Command = "./app"
		h.replayer.config.CommandType = string(utils.Native)
		h.replayer.config.Test.MaxFlakyChecks = 1
		produce := kafkaProduceMock("mock-kafka-produce-3", "order-events", "kafka:9092", attrAt(1040))
		h.mocks.filtered = []*models.Mock{produce}
		h.mappings.exists = true
		h.mappings.onDisk = map[string][]models.MockEntry{
			"post-api-v1-orders-3": {{Name: "mock-kafka-produce-3", Kind: string(models.KAFKA)}},
		}
		h.replayer.hookImpl = prHooks{}

		saved := utils.ErrCode
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = saved })

		ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
		defer cancel()
		_ = h.replayer.Start(ctx)

		if utils.ErrCode == 0 {
			t.Fatal("strict mode (KEPLOY_PROVEN=1) must fail when recorded Kafka Produce is not made")
		}
	})
}

// A pre-test message delivery consumed before test 1 must never be baked into
// mappings.yaml's STARTUP section, while genuine non-delivery startup mocks
// remain in STARTUP.
func TestWithoutMessageDeliveriesExcludesDeliveriesFromStartupMapping(t *testing.T) {
	lookup := map[string]mockDisplayInfo{
		"mock-handshake":    {protocol: "Mongo", messageDelivery: false},
		"mock-sqs-delivery": {protocol: "HTTP", messageDelivery: true},
	}
	in := []models.MockState{
		{Name: "mock-handshake", Kind: models.Mongo, Usage: models.Updated},
		{Name: "mock-sqs-delivery", Kind: models.HTTP, Usage: models.Deleted, CarryOver: true},
	}
	got := withoutMessageDeliveries(in, lookup)
	if len(got) != 1 || got[0].Name != "mock-handshake" {
		t.Fatalf("withoutMessageDeliveries = %+v, want only [mock-handshake]", got)
	}
}
