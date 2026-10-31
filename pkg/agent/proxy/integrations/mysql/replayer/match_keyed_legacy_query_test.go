package replayer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

const legacyTraceparent = "00-a5ff4ff820b14ad28693c1b155015340-0000000000000001-01"

// attrQueryReq is a live COM_QUERY that carries one query attribute, a
// traceparent, as Connector/J sends it: decoded as this keploy reads it, and
// read the way an older keploy read it too (LegacyQuery).
func attrQueryReq(t *testing.T, tp, sql string) mysql.Request {
	t.Helper()
	data := []byte{mysql.COM_QUERY, 0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeString), 0x00, byte(len("traceparent"))}
	data = append(data, "traceparent"...)
	data = append(data, byte(len(tp)))
	data = append(data, tp...)
	data = append(data, sql...)
	p, err := query.DecodeQuery(context.Background(), zap.NewNop(), data, mysql.CLIENT_QUERY_ATTRIBUTES, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.LegacyQuery == "" {
		t.Fatalf("precondition: an older keploy reads it otherwise: %+v", p)
	}
	req := queryReq(sql)
	req.Message = p
	return req
}

func attrQueryMock(name, sql string, params []mysql.Parameter, ms int) *models.Mock {
	req := queryReq(sql)
	qp := req.Message.(*mysql.QueryPacket)
	qp.ParameterCount, qp.NewParamsBindFlag, qp.NullBitmap = 1, 1, []byte{0}
	qp.Parameters = params
	return diffMock(name, "c1", []mysql.Request{req}, diffBase.Add(time.Duration(ms)*time.Millisecond), models.LifetimeSession, "config")
}

// TestNoWindowQueryReadAsAnOlderKeployReadItServesWhatTheFullScanServes: a
// set partly re-recorded by a newer keploy holds an older keploy's recording
// of a Connector/J query that names a traceparent attribute (the attribute's
// name taken for its value, the value glued to the head of the query), then
// this keploy's recording of the same call. The full scan serves the first in
// recorded order, the older one: the live query is compared with it on the
// older keploy's reading, an identity no key of the live query names. So the
// keyed pass must leave such a query to the full scan (readsLegacy), and
// serve the same recording.
func TestNoWindowQueryReadAsAnOlderKeployReadItServesWhatTheFullScanServes(t *testing.T) {
	const sql = "SELECT 2 AS v"
	pool := []*models.Mock{
		attrQueryMock("legacy", string(rune(len(legacyTraceparent)))+legacyTraceparent+sql, []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Value: "traceparent"}}, 1),
		attrQueryMock("current", sql, []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Name: "traceparent", Value: legacyTraceparent}}, 2),
	}
	ref, fast := newDiffSide(pool), newDiffSide(pool)
	for _, s := range []*diffSide{ref, fast} {
		s.stageNoWindow()
	}
	live := attrQueryReq(t, legacyTraceparent, sql)
	want := ref.match(context.Background(), live, nil, true)
	got := fast.match(context.Background(), live, nil, false)
	if !strings.HasPrefix(want, "served legacy ") {
		t.Fatalf("precondition: the full scan %s, want the older keploy's recording", want)
	}
	if got != want {
		t.Fatalf("\n full scan:  %s\n keyed pass: %s", want, got)
	}
	ref.mm.Close()
	fast.mm.Close()
}
