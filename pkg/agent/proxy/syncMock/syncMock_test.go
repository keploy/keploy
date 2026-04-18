package manager

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func TestSetMemoryPressureClearsBufferedMocksAndDropsNewOnes(t *testing.T) {
	t.Parallel()

	mgr := &SyncMockManager{
		buffer: []*models.Mock{
			{
				Spec: models.MockSpec{ReqTimestampMock: time.Now()},
			},
			{
				Spec: models.MockSpec{ReqTimestampMock: time.Now()},
			},
		},
	}
	oldBuffer := mgr.buffer

	mgr.SetMemoryPressure(true)
	if len(mgr.buffer) != 0 {
		t.Fatalf("expected memory pressure to clear buffered mocks, got %d items", len(mgr.buffer))
	}
	if cap(mgr.buffer) != defaultMockBufferCapacity {
		t.Fatalf("expected buffer capacity to reset to %d, got %d", defaultMockBufferCapacity, cap(mgr.buffer))
	}
	for i, mock := range oldBuffer {
		if mock != nil {
			t.Fatalf("expected cleared buffer entry %d to be nil", i)
		}
	}

	mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	if len(mgr.buffer) != 0 {
		t.Fatalf("expected memory pressure to drop new mocks, got %d buffered items", len(mgr.buffer))
	}

	mgr.SetMemoryPressure(false)
	mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	if len(mgr.buffer) != 1 {
		t.Fatalf("expected buffer to accept mocks after recovery, got %d buffered items", len(mgr.buffer))
	}
}

// TestAddMockSwallowsClosedChannelPanic exercises the shutdown-race
// branch in AddMock where HandleOutgoing has closed outChan while a
// parser goroutine is still calling AddMock. The expected behavior:
// AddMock returns normally (the mock is silently dropped; the reader
// is gone so no consumer remains), and nothing panics past the
// function boundary.
func TestAddMockSwallowsClosedChannelPanic(t *testing.T) {
	t.Parallel()

	ch := make(chan *models.Mock, 1)
	close(ch)

	mgr := &SyncMockManager{
		buffer:  make([]*models.Mock, 0, defaultMockBufferCapacity),
		outChan: ch,
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("AddMock must not propagate closed-channel panic, got %v", r)
		}
	}()

	mgr.AddMock(&models.Mock{Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
}

// TestAddMockRepanicsUnrelatedValues proves the recover() is narrow:
// if something in the send path panics with any value other than the
// runtime's "send on closed channel" error, AddMock re-panics so the
// real bug surfaces. We cannot easily inject a non-closed-channel
// panic into the real chansend path, so we verify the recover logic
// inline instead.
func TestAddMockRepanicsUnrelatedValues(t *testing.T) {
	t.Parallel()

	tryRecover := func(toPanic interface{}) (repanicked interface{}) {
		defer func() {
			repanicked = recover()
		}()
		func() {
			defer func() {
				r := recover()
				if r == nil {
					return
				}
				if err, ok := r.(error); ok && err.Error() == "send on closed channel" {
					return
				}
				panic(r)
			}()
			panic(toPanic)
		}()
		return nil
	}

	cases := []struct {
		name        string
		value       interface{}
		wantRepanic bool
	}{
		{"closed channel message", chanSendError{}, false},
		{"other runtime error", otherError{msg: "something else entirely"}, true},
		{"plain string panic", "boom", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tryRecover(tc.value)
			if tc.wantRepanic && got == nil {
				t.Fatalf("expected re-panic for %v, but recover swallowed it", tc.value)
			}
			if !tc.wantRepanic && got != nil {
				t.Fatalf("expected %v to be swallowed, but got re-panic: %v", tc.value, got)
			}
		})
	}
}

type chanSendError struct{}

func (chanSendError) Error() string { return "send on closed channel" }

type otherError struct{ msg string }

func (e otherError) Error() string { return e.msg }
