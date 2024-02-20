package hooks

import (
	"encoding/binary"
	"errors"
	"net"
)

const mockTable string = "mock"
const configMockTable string = "configMock"
const mockTableIndex string = "id"
const configMockTableIndex string = "id"
const mockTableIndexField string = "Id"
const configMockTableIndexField string = "Id"

// ConvertIPToUint32 converts a string representation of an IPv4 address to a 32-bit integer.
func ConvertIPToUint32(ipStr string) (uint32, error) {
	ipAddr := net.ParseIP(ipStr)
	if ipAddr != nil {
		ipAddr = ipAddr.To4()
		if ipAddr != nil {
			return binary.BigEndian.Uint32(ipAddr), nil
		} else {
			return 0, errors.New("not a valid IPv4 address")
		}
	} else {
		return 0, errors.New("failed to parse IP address")
	}
}