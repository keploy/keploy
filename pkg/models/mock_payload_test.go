package models

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mongoPayloadMock() *Mock {
	return &Mock{
		Name: "mock-1",
		Kind: Mongo,
		Spec: MockSpec{
			MongoRequests: []MongoRequest{{
				Header:    &MongoHeader{Length: 50, RequestID: 1},
				Message:   &MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"find":"apps"} }`}},
				ReadDelay: 7,
			}},
			MongoResponses: []MongoResponse{{
				Header:  &MongoHeader{Length: 60, RequestID: 2, ResponseTo: 1},
				Message: &MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"a"}]}} }`}},
			}},
		},
	}
}

// loaderOf returns a loader that reads back the payload src has now.
func loaderOf(src *Mock) PayloadLoader {
	reqs := append([]MongoRequest(nil), src.Spec.MongoRequests...)
	resps := append([]MongoResponse(nil), src.Spec.MongoResponses...)
	return func(_ context.Context, m *Mock) error {
		m.Spec.MongoRequests = append([]MongoRequest(nil), reqs...)
		m.Spec.MongoResponses = append([]MongoResponse(nil), resps...)
		return nil
	}
}

func TestLeavePayloadOnDisk(t *testing.T) {
	noop := func(context.Context, *Mock) error { return nil }
	tests := []struct {
		name  string
		mock  func() *Mock
		load  PayloadLoader
		wants bool
	}{
		{"a Mongo mock's messages", mongoPayloadMock, noop, true},
		{"not without a loader", mongoPayloadMock, nil, false},
		{"not another kind's payload", func() *Mock {
			return &Mock{Kind: HTTP, Spec: MockSpec{HTTPResp: &HTTPResp{Body: "big"}}}
		}, noop, false},
		{"not a Mongo mock with no messages", func() *Mock {
			m := mongoPayloadMock()
			m.Spec.MongoRequests[0].Message = nil
			m.Spec.MongoResponses[0].Message = nil
			return m
		}, noop, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.mock()
			before := len(m.Spec.MongoRequests) + len(m.Spec.MongoResponses)
			require.Equal(t, tt.wants, m.LeavePayloadOnDisk(tt.load))
			assert.Equal(t, tt.wants, m.PayloadOnDisk())
			// The mock still says how many requests and responses it has,
			// each with its header: the reports' summary asks for no more.
			assert.Equal(t, before, len(m.Spec.MongoRequests)+len(m.Spec.MongoResponses))
			if tt.wants {
				assert.Nil(t, m.Spec.MongoRequests[0].Message)
				assert.Nil(t, m.Spec.MongoResponses[0].Message)
				assert.Equal(t, int64(7), m.Spec.MongoRequests[0].ReadDelay)
				assert.Equal(t, int32(1), m.Spec.MongoResponses[0].Header.ResponseTo)
			}
		})
	}
}

func TestWithPayloadReadsThePayloadBackIntoACopy(t *testing.T) {
	ctx := context.Background()
	whole := mongoPayloadMock()
	m := mongoPayloadMock()
	require.True(t, m.LeavePayloadOnDisk(loaderOf(whole)))

	got, err := m.WithPayload(ctx)
	require.NoError(t, err)
	require.NotSame(t, m, got)
	assert.Equal(t, whole.Spec.MongoRequests, got.Spec.MongoRequests)
	assert.Equal(t, whole.Spec.MongoResponses, got.Spec.MongoResponses)
	assert.False(t, got.PayloadOnDisk(), "the copy is the mock as recorded")
	// The mock in memory still holds no payload, so a second send reads it
	// back again.
	assert.True(t, m.PayloadOnDisk())
	assert.Nil(t, m.Spec.MongoRequests[0].Message)
	assert.Nil(t, m.Spec.MongoResponses[0].Message)

	inMemory := mongoPayloadMock()
	same, err := inMemory.WithPayload(ctx)
	require.NoError(t, err)
	assert.Same(t, inMemory, same, "a mock whose payload is in memory is sent as it is")
}

func TestWithPayloadFailsAPayloadItCannotReadBack(t *testing.T) {
	ctx := context.Background()
	failing := mongoPayloadMock()
	require.True(t, failing.LeavePayloadOnDisk(func(context.Context, *Mock) error { return ErrPayloadFileReplaced }))
	_, err := failing.WithPayload(ctx)
	assert.ErrorIs(t, err, ErrPayloadFileReplaced)

	unread := mongoPayloadMock()
	require.True(t, unread.LeavePayloadOnDisk(func(context.Context, *Mock) error { return nil }))
	_, err = unread.WithPayload(ctx)
	assert.True(t, errors.Is(err, errPayloadNotRead), "a loader that read nothing back: %v", err)
}

// The copies the replay makes of a mock before sending it (the session pool's
// DeepCopy, ShallowCopy) are sent too, so they carry the loader.
func TestCopiesOfAMockKeepItsPayloadOnDisk(t *testing.T) {
	whole := mongoPayloadMock()
	m := mongoPayloadMock()
	require.True(t, m.LeavePayloadOnDisk(loaderOf(whole)))
	for name, c := range map[string]*Mock{"DeepCopy": m.DeepCopy(), "ShallowCopy": m.ShallowCopy()} {
		t.Run(name, func(t *testing.T) {
			require.True(t, c.PayloadOnDisk())
			got, err := c.WithPayload(context.Background())
			require.NoError(t, err)
			assert.Equal(t, whole.Spec.MongoResponses, got.Spec.MongoResponses)
		})
	}
}
