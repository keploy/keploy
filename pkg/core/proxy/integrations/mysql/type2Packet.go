package mysql

type PacketType2 struct {
	Field1 byte `yaml:"field1"`
	Field2 byte `yaml:"field2"`
	Field3 byte `yaml:"field3"`
}

//func decodePacketType2(data []byte) (*PacketType2, error) {
//	if len(data) < 3 {
//		return nil, fmt.Errorf("PacketType2 too short")
//	}
//
//	return &PacketType2{
//		Field1: data[0],
//		Field2: data[1],
//		Field3: data[2],
//	}, nil
//}
