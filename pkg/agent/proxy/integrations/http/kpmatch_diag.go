package http

import (
	"bytes"
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// kpMatch is the KPMATCH diagnostic of one call's match under the proven
// switch: what the matcher compared it with, by where exact serving showed
// each recording from (the open test's own, the pool slice, the worker scope
// reached, none of those), the session mocks, how many went to the variable
// pass and to the strict pass, and which pass answered. Names and counts
// only, no value.
type kpMatch struct {
	start      time.Time
	call       string
	body       int
	attempts   int
	partsKnown bool
	test, key  string
	own, pool  int
	worker     int
	other      int
	session    int
	withVars   int
	strict     int
	pass       string
}

type kpMatchKey struct{}

// kpOf is the KPMATCH diagnostic a match's context carries, nil without one.
func kpOf(ctx context.Context) *kpMatch {
	kp, _ := ctx.Value(kpMatchKey{}).(*kpMatch)
	return kp
}

// kpCallName names a call by its GraphQL operation, else by method and path.
func kpCallName(input *req) string {
	head := input.body
	if len(head) > 4096 {
		head = head[:4096]
	}
	if i := bytes.Index(head, []byte(`"operationName":"`)); i >= 0 {
		rest := head[i+len(`"operationName":"`):]
		if j := bytes.IndexByte(rest, '"'); j > 0 {
			return string(rest[:j])
		}
	}
	path := ""
	if input.url != nil {
		path = input.url.Path
	}
	return input.method + " " + path
}

// attempt notes one attempt's pools: its per-test mocks by where exact
// serving shows them from, and its session mocks.
func (k *kpMatch) attempt(mockDb integrations.MockMemDb, perTest, session []*models.Mock) {
	k.attempts++
	k.session = len(session)
	if r, ok := mockDb.(integrations.ExactPoolPartsReader); ok {
		k.test, k.key, k.own, k.pool, k.worker, k.other, k.partsKnown = r.ExactPoolParts(perTest)
		return
	}
	k.other, k.partsKnown = len(perTest), false
}

func (k *kpMatch) log(l *zap.Logger, served bool) {
	l.Info("KPMATCH", zap.String("call", k.call), zap.Int("body", k.body), zap.String("test", k.test), zap.String("key", k.key),
		zap.Int("own", k.own), zap.Int("pool", k.pool), zap.Int("worker", k.worker), zap.Int("other", k.other), zap.Int("session", k.session),
		zap.Bool("parts_known", k.partsKnown), zap.Int("with_vars", k.withVars), zap.Int("strict", k.strict),
		zap.Bool("served", served), zap.String("pass", k.pass), zap.Int("attempts", k.attempts),
		zap.Float64("ms", float64(time.Since(k.start).Microseconds())/1000))
}
