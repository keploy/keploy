package postgresparser

import (
	"encoding/binary"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

func checkScram(packet string, log *zap.Logger) bool {
	encoded, err := PostgresDecoder(packet)
	if err != nil {
		log.Error("error in decoding packet", zap.Error(err))
		return false
	}
	// check if payload contains SCRAM-SHA-256
	messageType := encoded[0]
	log.Debug("Message Type: %c\n", zap.String("messageType", string(messageType)))
	if messageType == 'N' {
		return false
	}
	// Print the message payload (for simplicity, the payload is printed as a string)
	payload := string(encoded[5:])
	// fmt.Printf("Payload: %s\n", payload)
	if messageType == 'R' {
		if strings.Contains(payload, "SCRAM-SHA") {
			log.Debug("scram packet")
			return true
		}
	}

	return false
}

func isStartupPacket(packet []byte) bool {
	protocolVersion := binary.BigEndian.Uint32(packet[4:8])
	return protocolVersion == 196608 // 3.0 in PostgreSQL
}

func isRegularPacket(packet []byte) bool {
	messageType := packet[0]
	return messageType == 'Q' || messageType == 'P' || messageType == 'D' || messageType == 'C' || messageType == 'E'
}

func printStartupPacketDetails(packet []byte) {
	// fmt.Printf("Protocol Version: %d\n", binary.BigEndian.Uint32(packet[4:8]))

	// Print key-value pairs (for simplicity, only one key-value pair is shown)
	keyStart := 8
	for keyStart < len(packet) && packet[keyStart] != 0 {
		keyEnd := keyStart
		for keyEnd < len(packet) && packet[keyEnd] != 0 {
			keyEnd++
		}
		key := string(packet[keyStart:keyEnd])

		valueStart := keyEnd + 1
		valueEnd := valueStart
		for valueEnd < len(packet) && packet[valueEnd] != 0 {
			valueEnd++
		}
		value := string(packet[valueStart:valueEnd])

		fmt.Printf("Key: %s, Value: %s\n", key, value)

		keyStart = valueEnd + 1
	}
}

func printRegularPacketDetails(packet []byte) {
	messageType := packet[0]
	fmt.Printf("Message Type: %c\n", messageType)

	// Print the message payload (for simplicity, the payload is printed as a string)
	payload := string(packet[5:])
	fmt.Printf("Payload: %s\n", payload)
}
