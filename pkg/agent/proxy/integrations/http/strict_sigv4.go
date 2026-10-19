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
//   - the algorithm, the region, the service, the terminator (aws4_request)
//     and the signed headers exactly: another region, service or set of
//     signed headers is another call;
//   - the access key id, the date and the signature not at all (founder
//     ruling on SF-3, 2026-10-19): a replay runs with the credentials of
//     where it runs (a CI's, not the recording machine's) and on the day it
//     runs, and the signature is a function of the rest and the time.
//     Compared, every replay with other AWS credentials or on another day
//     missed.
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
// recorded one, named at prefix: the region, service and terminator exactly;
// the access key id and the date not at all.
func sigV4ScopeDiffs(prefix string, rec, live sigV4Scope) []strictField {
	var out []strictField
	part := func(name, r, l string) {
		if r != l {
			out = append(out, field(valueDiff(prefix+"."+name, r, l)))
		}
	}
	part("region", rec.region, live.region)
	part("service", rec.service, live.service)
	part("terminator", rec.terminator, live.terminator)
	return out
}

// sigV4AuthorizationDiffs compares a recorded Authorization header with the
// live one (its lines joined with ",") by their SigV4 parts
// (header.authorization.<part>); ok is false when either is not SigV4, and
// the header is then compared whole, as any header.
func sigV4AuthorizationDiffs(rec, live string) (out []strictField, ok bool) {
	ra, rok := parseSigV4Authorization(rec)
	la, lok := parseSigV4Authorization(live)
	if !rok || !lok {
		return nil, false
	}
	out = sigV4ScopeDiffs("header.authorization", ra.scope, la.scope)
	if signedHeadersWithoutToken(ra.signedHeaders) != signedHeadersWithoutToken(la.signedHeaders) {
		out = append(out, field(valueDiff("header.authorization.signed_headers", ra.signedHeaders, la.signedHeaders)))
	}
	return out, true
}

// sigV4SecurityToken is the SigV4 session token's header, signed exactly when
// the credentials are temporary (an SSO session, an assumed role, IRSA).
const sigV4SecurityToken = "x-amz-security-token"

// signedHeadersWithoutToken is a SignedHeaders list without the session
// token's header: whether it is signed says what kind of credentials signed
// the call, which the credentials of where the replay runs decide, as they
// decide the access key id (SF-3, founder ruling: credential and
// environment values are never compared). Compared, a recording made with
// temporary credentials missed every replay with static ones, and the
// reverse.
func signedHeadersWithoutToken(list string) string {
	var out []string
	for _, h := range strings.Split(list, ";") {
		if h != sigV4SecurityToken {
			out = append(out, h)
		}
	}
	return strings.Join(out, ";")
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
}

// The parameters of a SigV4 presigned URL that are read by its grammar.
const (
	amzAlgorithm  = "X-Amz-Algorithm"
	amzCredential = "X-Amz-Credential"
	amzDate       = "X-Amz-Date"
	amzSignature  = "X-Amz-Signature"
	// amzSecurityToken is the session token of temporary credentials, in
	// a presigned URL's query.
	amzSecurityToken = "X-Amz-Security-Token"
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
	// Its signing time must read as one (the grammar), though it is not
	// compared.
	if sigV4Day(date, "") == "" {
		return presignedSigV4{}, false
	}
	if sig, ok := one(amzSignature); !ok || !isSHA256Hex(sig) {
		return presignedSigV4{}, false
	}
	return presignedSigV4{scope: scope}, true
}

func firstOf(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// presignedDiffs compares the signing parameters of two presigned URLs
// (parsePresignedSigV4): the credential by its parts
// (query.X-Amz-Credential.<part>), the signature, the signing time and the
// session token not at all. byGrammar names the parameters it read, which
// the query comparison does not compare again; nil when either query is no
// presigned URL's, and the query is compared value by value, as before.
func presignedDiffs(rec, live url.Values) (byGrammar map[string]bool, out []strictField) {
	rp, rok := parsePresignedSigV4(rec)
	lp, lok := parsePresignedSigV4(live)
	if !rok || !lok {
		return nil, nil
	}
	// X-Amz-Security-Token, the session token of temporary credentials, is
	// a credential: read by the grammar, never compared (SF-3), as its
	// header form is noise. Compared by value, every URL presigned with
	// temporary credentials missed, its token rotating hourly.
	return map[string]bool{amzCredential: true, amzDate: true, amzSignature: true, amzSecurityToken: true},
		sigV4ScopeDiffs("query."+amzCredential, rp.scope, lp.scope)
}
