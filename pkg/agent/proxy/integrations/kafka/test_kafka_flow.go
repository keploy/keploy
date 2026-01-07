package kafka

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

func main() {
	// Start a fake Kafka broker that echoes responses
	go startFakeBroker()
	time.Sleep(100 * time.Millisecond)

	// Connect through Keploy proxy (assuming it's on port 16789)
	// If you run Keploy on a different port, change this
	proxyAddr := "localhost:16789"
	fmt.Printf("proxyaddress: %s\n", proxyAddr)

	// Or connect directly to test the protocol parsing
	// This just tests that you can create valid Kafka packets
	brokerAddr := "localhost:9092"
	fmt.Printf("brokeraddress: %s\n", brokerAddr)

	fmt.Println("Testing Kafka packet creation...")

	// Create a Kafka ApiVersions request (simplest request)
	packet := createApiVersionsRequest(1, "test-client")
	fmt.Printf("Created Kafka packet: %d bytes\n", len(packet))
	fmt.Printf("Packet hex: %x\n", packet)

	// Try to connect to the fake broker
	conn, err := net.Dial("tcp", brokerAddr)
	if err != nil {
		fmt.Printf("Could not connect to %s: %v\n", brokerAddr, err)
		fmt.Println("\nTo test with Keploy proxy:")
		fmt.Println("1. Start Keploy in record mode")
		fmt.Println("2. Start a Kafka broker (or use Docker)")
		fmt.Println("3. Run your Kafka client app through Keploy")
		return
	}
	defer conn.Close()

	// Send request
	_, err = conn.Write(packet)
	if err != nil {
		fmt.Printf("Failed to write: %v\n", err)
		return
	}
	fmt.Println("Sent Kafka request")

	// Read response
	resp := make([]byte, 1024)
	n, err := conn.Read(resp)
	if err != nil && err != io.EOF {
		fmt.Printf("Failed to read: %v\n", err)
		return
	}
	fmt.Printf("Received response: %d bytes\n", n)
	if n >= 8 {
		corrID := int32(binary.BigEndian.Uint32(resp[4:8]))
		fmt.Printf("Response CorrelationID: %d\n", corrID)
	}
}

func createApiVersionsRequest(correlationID int32, clientID string) []byte {
	// Kafka Request Header:
	// - Size: 4 bytes (int32) - length of message after this
	// - ApiKey: 2 bytes (int16) - 18 for ApiVersions
	// - ApiVersion: 2 bytes (int16) - version 0
	// - CorrelationID: 4 bytes (int32)
	// - ClientID: 2 bytes length + string

	clientIDBytes := []byte(clientID)
	headerSize := 2 + 2 + 4 + 2 + len(clientIDBytes) // apiKey + apiVersion + corrID + clientIDLen + clientID

	packet := make([]byte, 4+headerSize)

	// Size (everything after this field)
	binary.BigEndian.PutUint32(packet[0:4], uint32(headerSize))

	// ApiKey = 18 (ApiVersions)
	binary.BigEndian.PutUint16(packet[4:6], 18)

	// ApiVersion = 0
	binary.BigEndian.PutUint16(packet[6:8], 0)

	// CorrelationID
	binary.BigEndian.PutUint32(packet[8:12], uint32(correlationID))

	// ClientID
	binary.BigEndian.PutUint16(packet[12:14], uint16(len(clientIDBytes)))
	copy(packet[14:], clientIDBytes)

	return packet
}

func startFakeBroker() {
	ln, err := net.Listen("tcp", ":9092")
	if err != nil {
		fmt.Printf("Could not start fake broker: %v\n", err)
		return
	}
	defer ln.Close()
	fmt.Println("Fake Kafka broker listening on :9092")

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleConn(conn)
	}
}

func handleConn(conn net.Conn) {
	defer conn.Close()

	// Read request
	header := make([]byte, 4)
	_, err := io.ReadFull(conn, header)
	if err != nil {
		return
	}

	size := binary.BigEndian.Uint32(header)
	payload := make([]byte, size)
	_, err = io.ReadFull(conn, payload)
	if err != nil {
		return
	}

	// Parse correlation ID
	if len(payload) >= 8 {
		apiKey := binary.BigEndian.Uint16(payload[0:2])
		apiVersion := binary.BigEndian.Uint16(payload[2:4])
		corrID := binary.BigEndian.Uint32(payload[4:8])
		fmt.Printf("[Broker] Received: ApiKey=%d, Version=%d, CorrID=%d\n", apiKey, apiVersion, corrID)

		// Send a simple response with the same correlation ID
		resp := make([]byte, 8)
		binary.BigEndian.PutUint32(resp[0:4], 4)      // size
		binary.BigEndian.PutUint32(resp[4:8], corrID) // correlation ID
		conn.Write(resp)
		fmt.Printf("[Broker] Sent response for CorrID=%d\n", corrID)
	}
}
