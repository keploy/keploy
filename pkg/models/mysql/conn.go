package mysql

// This file contains struct for connection phase packets
// refer: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets.html

// Initial Handshake Packets

// HandshakeV10Packet represents the initial handshake packet sent by the server to the client
type HandshakeV10Packet struct {
	ProtocolVersion uint8  `yaml:"protocol_version" json:"protocol_version"`
	ServerVersion   string `yaml:"server_version" json:"server_version"`
	ConnectionID    uint32 `yaml:"connection_id" json:"connection_id"`
	AuthPluginData  []byte `yaml:"auth_plugin_data,omitempty,flow" json:"auth_plugin_data,omitempty"`
	Filler          byte   `yaml:"filler" json:"filler"`
	CapabilityFlags uint32 `yaml:"capability_flags" json:"capability_flags"`
	CharacterSet    uint8  `yaml:"character_set" json:"character_set"`
	StatusFlags     uint16 `yaml:"status_flags" json:"status_flags"`
	AuthPluginName  string `yaml:"auth_plugin_name" json:"auth_plugin_name"`
}

// HandshakeResponse41Packet represents the response packet sent by the client to the server after receiving the HandshakeV10Packet
type HandshakeResponse41Packet struct {
	CapabilityFlags      uint32            `yaml:"capability_flags" json:"capability_flags"`
	MaxPacketSize        uint32            `yaml:"max_packet_size" json:"max_packet_size"`
	CharacterSet         uint8             `yaml:"character_set" json:"character_set"`
	Filler               [23]byte          `yaml:"filler,omitempty,flow" json:"filler,omitempty"`
	Username             string            `yaml:"username" json:"username"`
	AuthResponse         []byte            `yaml:"auth_response,omitempty,flow" json:"auth_response,omitempty"`
	Database             string            `yaml:"database" json:"database"`
	AuthPluginName       string            `yaml:"auth_plugin_name" json:"auth_plugin_name"`
	ConnectionAttributes map[string]string `yaml:"connection_attributes,omitempty" json:"connection_attributes,omitempty"`
	ZstdCompressionLevel byte              `yaml:"zstdcompressionlevel" json:"zstdcompressionlevel"`
}

// Authentication Packets

// AuthSwitchRequestPacket represents the packet sent by the server to the client to switch to a different authentication method
type AuthSwitchRequestPacket struct {
	StatusTag  byte   `yaml:"status_tag" json:"status_tag"`
	PluginName string `yaml:"plugin_name" json:"plugin_name"`
	PluginData string `yaml:"plugin_data" json:"plugin_data"`
}

// AuthSwitchResponsePacket represents the packet sent by the client to the server in response to an AuthSwitchRequestPacket.
// Note: If the server sends an AuthMoreDataPacket, the client will continue sending AuthSwitchResponsePackets until the server sends an OK packet or an ERR packet.
type AuthSwitchResponsePacket struct {
	Data string `yaml:"data" json:"data"`
}

// AuthMoreDataPacket represents the packet sent by the server to the client to request additional data for authentication
type AuthMoreDataPacket struct {
	StatusTag byte   `yaml:"status_tag" json:"status_tag"`
	Data      string `yaml:"data" json:"data"`
}

// AuthNextFactorPacket represents the packet sent by the server to the client to request the next factor for multi-factor authentication
type AuthNextFactorPacket struct {
	PacketType byte   `yaml:"packet_type" json:"packet_type"`
	PluginName string `yaml:"plugin_name" json:"plugin_name"`
	PluginData string `yaml:"plugin_data" json:"plugin_data"`
}
