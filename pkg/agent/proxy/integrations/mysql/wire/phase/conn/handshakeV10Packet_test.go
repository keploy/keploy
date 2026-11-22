package conn

import (
	"context"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// A truncated greeting must fail to decode, not panic. The recorder decodes a
// greeting it fetched from the server inside a shared (singleflight) fetch,
// where a panic cannot be recovered and takes the whole agent down; before the
// bounds fix any greeting cut off in the 18 fixed bytes after the filler did
// exactly that.
func TestDecodeHandshakeV10_TruncatedNeverPanics(t *testing.T) {
	full, err := EncodeHandshakeV10(context.Background(), zap.NewNop(), &mysql.HandshakeV10Packet{
		ProtocolVersion: mysql.HandshakeV10,
		ServerVersion:   "8.0.36",
		ConnectionID:    7,
		AuthPluginData:  []byte("abcdefghijklmnopqrst"),
		CapabilityFlags: mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_PROTOCOL_41,
		CharacterSet:    0xff,
		StatusFlags:     2,
		AuthPluginName:  "caching_sha2_password",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := DecodeHandshakeV10(context.Background(), zap.NewNop(), full); err != nil {
		t.Fatalf("the untruncated greeting does not decode: %v", err)
	}
	for n := 0; n < len(full); n++ {
		t.Run(fmt.Sprintf("first_%d_of_%d_bytes", n, len(full)), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("DecodeHandshakeV10 panicked on a %d-byte greeting: %v", n, r)
				}
			}()
			_, _ = DecodeHandshakeV10(context.Background(), zap.NewNop(), full[:n])
		})
	}
}
