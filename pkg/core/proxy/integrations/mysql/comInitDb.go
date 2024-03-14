package mysql

type ComInitDbPacket struct {
	Status byte   `json:"status,omitempty" yaml:"status,omitempty"`
	DbName string `json:"db_name,omitempty" yaml:"db_name,omitempty"`
}
