//go:build linux

package grpc

import (
	"fmt"

	"google.golang.org/protobuf/proto"
)

const rawCodecName = "keploy-raw"

// rawCodec is a gRPC codec that passes byte slices through without any serialization.
// This is crucial for proxying or mocking requests when we don't have the .proto definitions.
type rawCodec struct{}

// rawMessage is a wrapper for byte slices to satisfy the proto.Message interface.
type rawMessage struct {
	data []byte
}

func (m *rawMessage) Reset()         { *m = rawMessage{} }
func (m *rawMessage) String() string { return string(m.data) }
func (*rawMessage) ProtoMessage()    {}

func (c *rawCodec) Marshal(v interface{}) ([]byte, error) {
	// Marshal the rawMessage wrapper to its underlying byte slice.
	if rm, ok := v.(*rawMessage); ok {
		return rm.data, nil
	}
	// Fallback for other types, though we primarily use rawMessage.
	if p, ok := v.(proto.Message); ok {
		return proto.Marshal(p)
	}
	return nil, fmt.Errorf("failed to marshal, message is %T, want proto.Message", v)
}

func (c *rawCodec) Unmarshal(data []byte, v interface{}) error {
	// Unmarshal the byte slice into the rawMessage wrapper.
	if rm, ok := v.(*rawMessage); ok {
		rm.data = make([]byte, len(data))
		copy(rm.data, data)
		return nil
	}
	// Fallback for other types.
	if p, ok := v.(proto.Message); ok {
		return proto.Unmarshal(data, p)
	}
	return fmt.Errorf("failed to unmarshal, message is %T, want proto.Message", v)
}

func (c *rawCodec) Name() string {
	return rawCodecName
}

func (c *rawCodec) String() string {
	return c.Name()
}

// passthroughCodec keeps 'proto' on the wire but avoids re-encoding.
type passthroughCodec struct{}

func (passthroughCodec) Name() string { return "proto" } // server already knows this one
func (passthroughCodec) Marshal(v interface{}) ([]byte, error) {
	if m, ok := v.(*rawMessage); ok {
		return m.data, nil // send bytes exactly as we received them
	}
	return proto.Marshal(v.(proto.Message))
}
func (passthroughCodec) Unmarshal(data []byte, v interface{}) error {
	if m, ok := v.(*rawMessage); ok {
		m.data = append([]byte(nil), data...)
		return nil
	}
	return proto.Unmarshal(data, v.(proto.Message))
}
