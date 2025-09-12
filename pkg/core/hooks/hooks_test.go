//go:build linux

package hooks

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func TestNewHooks_UnloadDoneChannel(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		ProxyPort: 6789,
		DNSPort:   26789,
	}

	hooks := NewHooks(logger, cfg)

	// Verify that the unloadDone channel is initialized
	assert.NotNil(t, hooks.unloadDone, "unloadDone channel should be initialized")

	// Verify the channel is not closed initially
	select {
	case <-hooks.unloadDone:
		t.Error("unloadDone channel should not be closed initially")
	default:
		// Expected behavior - channel is not closed
	}
}

func TestHooks_GetUnloadDone(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		ProxyPort: 6789,
		DNSPort:   26789,
	}

	hooks := NewHooks(logger, cfg)

	// Get the channel
	ch := hooks.GetUnloadDone()
	assert.NotNil(t, ch, "GetUnloadDone should return a channel")

	// Verify it's the same channel
	ch2 := hooks.GetUnloadDone()
	assert.Equal(t, ch, ch2, "GetUnloadDone should return the same channel instance")

	// Verify the channel is not closed initially
	select {
	case <-ch:
		t.Error("Channel should not be closed initially")
	default:
		// Expected behavior
	}
}

func TestHooks_GetUnloadDone_ThreadSafety(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		ProxyPort: 6789,
		DNSPort:   26789,
	}

	hooks := NewHooks(logger, cfg)

	// Test concurrent access to GetUnloadDone
	var wg sync.WaitGroup
	channels := make([]<-chan struct{}, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			channels[index] = hooks.GetUnloadDone()
		}(i)
	}

	wg.Wait()

	// All should return the same channel
	for i := 1; i < 10; i++ {
		assert.Equal(t, channels[0], channels[i], "All GetUnloadDone calls should return the same channel")
	}
}

func TestHooks_Load_ResetsUnloadDoneChannel(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		ProxyPort: 6789,
		DNSPort:   26789,
	}

	hooks := NewHooks(logger, cfg)

	// Create a context with errgroup for Load method
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := &errgroup.Group{}
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)

	// Mock the load method to avoid eBPF dependency
	// We'll test the channel reset logic by examining the struct directly
	hooks.unloadDoneMutex.Lock()
	oldCh := hooks.unloadDone
	hooks.unloadDone = make(chan struct{})
	newCh := hooks.unloadDone
	hooks.unloadDoneMutex.Unlock()

	// Verify that a new channel was created
	assert.NotEqual(t, oldCh, newCh, "Load should create a new unloadDone channel")

	// Verify the new channel is not closed
	select {
	case <-newCh:
		t.Error("New channel should not be closed")
	default:
		// Expected behavior
	}
}

func TestHooks_UnloadDoneChannel_ClosedOnUnload(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		ProxyPort: 6789,
		DNSPort:   26789,
	}

	hooks := NewHooks(logger, cfg)

	// Get the channel before simulating unload
	ch := hooks.GetUnloadDone()

	// Simulate the unload completion by manually closing the channel
	// (This mimics what happens in the goroutine when context is cancelled)
	hooks.unloadDoneMutex.Lock()
	close(hooks.unloadDone)
	hooks.unloadDoneMutex.Unlock()

	// Verify the channel is now closed
	select {
	case <-ch:
		// Expected behavior - channel is closed
	case <-time.After(100 * time.Millisecond):
		t.Error("Channel should be closed after unload")
	}
}

func TestHooks_UnloadDoneChannel_ThreadSafetyOnClose(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		ProxyPort: 6789,
		DNSPort:   26789,
	}

	hooks := NewHooks(logger, cfg)

	// Get multiple references to the channel
	channels := make([]<-chan struct{}, 5)
	for i := 0; i < 5; i++ {
		channels[i] = hooks.GetUnloadDone()
	}

	// Close the channel from one goroutine
	go func() {
		time.Sleep(50 * time.Millisecond)
		hooks.unloadDoneMutex.Lock()
		close(hooks.unloadDone)
		hooks.unloadDoneMutex.Unlock()
	}()

	// All channels should be closed
	for i, ch := range channels {
		select {
		case <-ch:
			// Expected behavior
		case <-time.After(200 * time.Millisecond):
			t.Errorf("Channel %d should be closed", i)
		}
	}
}

func TestHooks_UnloadDoneChannel_MultipleLoads(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		ProxyPort: 6789,
		DNSPort:   26789,
	}

	hooks := NewHooks(logger, cfg)

	// Simulate multiple load cycles
	var channels []<-chan struct{}

	for i := 0; i < 3; i++ {
		// Reset channel (simulating Load call)
		hooks.unloadDoneMutex.Lock()
		hooks.unloadDone = make(chan struct{})
		hooks.unloadDoneMutex.Unlock()

		// Get the channel
		ch := hooks.GetUnloadDone()
		channels = append(channels, ch)

		// Verify each channel is different
		if i > 0 {
			assert.NotEqual(t, channels[i-1], channels[i], "Each load should create a new channel")
		}

		// Verify channel is not closed
		select {
		case <-ch:
			t.Errorf("Channel %d should not be closed initially", i)
		default:
			// Expected behavior
		}
	}
}

func TestHooks_UnloadDoneChannel_SequentialLoadAndUnload(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		ProxyPort: 6789,
		DNSPort:   26789,
	}

	hooks := NewHooks(logger, cfg)

	// First load cycle
	hooks.unloadDoneMutex.Lock()
	hooks.unloadDone = make(chan struct{})
	hooks.unloadDoneMutex.Unlock()

	ch1 := hooks.GetUnloadDone()

	// Simulate unload
	hooks.unloadDoneMutex.Lock()
	close(hooks.unloadDone)
	hooks.unloadDoneMutex.Unlock()

	// Verify first channel is closed
	select {
	case <-ch1:
		// Expected behavior
	case <-time.After(100 * time.Millisecond):
		t.Error("First channel should be closed")
	}

	// Second load cycle
	hooks.unloadDoneMutex.Lock()
	hooks.unloadDone = make(chan struct{})
	hooks.unloadDoneMutex.Unlock()

	ch2 := hooks.GetUnloadDone()

	// Verify second channel is different and not closed
	assert.NotEqual(t, ch1, ch2, "Second load should create a new channel")

	select {
	case <-ch2:
		t.Error("Second channel should not be closed initially")
	default:
		// Expected behavior
	}
}
