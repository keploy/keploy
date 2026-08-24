package replayer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// TestMatchQuery_AgainstRecordedCorpus drives a whole recorded MySQL mock pool
// through the real matcher and checks that every statement resolves to its OWN
// recording — not to a different statement's, and not to nothing.
//
// Unit tests pin individual rules; this pins the property that matters in
// aggregate, against real traffic. It exists because the failure it guards is
// invisible one case at a time: when a matcher falls back to a signal that is
// not the statement's identity (byte length, a type-only parse shape), each
// individual answer still looks like a plausible result set, and only a pool-
// wide run shows that most of them belong to some other query.
//
// Skipped unless KEPLOY_MYSQL_QUERY_CORPUS points at a JSON array of the
// COM_QUERY texts from a mocks.yaml:
//
//	grep -oP '^\s*query:\s*\K.*' mocks.yaml | jq -Rs 'split("\n")|map(select(.!=""))'
//
// Point it at a recording whose client injects an observability prologue
// (Datadog DBM, sqlcommenter, OpenTelemetry) — that is what defeats
// identity-by-raw-text and what this guards.
func TestMatchQuery_AgainstRecordedCorpus(t *testing.T) {
	path := os.Getenv("KEPLOY_MYSQL_QUERY_CORPUS")
	if path == "" {
		t.Skip("set KEPLOY_MYSQL_QUERY_CORPUS to a JSON array of recorded COM_QUERY texts")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var recorded []string
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if len(recorded) == 0 {
		t.Fatal("corpus is empty")
	}

	pool := make([]mysql.PacketBundle, 0, len(recorded))
	for _, q := range recorded {
		pool = append(pool, queryBundle(q))
	}

	logger := zap.NewNop()
	ctx := context.Background()

	// Mirrors matchCommand's COM_QUERY selection: an exact match wins
	// immediately, otherwise the highest score with strict-greater replacement,
	// and score 0 is not a candidate at all.
	pick := func(live mysql.PacketBundle) int {
		best, bestScore := -1, 0
		for j := range pool {
			ok, score := matchQueryPacket(ctx, logger, pool[j], live)
			if ok {
				return j
			}
			if score > bestScore {
				best, bestScore = j, score
			}
		}
		return best
	}

	var crossServed, unmatched []string
	for _, q := range recorded {
		j := pick(queryBundle(replayVariant(q)))
		switch {
		case j < 0:
			unmatched = append(unmatched, truncate(sqlStatementIdentity(q), 90))
		case oracleStatement(recorded[j]) == oracleStatement(q):
			// Resolved to its own statement. Judged by an INDEPENDENT normaliser,
			// deliberately: using sqlStatementIdentity as the oracle would let any
			// identity collapse inside it — the very failure this file exists to
			// catch — report itself as a success. Raw text equality will not do
			// either, because the same statement is recorded many times, each with
			// its own traceparent.
		default:
			crossServed = append(crossServed, fmt.Sprintf("asked %q -> served %q",
				truncate(sqlStatementIdentity(q), 90), truncate(sqlStatementIdentity(recorded[j]), 90)))
		}
	}

	t.Logf("corpus: %d statements, %d cross-served, %d unmatched",
		len(recorded), len(crossServed), len(unmatched))

	if n := len(crossServed); n > 0 {
		t.Errorf("%d statement(s) were answered with a DIFFERENT statement's recorded response:\n  %s",
			n, strings.Join(capped(crossServed, 5), "\n  "))
	}
	if n := len(unmatched); n > 0 {
		t.Errorf("%d statement(s) matched nothing, so replay would tear the connection down:\n  %s",
			n, strings.Join(capped(unmatched, 5), "\n  "))
	}
}

// oracleStatement is a deliberately naive, independently written comment
// stripper used only to judge the corpus run. It does not share a line of code
// with stripInertSQLComments, so the two agreeing is real evidence; it is not
// quote-aware, which is fine for judging recorded production SQL and is why it
// is confined to this test.
func oracleStatement(sql string) string {
	out := oracleBlockComment.ReplaceAllString(sql, " ")
	out = oracleLineComment.ReplaceAllString(out, " ")
	return strings.Join(strings.Fields(out), " ")
}

var (
	// Non-executable block comments only: "/*!" and "/*+" are the server's.
	oracleBlockComment = regexp.MustCompile(`(?s)/\*[^!+].*?\*/|/\*\*/`)
	oracleLineComment  = regexp.MustCompile(`(?m)(--[ \t][^\n]*|#[^\n]*)`)
)

func capped(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(s[:n:n], fmt.Sprintf("... and %d more", len(s)-n))
}

var traceparentRe = regexp.MustCompile(`traceparent='[^']*'`)

const (
	writerHostMarker = "cluster-"
	readerHostMarker = "cluster-ro-"
)

// replayVariant turns a recorded query into the text the SAME statement would
// carry on a later run: a fresh traceparent, and the other cluster endpoint.
// Those are the two ways an observability prologue differs between runs — one
// changes its content, the other also changes its length.
func replayVariant(q string) string {
	end := strings.Index(q, "*/")
	if !strings.HasPrefix(strings.TrimSpace(q), "/*") || end < 0 {
		return q
	}
	prologue, body := q[:end+2], q[end+2:]
	prologue = traceparentRe.ReplaceAllString(prologue,
		"traceparent='00-11112222333344445555666677778888-9999aaaabbbbcccc-01'")
	switch {
	case strings.Contains(prologue, readerHostMarker):
		prologue = strings.Replace(prologue, readerHostMarker, writerHostMarker, 1)
	case strings.Contains(prologue, writerHostMarker):
		prologue = strings.Replace(prologue, writerHostMarker, readerHostMarker, 1)
	}
	return prologue + body
}
