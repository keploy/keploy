package hooks

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	_ "strings"
	"time"
	"unsafe"

	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/ringbuf"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks/connection"
	"go.keploy.io/server/pkg/hooks/settings"
	"go.keploy.io/server/pkg/hooks/structs"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
	_ "golang.org/x/sys/unix"
)

var PerfEventReaders []*perf.Reader
var RingEventReaders []*ringbuf.Reader

// LaunchPerfBufferConsumers launches socket events
func (h *Hook) LaunchPerfBufferConsumers(connectionFactory *connection.Factory) {

	h.launchSocketOpenEvent(connectionFactory)
	h.launchSocketDataEvent(connectionFactory)
	h.launchSocketCloseEvent(connectionFactory)
}

func (h *Hook) launchSocketOpenEvent(connectionFactory *connection.Factory) {

	// Open a perf event reader from userspace on the PERF_EVENT_ARRAY map
	// described in the eBPF C program.
	reader, err := perf.NewReader(h.objects.SocketOpenEvents, os.Getpagesize())
	if err != nil {
		h.logger.Error("failed to create perf event reader of socketOpenEvent", zap.Error(err))
		return
	}
	PerfEventReaders = append(PerfEventReaders, reader)

	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())
		defer utils.HandlePanic()
		socketOpenEventCallback(reader, connectionFactory, h.logger)
	}()
}

func (h *Hook) launchSocketDataEvent(connectionFactory *connection.Factory) {

	// Open a ringbuf event reader from userspace on the RING_BUF map
	// described in the eBPF C program.
	reader, err := ringbuf.NewReader(h.objects.SocketDataEvents)
	if err != nil {
		h.logger.Error("failed to create ring buffer of socketDataEvent", zap.Error(err))
		return
	}
	RingEventReaders = append(RingEventReaders, reader)

	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())
		defer utils.HandlePanic()
		socketDataEventCallback(reader, connectionFactory, h.logger)
	}()

}

func (h *Hook) launchSocketCloseEvent(connectionFactory *connection.Factory) {

	// Open a perf event reader from userspace on the PERF_EVENT_ARRAY map
	// described in the eBPF C program.
	reader, err := perf.NewReader(h.objects.SocketCloseEvents, os.Getpagesize())
	if err != nil {
		h.logger.Error("failed to create perf event reader of socketCloseEvent", zap.Error(err))
		return
	}
	PerfEventReaders = append(PerfEventReaders, reader)

	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())
		defer utils.HandlePanic()
		socketCloseEventCallback(reader, connectionFactory, h.logger)
	}()
}

var eventAttributesSize = int(unsafe.Sizeof(structs.SocketDataEvent{}))

func socketDataEventCallback(reader *ringbuf.Reader, connectionFactory *connection.Factory, logger *zap.Logger) {

	for {

		record, err := reader.Read()
		if err != nil {
			if !errors.Is(err, ringbuf.ErrClosed) {
				logger.Error("failed to receive signal from ringbuf socketDataEvent reader", zap.Error(err))
				return
			}
			continue
		}

		data := record.RawSample
		if len(data) < eventAttributesSize {
			logger.Debug(fmt.Sprintf("Buffer's for SocketDataEvent is smaller (%d) than the minimum required (%d)", len(data), eventAttributesSize))
			continue
		} else if len(data) > structs.EventBodyMaxSize+eventAttributesSize {
			logger.Debug(fmt.Sprintf("Buffer's for SocketDataEvent is bigger (%d) than the maximum for the struct (%d)", len(data), structs.EventBodyMaxSize+eventAttributesSize))
			continue
		}

		var event structs.SocketDataEvent

		if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &event); err != nil {
			logger.Error("failed to decode the recieve data from ringbuf socketDataEvent reader", zap.Error(err))
			continue
		}
		
		event.TimestampNano += settings.GetRealTimeOffset()
		event.EntryTimestampNano += settings.GetRealTimeOffset()
		
		connectionFactory.GetOrCreate(event.ConnID).AddDataEvent(event)

	}
}

func socketOpenEventCallback(reader *perf.Reader, connectionFactory *connection.Factory, logger *zap.Logger) {
	for {

		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				return
			}
			logger.Error("failed to read from perf socketOpenEvent reader", zap.Error(err))
			continue
		}

		if record.LostSamples != 0 {
			logger.Debug("Unable to add samples to the socketOpenEvent array due to its full capacity", zap.Any("samples", record.LostSamples))
			continue
		}
		data := record.RawSample
		var event structs.SocketOpenEvent

		if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &event); err != nil {
			logger.Error("failed to decode the recieved data from perf socketOpenEvent reader", zap.Error(err))
			continue
		}

		event.TimestampNano += settings.GetRealTimeOffset()
		connectionFactory.GetOrCreate(event.ConnID).AddOpenEvent(event)
	}
}

func socketCloseEventCallback(reader *perf.Reader, connectionFactory *connection.Factory, logger *zap.Logger) {
	for {

		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				return
			}
			logger.Error("reading from perf socketCloseEvent reader", zap.Error(err))
			continue
		}

		if record.LostSamples != 0 {
			logger.Debug(fmt.Sprintf("perf socketCloseEvent array full, dropped %d samples", record.LostSamples))
			continue
		}
		data := record.RawSample

		var event structs.SocketCloseEvent
		if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &event); err != nil {
			logger.Debug(fmt.Sprintf("Failed to decode received data: %+v", err))
			continue
		}

		event.TimestampNano += settings.GetRealTimeOffset()
		connectionFactory.GetOrCreate(event.ConnID).AddCloseEvent(event)
	}
}

// LogAny appends input of any type to a logs.txt file in the current directory
func LogAny(value string) error {

	logMessage := value

	// Add a timestamp to the log message
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	logLine := fmt.Sprintf("%s - %s\n", timestamp, logMessage)

	// Open logs.txt in append mode, create it if it doesn't exist
	file, err := os.OpenFile("logs.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	// Write the log line to the file
	_, err = file.WriteString(logLine)
	if err != nil {
		return err
	}

	return nil
}
