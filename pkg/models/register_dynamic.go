package models

import "encoding/gob"

// register_dynamic.go contains gob registrations for dynamic concrete types
// that are stored in interface{} fields across various protocol models (e.g.
// mysql.PacketBundle.Message, postgres.Packet.Message, mongo request/response
// Message fields, assertion maps, etc). Without registering these, gob cannot
// encode interface values whose underlying concrete type is one of these
// composite map forms and will error: "gob: type not registered for interface: map[string]interface {}".
//
// NOTE: We deliberately register a few common composite shapes that appear in
// the models to future-proof against similar failures when the underlying
// interface values hold more complex map forms (nested maps or slices of maps).
// gob ignores duplicates so these are safe.
func init() {
	gob.Register(map[string]interface{}{})            // simplest form
	gob.Register([]map[string]interface{}{})          // slice of maps
	gob.Register(map[string]map[string]interface{}{}) // nested map used in OpenAPI properties
	gob.Register([]interface{}{})                     // slice of empty-interface values
	gob.Register(map[string][]interface{}{})          // map to heterogeneous list (common in JSON specs)
}
