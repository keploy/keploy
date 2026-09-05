package postgres

import (
	"encoding/gob"
)

// register.go pins the gob wire names for postgres packet types.
func init() {
	// gob.RegisterName with an EXPLICIT wire name, not gob.Register, so the name
	// gob writes into every mocks.gob (and the interface payloads streamed
	// between the keploy binaries) is fixed here rather than derived from the Go
	// package path and type name. gob.Register(&T{}) keys the wire on the live
	// identifier, so renaming or moving one of these types silently changes the
	// on-disk format and makes previously recorded mocks fail to decode — the
	// stability gap env-vars.md flags for the gob format. Each literal below is
	// exactly the name gob.Register produced before, so this is byte-identical on
	// the wire (verified by the round-trip + golden tests in gob_register_test.go)
	// and existing mocks keep decoding; the difference is only that a future
	// rename can no longer move it. Frozen: change a literal only with intent, and
	// update the golden test.
	gob.RegisterName("*postgres.Spec", &Spec{})
	gob.RegisterName("*postgres.RequestYaml", &RequestYaml{})
	gob.RegisterName("*postgres.ResponseYaml", &ResponseYaml{})
	gob.RegisterName("*postgres.PacketInfo", &PacketInfo{})
	gob.RegisterName("*postgres.Request", &Request{})
	gob.RegisterName("*postgres.Response", &Response{})
	gob.RegisterName("*postgres.PacketBundle", &PacketBundle{})
	gob.RegisterName("*postgres.Packet", &Packet{})
	gob.RegisterName("*postgres.Header", &Header{})
}
