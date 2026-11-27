package http

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

type deliveryTrackDb struct {
	mockMemDb
	markedUsed bool
}

func (d *deliveryTrackDb) MarkMockAsUsed(_ models.Mock) bool {
	d.markedUsed = true
	return true
}

// TestUpdateMock_SQSDeliveryNeverTakesKeepingCursorPath proves that a stated
// per-test SQS ReceiveMessage carrying Messages with ConsumeCursorSaturate
// ALWAYS consumes via DeleteFilteredMock (never MarkMockAsUsed), both when
// consumed before any test opens (!ownedBy) and during a stateful exact-body
// match inside a test.
func TestUpdateMock_SQSDeliveryNeverTakesKeepingCursorPath(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
	}{
		{
			name: "before_first_test_unowned",
			ctx: context.WithValue(context.Background(), ownedKey{}, func(*models.Mock) bool {
				return false
			}),
		},
		{
			name: "in_test_stateful_exact_body",
			ctx: context.WithValue(
				context.WithValue(context.Background(), statefulMocksKey{}, true),
				keepForRepeatsKey{}, true,
			),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHTTP()
			db := &deliveryTrackDb{mockMemDb: mockMemDb{deleteFilteredReturn: true}}

			sqsDelivery := &models.Mock{
				Name: "mock-2-sqs",
				Kind: models.HTTP,
				Spec: models.MockSpec{
					Metadata: map[string]string{
						"type":     "HTTP_CLIENT",
						"lifetime": models.MetadataLifetimePerTest,
					},
					HTTPReq: &models.HTTPReq{
						Method: models.Method("POST"),
						URL:    "/000000000000/localstack-queue/",
						Header: map[string]string{
							"Content-Type": "application/x-amz-json-1.0",
							"X-Amz-Target": "AmazonSQS.ReceiveMessage",
						},
						Body: `{"MaxNumberOfMessages":10,"QueueUrl":"http://sqs/q"}`,
					},
					HTTPResp: &models.HTTPResp{
						StatusCode: 200,
						Header:     map[string]string{"Content-Type": "application/x-amz-json-1.0"},
						Body:       `{"Messages":[{"MessageId":"1","Body":"Test message 1"}]}`,
					},
				},
				TestModeInfo: models.TestModeInfo{
					IsFiltered: true,
					SortOrder:  1,
					Lifetime:   models.LifetimePerTest,
					Consume:    models.ConsumeCursorSaturate,
				},
			}

			if ok := h.updateMock(tc.ctx, sqsDelivery, db, nil); !ok {
				t.Fatal("updateMock returned false for SQS delivery")
			}
			if db.markedUsed {
				t.Fatal("SQS delivery took MarkMockAsUsed keeping path instead of DeleteFilteredMock")
			}
			if db.deletedFiltered == nil || db.deletedFiltered.Name != "mock-2-sqs" {
				t.Fatalf("expected DeleteFilteredMock(mock-2-sqs), got %+v", db.deletedFiltered)
			}
		})
	}
}
