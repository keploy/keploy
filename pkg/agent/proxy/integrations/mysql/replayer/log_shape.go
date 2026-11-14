package replayer

import (
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// maxLoggedSQL is the most of a statement's shape a log line shows.
const maxLoggedSQL = 300

// sqlLogShape is sql as a log line shows it: its keywords, identifiers and
// operators, with every literal (quoted either way, numeric, hex) written ?
// and every comment's text left out, at most maxLoggedSQL bytes of it. A
// statement's literals are the application's data (a password in CREATE
// USER, an email in a WHERE, a token in an INSERT), and its comments can
// carry a trace's or a tenant's ids. The agent's log reaches a log store, so
// no line shows them, at any level, whatever a recording redacted: the
// statement's shape says which statement it is, and the miss's report, not
// the log, is where a user reads their own statement.
func sqlLogShape(sql string) string {
	var b strings.Builder
	b.Grow(min(len(sql), maxLoggedSQL))
	for i := 0; i < len(sql) && b.Len() < maxLoggedSQL; {
		c := sql[i]
		switch {
		// A backquoted identifier is a name, not a value.
		case c == '`':
			j := scanQuoted(sql, i, '`')
			if j == len(sql) && (j-i < 2 || sql[j-1] != '`') {
				// Never closed: what follows is no name keploy can
				// tell from a literal, so none of it is shown.
				b.WriteString("`?")
				i = j
				continue
			}
			b.WriteString(sql[i:j])
			i = j
		// A double-quoted token is a string literal unless ANSI_QUOTES is
		// on, which nothing on the wire says: a log line takes it for one.
		case c == '\'' || c == '"':
			j := scanQuoted(sql, i, c)
			b.WriteByte('?')
			if strings.Contains(sql[i:j], "\\"+string(c)) {
				// A backslash before the quote: an escape, or, under
				// NO_BACKSLASH_ESCAPES, the literal's end, with a literal
				// after it the scan would take for words. Which, nothing on
				// the wire says: none of the rest is shown.
				i = len(sql)
				b.WriteString("...")
				continue
			}
			i = j
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = len(sql)
			}
			b.WriteString("/*?*/")
		case c == '#' || c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			if end := strings.IndexByte(sql[i:], '\n'); end >= 0 {
				i += end
			} else {
				i = len(sql)
			}
			b.WriteString("-- ?")
		case isNumericLiteralStart(sql, i):
			j := scanNumericLiteral(sql, i)
			b.WriteByte('?')
			// A digit run that runs into an identifier's bytes is part of
			// a name or a malformed token: the rest of it is left out.
			for j < len(sql) && isIdentByte(sql[j]) {
				j++
			}
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	out := b.String()
	if len(out) > maxLoggedSQL {
		out = out[:maxLoggedSQL]
	}
	if b.Len() >= maxLoggedSQL {
		out += "..."
	}
	return out
}

// liveCommandShape is the live command msg as a log line shows it: its
// statement's shape (sqlLogShape), and for an EXECUTE its prepared
// statement's shape and how many values it bound, never the values.
func liveCommandShape(msg any, decodeCtx *wire.DecodeContext) string {
	switch msg := msg.(type) {
	case *mysql.QueryPacket:
		if msg != nil {
			return sqlLogShape(msg.Query)
		}
	case *mysql.StmtPreparePacket:
		if msg != nil {
			return sqlLogShape(msg.Query)
		}
	case *mysql.StmtExecutePacket:
		if msg != nil {
			query := ""
			if decodeCtx != nil && decodeCtx.StmtIDToQuery != nil {
				query = sqlLogShape(decodeCtx.StmtIDToQuery[msg.StatementID])
			}
			return strings.TrimSpace(fmt.Sprintf("%s (%d bound values)", query, len(msg.Parameters)))
		}
	case *mysql.StmtSendLongDataPacket:
		if msg != nil {
			return fmt.Sprintf("(param %d, %d streamed bytes)", msg.ParameterID, len(msg.Data))
		}
	}
	return ""
}

// responseKind is the kind of the recorded answer resp, as a log line names
// it; "" for none (a command that takes no answer is matched to an empty
// one).
func responseKind(resp *mysql.Response) string {
	if resp == nil || resp.Header == nil {
		return ""
	}
	return resp.Header.Type
}
