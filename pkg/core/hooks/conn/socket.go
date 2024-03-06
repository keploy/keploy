package conn

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	_ "strings"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"

	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/ringbuf"
	"go.uber.org/zap"
	_ "golang.org/x/sys/unix"
)

var eventAttributesSize = int(unsafe.Sizeof(SocketDataEvent{}))

// ListenSocket starts the socket event listeners
func ListenSocket(ctx context.Context, l *zap.Logger, openMap, dataMap, closeMap *ebpf.Map) (<-chan *models.TestCase, <-chan error) {
	errCh := make(chan error, 10) // Buffered channel to prevent blocking
	t := make(chan *models.TestCase, 500)
	err := initRealTimeOffset()
	if err != nil {
		utils.LogError(l, err, "failed to initialize real time offset")
		errCh <- err
		return nil, errCh
	}
	c := NewFactory(time.Minute, l)
	go func() {
		defer utils.Recover(l)
		for {
			select {
			case <-ctx.Done():
				close(t)
				errCh <- ctx.Err()
				close(errCh)
				return
			default:
				// TODO refactor this to directly consume the events from the maps
				//TODO: pass errCh to this function
				c.ProcessActiveTrackers(ctx, t)
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	go open(ctx, c, l, openMap, errCh)
	go data(ctx, c, l, dataMap, errCh)
	go exit(ctx, c, l, closeMap, errCh)
	return t, errCh
}

func open(ctx context.Context, c *Factory, l *zap.Logger, m *ebpf.Map, errCh chan error) {
	defer utils.Recover(l)
	defer close(errCh) // Close the channel when the function exits

	r, err := perf.NewReader(m, os.Getpagesize())
	if err != nil {
		utils.LogError(l, err, "failed to create perf event reader of socketOpenEvent")
		errCh <- err
		return
	}
	defer r.Close() // Ensure the reader is closed when the function exits

	for {
		select {
		case <-ctx.Done(): // Check for context cancellation
			errCh <- ctx.Err()
			return
		default:
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
	}
}

func data(ctx context.Context, c *Factory, l *zap.Logger, m *ebpf.Map, errCh chan error) {
	defer utils.Recover(l)
	r, err := ringbuf.NewReader(m)
	if err != nil {
		utils.LogError(l, err, "failed to create ring buffer of socketDataEvent")
		errCh <- err
		return
	}
	defer r.Close() // Ensure the reader is closed when the function exits

	for {
		select {
		case <-ctx.Done(): // Check for context cancellation
			errCh <- ctx.Err()
			return
		default:
			record, err := r.Read()
			if err != nil {
				if !errors.Is(err, ringbuf.ErrClosed) {
					utils.LogError(l, err, "failed to receive signal from ringbuf socketDataEvent reader")
					errCh <- err
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
	}
}

func exit(ctx context.Context, c *Factory, l *zap.Logger, m *ebpf.Map, errCh chan error) {
	defer utils.Recover(l)
	r, err := perf.NewReader(m, os.Getpagesize())
	if err != nil {
		utils.LogError(l, err, "failed to create perf event reader of socketCloseEvent")
		errCh <- err
		return
	}
	defer r.Close() // Ensure the reader is closed when the function exits

	for {
		select {
		case <-ctx.Done(): // Check for context cancellation
			errCh <- ctx.Err()
			return
		default:
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
	}
}
