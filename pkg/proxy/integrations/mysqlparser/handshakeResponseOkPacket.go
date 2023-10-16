package mysqlparser

import (
	"bytes"
	"fmt"

	"go.keploy.io/server/pkg/models"
)

type HandshakeResponseOk struct {
	PacketIndicator string        `yaml:"packet_indicator"`
	PluginDetails   PluginDetails `yaml:"plugin_details"`
	RemainingBytes  []byte        `yaml:"remaining_bytes"`
}

func decodeHandshakeResponseOk(data []byte) (*HandshakeResponseOk, error) {
	var (
		packetIndicator string
		authType        string
		message         string
		remainingBytes  []byte
	)
	if isPluginData {
		publicKeyData := string(data[1:])
		authType = "PublicKeyAuthentication"
		message = "Public key for authentication"
		remainingBytes = []byte(publicKeyData)
	}

	switch data[0] {
	case models.OK:
		packetIndicator = "OK"
	case models.AuthMoreData:
		packetIndicator = "AuthMoreData"
	case models.EOF:
		packetIndicator = "EOF"
	default:
		packetIndicator = "Unknown"
	}

	if data[0] == models.AuthMoreData {
		count := int(data[0])
		var authData = data[1 : count+1]
		switch handshakePluginName {
		case "caching_sha2_password":
			switch len(authData) {
			case 1:
				switch authData[0] {
				case models.CachingSha2PasswordFastAuthSuccess:
					authType = "cachingSha2PasswordFastAuthSuccess"
					message = "Ok"
					remainingBytes = data[count+1:]
				case models.CachingSha2PasswordPerformFullAuthentication:
					authType = "cachingSha2PasswordPerformFullAuthentication"
					message = ""
					remainingBytes = data[count+1:]
				}
			}
		}
	}

	return &HandshakeResponseOk{
		PacketIndicator: packetIndicator,
		PluginDetails: PluginDetails{
			Type:    authType,
			Message: message,
		},
		RemainingBytes: remainingBytes,
	}, nil
}

func encodeHandshakeResponseOk(packet *models.MySQLHandshakeResponseOk) ([]byte, error) {
	var buf bytes.Buffer
	var payload []byte
	if packet.PluginDetails.Type == "PublicKeyAuthentication" {
		publicKeydata := []byte(packet.RemainingBytes)

		// Calculate the payload length
		payloadLength := len(publicKeydata) + 1 // +1 for the MySQL protocol version byte

		// Construct the MySQL packet header
		header := make([]byte, 4)
		header[0] = byte(payloadLength & 0xFF)         // Least significant byte
		header[1] = byte((payloadLength >> 8) & 0xFF)  // Middle byte
		header[2] = byte((payloadLength >> 16) & 0xFF) // Most significant byte
		header[3] = 4                                  // Sequence ID

		// Append the MySQL protocol version byte and the public key data to the header
		finalData := append(header, 0x01) // MySQL protocol version
		finalData = append(finalData, publicKeydata...)

		buf.Write(finalData)
		payload = buf.Bytes()
	} else {

		var packetIndicator byte
		switch packet.PacketIndicator {
		case "OK":
			packetIndicator = models.OK
		case "AuthMoreData":
			packetIndicator = models.AuthMoreData
		case "EOF":
			packetIndicator = models.EOF
		default:
			return nil, fmt.Errorf("unknown packet indicator")
		}

		buf.WriteByte(packetIndicator)

		if packet.PacketIndicator == "AuthMoreData" {
			var authData byte
			switch packet.PluginDetails.Type {
			case "cachingSha2PasswordFastAuthSuccess":
				authData = models.CachingSha2PasswordFastAuthSuccess
			case "cachingSha2PasswordPerformFullAuthentication":
				authData = models.CachingSha2PasswordPerformFullAuthentication
			default:
				return nil, fmt.Errorf("unknown auth type")
			}

			// Write auth data
			buf.WriteByte(authData)
		}

		// Write remaining bytes if available
		if len(packet.RemainingBytes) > 0 {
			buf.Write(packet.RemainingBytes)
		}

		// Create header
		header := make([]byte, 4)
		header[0] = 2 // sequence number
		header[1] = 0
		header[2] = 0
		header[3] = 2
		// Prepend header to the payload
		payload = append(header, buf.Bytes()...)
	}
	return payload, nil
}
