package http

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AWS Signature Version 4 under the proven switch (founder decision 25): an
// AWS SDK signs every call over the request and the time it is made, so a
// replay's signature is never the recording's, and an Authorization header
// compared whole misses every AWS call. The strict comparison reads it by its
// grammar (AWS SigV4, "Authorization header" and "query string" forms) and
// compares its parts:
//
//	AWS4-HMAC-SHA256 Credential=<key id>/<date>/<region>/<service>/<terminator>, SignedHeaders=<h1;h2;...>, Signature=<64 hex>
//
//   - the algorithm, the key id, the region, the service, the terminator
//     (aws4_request) and the signed headers exactly: another account, region,
//     service or set of signed headers is another call;
//   - the date by the clock: it is the day the request was signed on, which
//     the clock the test runs on decides (design v14 §3.4: frozen at its
//     recorded time, or real time for a REAL test or a shape keploy cannot
//     freeze), so it may differ from the recording's only when each side's
//     date is the day of its own signing time (X-Amz-Date, else Date), as
//     SigV4 requires; any other difference is compared;
//   - the signature not at all: it is a function of the rest and the time.
//
// x-amz-content-sha256, the payload's hash a signature covers, must be the
// SHA-256 of the body the client sent when the recording's is a hash (the
// body itself is compared field by field, so a learned body field may change
// it), and the recorded literal otherwise (UNSIGNED-PAYLOAD, the STREAMING-*
// forms). A presigned URL's query is read the same way: X-Amz-Credential as
// the credential, X-Amz-Signature not compared, X-Amz-Date as the signing
// time; every other parameter as any query value.
//
// Nothing is guessed from a value's shape: a value that does not parse by
// this grammar on either side, or another scheme (Bearer, SigV4A, Azure
// SharedKey), is compared whole, as before.

// sigV4Algorithm is the algorithm, and scheme, of AWS Signature Version 4.
const sigV4Algorithm = "AWS4-HMAC-SHA256"

// sigV4Scope is a SigV4 credential: <key id>/<date>/<region>/<service>/<terminator>.
type sigV4Scope struct {
	keyID, date, region, service, terminator string
}

// parseSigV4Scope reads a credential: five non-empty parts, the date as
// YYYYMMDD.
func parseSigV4Scope(s string) (sigV4Scope, bool) {
	p := strings.Split(s, "/")
	if len(p) != 5 {
		return sigV4Scope{}, false
	}
	for _, x := range p {
		if x == "" {
			return sigV4Scope{}, false
		}
	}
	if _, err := time.Parse("20060102", p[1]); err != nil {
		return sigV4Scope{}, false
	}
	return sigV4Scope{keyID: p[0], date: p[1], region: p[2], service: p[3], terminator: p[4]}, true
}

// sigV4Auth is a SigV4 Authorization header's parts.
type sigV4Auth struct {
	scope         sigV4Scope
	signedHeaders string
}

// parseSigV4Authorization reads an Authorization header of SigV4: the
// algorithm, then Credential, SignedHeaders and Signature, each once, in any
// order, comma separated.
func parseSigV4Authorization(v string) (sigV4Auth, bool) {
	alg, rest, ok := strings.Cut(v, " ")
	if !ok || alg != sigV4Algorithm {
		return sigV4Auth{}, false
	}
	var a sigV4Auth
	seen := map[string]bool{}
	for _, kv := range strings.Split(rest, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || val == "" || seen[k] {
			return sigV4Auth{}, false
		}
		seen[k] = true
		switch k {
		case "Credential":
			if a.scope, ok = parseSigV4Scope(val); !ok {
				return sigV4Auth{}, false
			}
		case "SignedHeaders":
			a.signedHeaders = val
		case "Signature":
			if !isSHA256Hex(val) {
				return sigV4Auth{}, false
			}
		default:
			return sigV4Auth{}, false
		}
	}
	return a, len(seen) == 3
}

// isSHA256Hex reports whether s is a SHA-256 as SigV4 writes one: 64
// lowercase hex digits.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sigV4Day is the day of a SigV4 signing time: an X-Amz-Date (ISO 8601
// basic, YYYYMMDD'T'HHMMSS'Z'), else an HTTP Date header, in UTC; "" when
// neither says one.
func sigV4Day(amzDate, date string) string {
	if t, err := time.Parse("20060102T150405Z", amzDate); err == nil {
		return t.Format("20060102")
	}
	if t, err := http.ParseTime(date); err == nil {
		return t.UTC().Format("20060102")
	}
	return ""
}

