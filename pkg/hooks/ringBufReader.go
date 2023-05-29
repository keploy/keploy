package hooks

import (
	"bytes"
	"encoding/binary"
	"errors"
	_ "strings"

	"fmt"
	"log"
	"os"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/ringbuf"
	"go.keploy.io/server/pkg/hooks/connection"
	"go.keploy.io/server/pkg/hooks/settings"
	"go.keploy.io/server/pkg/hooks/structs"
	_ "golang.org/x/sys/unix"
)

var PerfEventReaders []*perf.Reader
var RingEventReaders []*ringbuf.Reader

// LaunchPerfBufferConsumers launches socket events
func LaunchPerfBufferConsumers(objs bpfObjects, connectionFactory *connection.Factory, stopper chan os.Signal) {

	launchSocketOpenEvent(objs.SocketOpenEvents, connectionFactory, stopper)
	launchSocketDataEvent(objs.SocketDataEvents, connectionFactory, stopper)
	launchSocketCloseEvent(objs.SocketCloseEvents, connectionFactory, stopper)
}

func launchSocketOpenEvent(openEventMap *ebpf.Map, connectionFactory *connection.Factory, stopper chan os.Signal) {

	// Open a perf event reader from userspace on the PERF_EVENT_ARRAY map
	// described in the eBPF C program.
	reader, err := perf.NewReader(openEventMap, os.Getpagesize())
	if err != nil {
		log.Fatalf("error creating perf event reader of socketOpenEvent: %s", err)
	}
	// defer reader.Close()
	PerfEventReaders = append(PerfEventReaders, reader)

	go socketOpenEventCallback(reader, connectionFactory)
}

func launchSocketDataEvent(dataEventMap *ebpf.Map, connectionFactory *connection.Factory, stopper chan os.Signal) {

	// Open a ringbuf event reader from userspace on the RING_BUF map
	// described in the eBPF C program.
	reader, err := ringbuf.NewReader(dataEventMap)
	if err != nil {
		log.Fatalf("error creating ring buffer of socketDataEvent: %s", err)
	}
	// defer reader.Close()
	RingEventReaders = append(RingEventReaders, reader)

	go socketDataEventCallback(reader, connectionFactory)

}

func launchSocketCloseEvent(closeEventMap *ebpf.Map, connectionFactory *connection.Factory, stopper chan os.Signal) {

	// Open a perf event reader from userspace on the PERF_EVENT_ARRAY map
	// described in the eBPF C program.
	reader, err := perf.NewReader(closeEventMap, os.Getpagesize())
	if err != nil {
		log.Fatalf("error creating perf event reader of socketCloseEvent: %s", err)
	}
	// defer reader.Close()
	PerfEventReaders = append(PerfEventReaders, reader)

	go socketCloseEventCallback(reader, connectionFactory)
}

var eventAttributesSize = int(unsafe.Sizeof(structs.SocketDataEvent{}))

func socketDataEventCallback(reader *ringbuf.Reader, connectionFactory *connection.Factory) {

	for {

		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				log.Println("received signal, exiting...")
				return
			}
			log.Printf("reading from ringbuf socketDataEvent reader: %s", err)
			continue
		}
		// else {
		// 	log.Printf("\n<-------->\nSuccessfully received socketDataEvent from ebpf code...\n=====\n")
		// }

		data := record.RawSample
		// println("length of data in socketdataevent:", len(data))
		if len(data) < eventAttributesSize {
			log.Printf("Buffer's for SocketDataEvent is smaller (%d) than the minimum required (%d)", len(data), eventAttributesSize)
			continue
		} else if len(data) > structs.EventBodyMaxSize+eventAttributesSize {
			log.Printf("Buffer's for SocketDataEvent is bigger (%d) than the maximum for the struct (%d)", len(data), structs.EventBodyMaxSize+eventAttributesSize)
			continue
		}

		// log.Printf("data is in the range bro...")
		var event structs.SocketDataEvent

		if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &event); err != nil {
			log.Printf("Failed to decode received data: %+v", err)
			continue
		}

		// temp := unix.ByteSliceToString(event.Msg[:])
		// len := len(strings.Trim(temp, " "))

		if event.Direction == structs.EgressTraffic {

			// log.Printf("[Egress]: Event buffer actual len: %v", len)
			// log.Printf("[Egress]: Event buffer assigned len: %v", event.MsgSize)

			// a := fmt.Sprintf("[Egress]: Event buffer actual len: %v", len)
			// b := fmt.Sprintf("[Egress]: Event buffer assigned len: %v", event.MsgSize)
			// LogAny(a)
			// LogAny(b)
		} else {
			// log.Printf("[Ingress]: Event buffer actual len: %v", len)
			// log.Printf("[Ingress]: Event buffer assigned len: %v", event.MsgSize)
			// a := fmt.Sprintf("[Ingress]: Event buffer actual len: %v", len)
			// b := fmt.Sprintf("[Ingress]: Event buffer assigned len: %v", event.MsgSize)
			// LogAny(a)
			// LogAny(b)
		}

		// log.Printf("Event Msg Actual size:%v", len(event.Msg))
		// if event.Direction == structs.EgressTraffic {
		// 	log.Printf("Response Event Msg:\n%s", unix.ByteSliceToString(event.Msg[:]))
		// } else {
		// 	log.Printf("Request Event Msg:\n%s", unix.ByteSliceToString(event.Msg[:]))
		// }

		// log.Printf("Event MsgSize sent from ebpf:%v", event.MsgSize)

		event.TimestampNano += settings.GetRealTimeOffset()
		connectionFactory.GetOrCreate(event.ConnID).AddDataEvent(event)

	}
}

func socketOpenEventCallback(reader *perf.Reader, connectionFactory *connection.Factory) {
	for {

		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				return
			}
			log.Printf("reading from perf socketOpenEvent reader: %s", err)
			continue
		}

		if record.LostSamples != 0 {
			log.Printf("perf socketOpenEvent array full, dropped %d samples", record.LostSamples)
			continue
		}
		data := record.RawSample
		var event structs.SocketOpenEvent

		if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &event); err != nil {
			log.Printf("Failed to decode received data: %+v", err)
			continue
		}
		// log.Printf("i got the open event bro:%v", event)

		event.TimestampNano += settings.GetRealTimeOffset()
		connectionFactory.GetOrCreate(event.ConnID).AddOpenEvent(event)
	}
}

func socketCloseEventCallback(reader *perf.Reader, connectionFactory *connection.Factory) {
	for {

		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				return
			}
			log.Printf("reading from perf socketCloseEvent reader: %s", err)
			continue
		}

		if record.LostSamples != 0 {
			log.Printf("perf socketCloseEvent array full, dropped %d samples", record.LostSamples)
			continue
		}
		data := record.RawSample

		var event structs.SocketCloseEvent
		if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &event); err != nil {
			log.Printf("Failed to decode received data: %+v", err)
			continue
		}

		log.Printf("i got the close event bro:%v", event)

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
