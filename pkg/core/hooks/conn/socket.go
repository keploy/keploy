//go:build linux

package conn

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	"github.com/cilium/ebpf"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"

	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/ringbuf"
	"go.uber.org/zap"
)

var eventAttributesSize = int(unsafe.Sizeof(SocketDataEvent{}))

// ListenSocket starts the socket event listeners
func ListenSocket(ctx context.Context, l *zap.Logger, openMap, dataMap, closeMap *ebpf.Map, opts models.IncomingOptions , conf *config.Config) (<-chan *models.TestCase, error) {
	t := make(chan *models.TestCase, 500)
	err := initRealTimeOffset()
	if err != nil {
		utils.LogError(l, err, "failed to initialize real time offset")
		return nil, errors.New("failed to start socket listeners")
	}
	c := NewFactory(time.Minute, l , conf)
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return nil, errors.New("failed to get the error group from the context")
	}
	g.Go(func() error {
		defer utils.Recover(l)
		go func() {
			defer utils.Recover(l)
			for {
				select {
				case <-ctx.Done():
					return
				default:
					// TODO refactor this to directly consume the events from the maps
					c.ProcessActiveTrackers(ctx, t, opts)
					time.Sleep(100 * time.Millisecond)
				}
			}
		}()
		<-ctx.Done()
		close(t)
		return nil
	})

	err = open(ctx, c, l, openMap)
	if err != nil {
		utils.LogError(l, err, "failed to start open socket listener")
		return nil, errors.New("failed to start socket listeners")
	}
	err = data(ctx, c, l, dataMap)
	if err != nil {
		utils.LogError(l, err, "failed to start data socket listener")
		return nil, errors.New("failed to start socket listeners")
	}
	err = exit(ctx, c, l, closeMap)
	if err != nil {
		utils.LogError(l, err, "failed to start close socket listener")
		return nil, errors.New("failed to start socket listeners")
	}
	return t, err
}

func open(ctx context.Context, c *Factory, l *zap.Logger, m *ebpf.Map) error {

	r, err := perf.NewReader(m, os.Getpagesize())
	if err != nil {
		utils.LogError(l, nil, "failed to create perf event reader of socketOpenEvent")
		return err
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}
	g.Go(func() error {
		defer utils.Recover(l)
		go func() {
			defer utils.Recover(l)
			for {
				rec, err := r.Read()
				if err != nil {
					if errors.Is(err, perf.ErrClosed) {
						return
					}
					utils.LogError(l, err, "failed to read from perf socketOpenEvent reader")
					continue
				}

				if rec.LostSamples != 0 {
					l.Debug("Unable to add samples to the socketOpenEvent array due to its full capacity", zap.Any("samples", rec.LostSamples))
					continue
				}
				data := rec.RawSample
				var event SocketOpenEvent

				if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &event); err != nil {
					utils.LogError(l, err, "failed to decode the received data from perf socketOpenEvent reader")
					continue
				}

				event.TimestampNano += getRealTimeOffset()
				c.GetOrCreate(event.ConnID).AddOpenEvent(event)
			}
		}()
		<-ctx.Done() // Check for context cancellation
		err := r.Close()
		if err != nil {
			utils.LogError(l, err, "failed to close perf socketOpenEvent reader")
		}
		return nil
	})
	return nil
}

func data(ctx context.Context, c *Factory, l *zap.Logger, m *ebpf.Map) error {
	r, err := ringbuf.NewReader(m)
	if err != nil {
		utils.LogError(l, nil, "failed to create ring buffer of socketDataEvent")
		return err
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}
	g.Go(func() error {
		defer utils.Recover(l)
		go func() {
			defer utils.Recover(l)
			for {
				record, err := r.Read()
				if err != nil {
					if !errors.Is(err, ringbuf.ErrClosed) {
						utils.LogError(l, err, "failed to receive signal from ringbuf socketDataEvent reader")
						return
					}
					continue
				}

				bin := record.RawSample
				if len(bin) < eventAttributesSize {
					l.Debug(fmt.Sprintf("Buffer's for SocketDataEvent is smaller (%d) than the minimum required (%d)", len(bin), eventAttributesSize))
					continue
				} else if len(bin) > EventBodyMaxSize+eventAttributesSize {
					l.Debug(fmt.Sprintf("Buffer's for SocketDataEvent is bigger (%d) than the maximum for the struct (%d)", len(bin), EventBodyMaxSize+eventAttributesSize))
					continue
				}

				var event SocketDataEvent

				if err := binary.Read(bytes.NewReader(bin), binary.LittleEndian, &event); err != nil {
					utils.LogError(l, err, "failed to decode the received data from ringbuf socketDataEvent reader")
					continue
				}

				event.TimestampNano += getRealTimeOffset()

				if event.Direction == IngressTraffic {
					event.EntryTimestampNano += getRealTimeOffset()
					l.Debug(fmt.Sprintf("Request EntryTimestamp :%v\n", convertUnixNanoToTime(event.EntryTimestampNano)))
				}

				c.GetOrCreate(event.ConnID).AddDataEvent(event)
			}
		}()
		<-ctx.Done() // Check for context cancellation
		err := r.Close()
		if err != nil {
			utils.LogError(l, err, "failed to close ringbuf socketDataEvent reader")
		}
		return nil
	})
	return nil
}

func exit(ctx context.Context, c *Factory, l *zap.Logger, m *ebpf.Map) error {

	r, err := perf.NewReader(m, os.Getpagesize())
	if err != nil {
		utils.LogError(l, nil, "failed to create perf event reader of socketCloseEvent")
		return err
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}
	g.Go(func() error {
		defer utils.Recover(l)
		go func() {
			defer utils.Recover(l)
			for {
				rec, err := r.Read()
				if err != nil {
					if errors.Is(err, perf.ErrClosed) {
						return
					}
					utils.LogError(l, err, "failed to read from perf socketCloseEvent reader")
					continue
				}
				if rec.LostSamples != 0 {
					l.Debug(fmt.Sprintf("perf socketCloseEvent array full, dropped %d samples", rec.LostSamples))
					continue
				}
				data := rec.RawSample

				var event SocketCloseEvent
				if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &event); err != nil {
					l.Debug(fmt.Sprintf("Failed to decode received data: %+v", err))
					continue
				}

				event.TimestampNano += getRealTimeOffset()
				c.GetOrCreate(event.ConnID).AddCloseEvent(event)
			}
		}()

		<-ctx.Done() // Check for context cancellation
		err := r.Close()
		if err != nil {
			utils.LogError(l, err, "failed to close perf socketCloseEvent reader")
			return err
		}
		return nil
	})
	return nil
}

// InitRealTimeOffset calculates the offset between the real clock and the monotonic clock used in the BPF.
func initRealTimeOffset() error {
	var monotonicTime, realTime unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonicTime); err != nil {
		return fmt.Errorf("failed getting monotonic clock due to: %v", err)
	}
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &realTime); err != nil {
		return fmt.Errorf("failed getting real clock time due to: %v", err)
	}
	realTimeOffset = uint64(time.Second)*(uint64(realTime.Sec)-uint64(monotonicTime.Sec)) + uint64(realTime.Nsec) - uint64(monotonicTime.Nsec)
	// realTimeCopy := time.Unix(int64(realTimeOffset/1e9), int64(realTimeOffset%1e9))
	// log.Debug(fmt.Sprintf("%s real time offset is: %v", Emoji, realTimeCopy))
	return nil
}

// GetRealTimeOffset is a getter for the real-time-offset.
func getRealTimeOffset() uint64 {
	return realTimeOffset
}
