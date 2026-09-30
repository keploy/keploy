package conn

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

// An AuthNextFactor request decodes to what was encoded. The plugin data was
// sliced from the plugin name's bytes past their end, so every request with a
// terminated plugin name panicked the decoder.
func TestAuthNextFactor_RoundTrip(t *testing.T) {
	ctx := context.Background()
	for _, want := range []mysql.AuthNextFactorPacket{
		{PacketType: 0x02, PluginName: "authentication_fido_client", PluginData: "\x01challenge"},
		{PacketType: 0x02, PluginName: "auth_ldap", PluginData: ""},
	} {
		b, err := EncodeAuthNextFactor(ctx, &want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := func() (p *mysql.AuthNextFactorPacket, err error) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%q: DecodeAuthNextFactor panicked: %v", want.PluginName, r)
				}
			}()
			return DecodeAuthNextFactor(ctx, b)
		}()
		if err != nil || *got != want {
			t.Fatalf("decoded %+v, %v; want %+v", got, err, want)
		}
	}
	for _, b := range [][]byte{nil, {0x02}, {0x02, 'a', 'b'}} {
		if got, err := DecodeAuthNextFactor(ctx, b); err == nil {
			t.Fatalf("% x decoded as %+v", b, got)
		}
	}
}
