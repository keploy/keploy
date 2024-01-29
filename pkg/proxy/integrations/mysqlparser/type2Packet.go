package mysqlparser

type PacketType2 struct {
	Field1 byte `json:"field1,omitempty" yaml:"field1,omitempty,flow"`
	Field2 byte `json:"field2,omitempty" yaml:"field2,omitempty,flow"`
	Field3 byte `json:"field3,omitempty" yaml:"field3,omitempty,flow"`
}

// func decodePacketType2(data []byte) (*PacketType2, error) {
// 	if len(data) < 3 {
// 		return nil, fmt.Errorf("PacketType2 too short")
// 	}

// 	return &PacketType2{
// 		Field1: data[0],
// 		Field2: data[1],
// 		Field3: data[2],
// 	}, nil
// }
