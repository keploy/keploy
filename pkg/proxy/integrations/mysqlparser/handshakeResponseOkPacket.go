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

	switch data[0] {
	case iOK:
		packetIndicator = "OK"
	case iAuthMoreData:
		packetIndicator = "AuthMoreData"
	case iEOF:
		packetIndicator = "EOF"
	default:
		packetIndicator = "Unknown"
	}

	if data[0] == iAuthMoreData {
		count := int(data[0])
		var authData = data[1 : count+1]
		switch handshakePluginName {
		case "caching_sha2_password":
			switch len(authData) {
			case 1:
				switch authData[0] {
				case cachingSha2PasswordFastAuthSuccess:
					authType = "cachingSha2PasswordFastAuthSuccess"
					message = "Ok"
					remainingBytes = data[count+1:]
				case cachingSha2PasswordPerformFullAuthentication:
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

	var packetIndicator byte
	switch packet.PacketIndicator {
	case "OK":
		packetIndicator = iOK
	case "AuthMoreData":
		packetIndicator = iAuthMoreData
	case "EOF":
		packetIndicator = iEOF
	default:
		return nil, fmt.Errorf("unknown packet indicator")
	}

	buf.WriteByte(packetIndicator)

	if packet.PacketIndicator == "AuthMoreData" {
		var authData byte
		switch packet.PluginDetails.Type {
		case "cachingSha2PasswordFastAuthSuccess":
			authData = cachingSha2PasswordFastAuthSuccess
		case "cachingSha2PasswordPerformFullAuthentication":
			authData = cachingSha2PasswordPerformFullAuthentication
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
	payload := append(header, buf.Bytes()...)

	return payload, nil
}