// sigV4ScopeDiffs are the parts in which a live credential differs from the
// recorded one, named at prefix: the key id, region, service and terminator
// exactly; the date only where it is not each side's own signing day (recDay,
// liveDay: sigV4Day).
func sigV4ScopeDiffs(prefix string, rec, live sigV4Scope, recDay, liveDay string) []strictField {
	var out []strictField
	part := func(name, r, l string) {
		if r != l {
			out = append(out, field(valueDiff(prefix+"."+name, r, l)))
		}
	}
	part("access_key_id", rec.keyID, live.keyID)
	if rec.date != live.date && (recDay == "" || liveDay == "" || rec.date != recDay || live.date != liveDay) {
		part("date", rec.date, live.date)
	}
	part("region", rec.region, live.region)
	part("service", rec.service, live.service)
	part("terminator", rec.terminator, live.terminator)
	return out
}

// sigV4AuthorizationDiffs compares a recorded Authorization header with the
// live one (its lines joined with ",") by their SigV4 parts
// (header.authorization.<part>); ok is false when either is not SigV4, and
// the header is then compared whole, as any header. recDay and liveDay are
// each side's signing day (sigV4Day).
func sigV4AuthorizationDiffs(rec, live, recDay, liveDay string) (out []strictField, ok bool) {
	ra, rok := parseSigV4Authorization(rec)
	la, lok := parseSigV4Authorization(live)
	if !rok || !lok {
		return nil, false
	}
	out = sigV4ScopeDiffs("header.authorization", ra.scope, la.scope, recDay, liveDay)
	if ra.signedHeaders != la.signedHeaders {
		out = append(out, field(valueDiff("header.authorization.signed_headers", ra.signedHeaders, la.signedHeaders)))
	}
	return out, true
}

// samePayloadHash reports whether a live x-amz-content-sha256 is the recorded
// one: the SHA-256 of the body the client sent (sent) when the recorded value
// is a hash, the recorded literal otherwise (UNSIGNED-PAYLOAD, STREAMING-*).
func samePayloadHash(rec string, live []string, sent []byte) bool {
	lv := strings.Join(live, ",")
	if !isSHA256Hex(rec) {
		return rec == lv
	}
	sum := sha256.Sum256(sent)
	return lv == hex.EncodeToString(sum[:])
}

// presignedSigV4 is a SigV4 presigned URL's signing parameters.
type presignedSigV4 struct {
	scope sigV4Scope
	day   string
}

// The parameters of a SigV4 presigned URL that are read by its grammar.
const (
	amzAlgorithm  = "X-Amz-Algorithm"
	amzCredential = "X-Amz-Credential"
	amzDate       = "X-Amz-Date"
	amzSignature  = "X-Amz-Signature"
)

// parsePresignedSigV4 reads a query as a SigV4 presigned URL's: X-Amz-Algorithm
// AWS4-HMAC-SHA256, and one X-Amz-Credential, X-Amz-Date and X-Amz-Signature
// that parse.
func parsePresignedSigV4(q url.Values) (presignedSigV4, bool) {
	one := func(name string) (string, bool) {
		v := q[name]
		return firstOf(v), len(v) == 1
	}
	if alg, ok := one(amzAlgorithm); !ok || alg != sigV4Algorithm {
		return presignedSigV4{}, false
	}
	cred, ok := one(amzCredential)
	if !ok {
		return presignedSigV4{}, false
	}
	scope, ok := parseSigV4Scope(cred)
	if !ok {
		return presignedSigV4{}, false
	}
	date, ok := one(amzDate)
	if !ok {
		return presignedSigV4{}, false
	}
	day := sigV4Day(date, "")
	if day == "" {
		return presignedSigV4{}, false
	}
	if sig, ok := one(amzSignature); !ok || !isSHA256Hex(sig) {
		return presignedSigV4{}, false
	}
	return presignedSigV4{scope: scope, day: day}, true
}

func firstOf(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// presignedDiffs compares the signing parameters of two presigned URLs
// (parsePresignedSigV4): the credential by its parts
// (query.X-Amz-Credential.<part>), the signature and the signing time not at
// all. byGrammar names the parameters it read, which the query comparison
// does not compare again; nil when either query is no presigned URL's, and
// the query is compared value by value, as before.
func presignedDiffs(rec, live url.Values) (byGrammar map[string]bool, out []strictField) {
	rp, rok := parsePresignedSigV4(rec)
	lp, lok := parsePresignedSigV4(live)
	if !rok || !lok {
		return nil, nil
	}
	return map[string]bool{amzCredential: true, amzDate: true, amzSignature: true},
		sigV4ScopeDiffs("query."+amzCredential, rp.scope, lp.scope, rp.day, lp.day)
}

// sigV4Days are the signing days of a recorded and a live request (sigV4Day),
// from their X-Amz-Date or Date headers: recH holds the recorded headers by
// lowercased name, liveH the live ones.
func sigV4Days(recH map[string]string, liveH map[string][]string) (string, string) {
	return sigV4Day(recH["x-amz-date"], recH["date"]), sigV4Day(strings.Join(liveH["x-amz-date"], ","), strings.Join(liveH["date"], ","))
}
