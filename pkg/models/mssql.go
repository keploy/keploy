package models


// TDSPacketHeader is a struct that represents the header of a TDS packet.
type TDSPacketHeader struct {
	PacketType byte
	Status     byte
	Length     uint16
	SPID       uint16
	Sequence   byte
	Window     byte
}

type TDSPacket struct {
	Header TDSPacketHeader
	Data   interface{}
}

