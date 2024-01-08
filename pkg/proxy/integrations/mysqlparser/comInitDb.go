package mysqlparser

import (
	"errors"
)

type ComInitDbPacket struct {
	Status byte
	DbName string
}

func decodeComInitDb(data []byte) (*ComInitDbPacket, error) {
	if len(data) < 2 {
		return nil, errors.New("data too short for COM_INIT_DB")
	}
	status := data[0]

	// The rest of the packet after the command byte is the database name
	dbName := string(data[1:])
	return &ComInitDbPacket{
		Status: status,
		DbName: dbName,
	}, nil
}
