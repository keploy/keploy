package replay

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v2/pkg/models"
)

// MockInstrumentation is a mock implementation of the Instrumentation interface
type MockInstrumentation struct {
	unloadDoneChannels map[uint64]chan struct{}
}

func NewMockInstrumentation() *MockInstrumentation {
	return &MockInstrumentation{
		unloadDoneChannels: make(map[uint64]chan struct{}),
	}
}

func (m *MockInstrumentation) Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error) {
	return 12345, nil // Mock return value
}

func (m *MockInstrumentation) Hook(ctx context.Context, id uint64, opts models.HookOptions) error {
	return nil // Mock return value
}

func (m *MockInstrumentation) GetHookUnloadDone(id uint64) <-chan struct{} {
	if ch, exists := m.unloadDoneChannels[id]; exists {
		return ch
	}
	// Create a new channel for this app ID
	ch := make(chan struct{})
	m.unloadDoneChannels[id] = ch
	return ch
}

func (m *MockInstrumentation) MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error {
	return nil // Mock return value
}

func (m *MockInstrumentation) SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
	return nil // Mock return value
}

func (m *MockInstrumentation) GetConsumedMocks(ctx context.Context, id uint64) ([]models.MockState, error) {
	return []models.MockState{}, nil // Mock return value
}

func (m *MockInstrumentation) Run(ctx context.Context, id uint64, opts models.RunOptions) models.AppError {
	return models.AppError{} // Mock return value
}

func (m *MockInstrumentation) GetContainerIP(ctx context.Context, id uint64) (string, error) {
	return "127.0.0.1", nil // Mock return value
}

// CloseUnloadChannel simulates hooks being unloaded by closing the channel
func (m *MockInstrumentation) CloseUnloadChannel(id uint64) {
	if ch, exists := m.unloadDoneChannels[id]; exists {
		close(ch)
	}
}

func TestInstrumentState_UnloadDoneChannel(t *testing.T) {
	// Test the InstrumentState struct with UnloadDone channel
	unloadCh := make(chan struct{})

	state := &InstrumentState{
		AppID:      12345,
		HookCancel: func() {}, // dummy cancel function
		UnloadDone: unloadCh,
	}

	// Verify the channel is present and not closed
	assert.NotNil(t, state.UnloadDone, "UnloadDone channel should be present")

	select {
	case <-state.UnloadDone:
		t.Error("UnloadDone channel should not be closed initially")
	default:
		// Expected behavior
	}

	// Close the channel to simulate unload completion
	close(unloadCh)

	// Verify the channel is now closed
	select {
	case <-state.UnloadDone:
		// Expected behavior - channel is closed
	case <-time.After(100 * time.Millisecond):
		t.Error("UnloadDone channel should be closed after unload")
	}
}

func TestMockInstrumentation_GetHookUnloadDone(t *testing.T) {
	mockInstr := NewMockInstrumentation()

	appID := uint64(12345)

	// Get the channel
	ch := mockInstr.GetHookUnloadDone(appID)
	assert.NotNil(t, ch, "GetHookUnloadDone should return a channel")

	// Verify the channel is not closed initially
	select {
	case <-ch:
		t.Error("Channel should not be closed initially")
	default:
		// Expected behavior
	}

	// Multiple calls should return the same channel
	ch2 := mockInstr.GetHookUnloadDone(appID)
	assert.Equal(t, ch, ch2, "Multiple calls should return the same channel")
}

func TestMockInstrumentation_GetHookUnloadDone_DifferentApps(t *testing.T) {
	mockInstr := NewMockInstrumentation()

	appID1 := uint64(12345)
	appID2 := uint64(67890)

	// Get channels for different app IDs
	ch1 := mockInstr.GetHookUnloadDone(appID1)
	ch2 := mockInstr.GetHookUnloadDone(appID2)

	assert.NotNil(t, ch1, "First app should return a channel")
	assert.NotNil(t, ch2, "Second app should return a channel")
	assert.NotEqual(t, ch1, ch2, "Different app IDs should return different channels")
}

func TestMockInstrumentation_CloseUnloadChannel(t *testing.T) {
	mockInstr := NewMockInstrumentation()

	appID := uint64(12345)

	// Get the channel
	ch := mockInstr.GetHookUnloadDone(appID)

	// Verify not closed initially
	select {
	case <-ch:
		t.Error("Channel should not be closed initially")
	default:
		// Expected behavior
	}

	// Close the channel
	mockInstr.CloseUnloadChannel(appID)

	// Verify the channel is now closed
	select {
	case <-ch:
		// Expected behavior - channel is closed
	case <-time.After(100 * time.Millisecond):
		t.Error("Channel should be closed after CloseUnloadChannel")
	}
}

func TestInstrument_ChannelIntegration(t *testing.T) {
	// Test scenario similar to how the channel is used in the replay logic
	mockInstr := NewMockInstrumentation()
	appID := uint64(12345)

	// Simulate getting the instrument state
	unloadCh := mockInstr.GetHookUnloadDone(appID)
	state := &InstrumentState{
		AppID:      appID,
		HookCancel: func() {},
		UnloadDone: unloadCh,
	}

	// Simulate waiting for unload in a goroutine (similar to replay logic)
	done := make(chan bool, 1)
	go func() {
		select {
		case <-state.UnloadDone:
			done <- true
		case <-time.After(200 * time.Millisecond):
			done <- false
		}
	}()

	// Simulate unload happening after a delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		mockInstr.CloseUnloadChannel(appID)
	}()

	// Verify that the channel was closed and detected
	result := <-done
	assert.True(t, result, "Should detect channel closure within timeout")
}

func TestInstrument_MultipleUnloadWaiters(t *testing.T) {
	// Test multiple goroutines waiting on the same unload channel
	mockInstr := NewMockInstrumentation()
	appID := uint64(12345)

	unloadCh := mockInstr.GetHookUnloadDone(appID)

	// Create multiple waiters
	numWaiters := 3
	results := make(chan bool, numWaiters)

	for i := 0; i < numWaiters; i++ {
		go func() {
			select {
			case <-unloadCh:
				results <- true
			case <-time.After(200 * time.Millisecond):
				results <- false
			}
		}()
	}

	// Close the channel after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		mockInstr.CloseUnloadChannel(appID)
	}()

	// All waiters should detect the closure
	for i := 0; i < numWaiters; i++ {
		result := <-results
		assert.True(t, result, "Waiter %d should detect channel closure", i)
	}
}

func TestInstrument_ChannelReuse(t *testing.T) {
	// Test scenario where we get a new channel after the previous one was closed
	mockInstr := NewMockInstrumentation()
	appID := uint64(12345)

	// First channel
	ch1 := mockInstr.GetHookUnloadDone(appID)
	mockInstr.CloseUnloadChannel(appID)

	// Verify first channel is closed
	select {
	case <-ch1:
		// Expected behavior
	case <-time.After(100 * time.Millisecond):
		t.Error("First channel should be closed")
	}

	// Get a new channel (simulating a new load after unload)
	// Note: In the real implementation, a new channel would be created for each load
	// For this test, we'll manually create a new one
	delete(mockInstr.unloadDoneChannels, appID) // Remove the old closed channel
	ch2 := mockInstr.GetHookUnloadDone(appID)

	// Verify second channel is different and not closed
	assert.NotEqual(t, ch1, ch2, "New channel should be different from the closed one")

	select {
	case <-ch2:
		t.Error("New channel should not be closed initially")
	default:
		// Expected behavior
	}
}
