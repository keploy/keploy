package generic

import (
	"hash/maphash"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
)

// ownExchanges is the index of the open test's own Generic recordings that
// the own-call check reads (integrations.OwnRepeatRead): each filed under the
// hash of the bytes it recorded (recordedKey), so a call reads only the
// recordings of its own bytes (sentKey), never every exchange the test owns.
var ownExchanges = integrations.NewOwnIndex(models.GENERIC, recordedKey)

// exchangeSeed is the seed of every exchange key, so a recording's key and a
// call's agree.
var exchangeSeed = maphash.MakeSeed()

// sentKey is the key of the bytes a connection sent for one exchange, in the
// pieces its reads returned: the hash of the pieces joined. An exchange the
// matcher finds byte for byte in a recording (findExactMatch with exact) sent
// the recording's bytes, cut where they are or where the recording's pieces
// end, so it has the recording's key.
func sentKey(pieces [][]byte) uint64 {
	var h maphash.Hash
	h.SetSeed(exchangeSeed)
	for _, p := range pieces {
		_, _ = h.Write(p)
	}
	return h.Sum64()
}

// recordedKey is a recording's key: the hash of its request pieces' bytes,
// joined. False for a recording a piece of which no bytes can be
// (isRecordedBytes: not one message, of a type that is neither text nor
// base64, or base64 that does not decode): no call is that recording byte for
// byte.
func recordedKey(mk *models.Mock) (uint64, bool) {
	var h maphash.Hash
	h.SetSeed(exchangeSeed)
	for _, p := range mk.Spec.GenericRequests {
		if len(p.Message) != 1 {
			return 0, false
		}
		msg := p.Message[0]
		switch msg.Type {
		case models.String:
			_, _ = h.WriteString(msg.Data)
		case binaryPayload:
			b, err := util.DecodeBase64(msg.Data)
			if err != nil {
				return 0, false
			}
			_, _ = h.Write(b)
		default:
			return 0, false
		}
	}
	return h.Sum64(), true
}
