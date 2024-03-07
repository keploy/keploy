package mysql

type PacketType2 struct {
	Field1 byte `json:"field1,omitempty" yaml:"field1,omitempty,flow"`
	Field2 byte `json:"field2,omitempty" yaml:"field2,omitempty,flow"`
	Field3 byte `json:"field3,omitempty" yaml:"field3,omitempty,flow"`
}
