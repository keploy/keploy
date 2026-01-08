package postgres

import (
	"encoding/gob"

	// Import platform-independent gob registrations for PostgresV2 wire types
	// This ensures macOS/Windows hosts can decode mocks streamed from Linux containers
	_ "github.com/keploy/integrations/pkg/postgres/v2/types"
)

// register.go registers all postgres-related structs used inside interface
// fields so gob can encode/decode them when transmitted inside interface{}.
func init() {
	gob.Register(&Spec{})
	gob.Register(&RequestYaml{})
	gob.Register(&ResponseYaml{})
	gob.Register(&PacketInfo{})
	gob.Register(&Request{})
	gob.Register(&Response{})
	gob.Register(&PacketBundle{})
	gob.Register(&Packet{})
	gob.Register(&Header{})
}
