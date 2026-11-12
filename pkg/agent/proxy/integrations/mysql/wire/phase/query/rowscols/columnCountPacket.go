package rowscols

import (
	"context"
	"errors"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// DecodeColumnCount decodes a result set's column count packet (its payload,
// without the header), framed as the connection negotiated. Under MariaDB's
// metadata cache (framing.MetadataFollows) the count is followed by one byte
// that says whether the column definitions follow. A server leaves them out
// only for a prepared statement whose client has them already: a recorder
// that read definitions there would read the rows as definitions, so such a
// result set is refused, with its reason, rather than recorded misframed.
func DecodeColumnCount(_ context.Context, _ *zap.Logger, data []byte, framing mysql.MetadataFraming) (uint64, error) {
	columnCount, _, n := utils.ReadLengthEncodedInteger(data)
	if n == 0 {
		return 0, errors.New("invalid column count")
	}
	if framing.MetadataFollows() {
		if len(data) <= n {
			return 0, errors.New("column count packet ends before its metadata-follows byte (MariaDB's metadata cache)")
		}
		if data[n] != 1 {
			return 0, errors.New("the result set leaves out its column definitions (MariaDB's metadata cache): keploy cannot record a result set without them")
		}
	}

	return columnCount, nil
}
