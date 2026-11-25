// Package sigv4 implements AWS Signature Version 4 part-wise parsing and
// comparison shared by HTTP/1 strict compare and HTTP/2 exact serving.
package sigv4

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AWS Signature Version 4 part-wise comparison (founder decision 25): an AWS
// SDK signs every call over the request and the time it is made, so a replay's
// signature is never the recording's, and an Authorization header compared
// whole misses every AWS call. Both HTTP/1 strict compare and HTTP/2 exact
// serving share this grammar so neither copies the rules:
//
//	AWS4-HMAC-SHA256 Credential=<key id>/<date>/<region>/<service>/<terminator>, SignedHeaders=<h1;h2;...>, Signature=<64 hex>
//
//   - the algorithm, the region, the service, the terminator (aws4_request)
//     and the signed headers (minus x-amz-security-token) are compared
//     exactly: another region, service or set of signed headers is another
//     call;
//   - the access key id, the date and the signature are not compared (SF-3:
//     credential and environment values differ across runs and machines).
//
// x-amz-content-sha256 is checked against the SHA-256 of the body the client
// sent when the recording's is a hash, and against the recorded literal
// otherwise (UNSIGNED-PAYLOAD, STREAMING-*). A presigned URL's query is read
// the same way: X-Amz-Credential by scope, X-Amz-Signature / X-Amz-Date /
// X-Amz-Security-Token not compared; every other query parameter as any query
// value.
//
// Nothing is guessed from a value's shape: a value that does not parse by this
// grammar on either side, or another scheme (Bearer, SigV4A, Azure SharedKey),
// is rejected by ParseAuthorization so callers compare it whole.

// Algorithm is the algorithm, and scheme, of AWS Signature Version 4.
const Algorithm = "AWS4-HMAC-SHA256"

// SecurityTokenHeader is the SigV4 session token's header, signed exactly when
// credentials are temporary (SSO, assumed role, IRSA).
const SecurityTokenHeader = "x-amz-security-token"

// Query parameter names of a SigV4 presigned URL read by its grammar.
const (
	AmzAlgorithm     = "X-Amz-Algorithm"
	AmzCredential    = "X-Amz-Credential"
	AmzDate          = "X-Amz-Date"
	AmzSignature     = "X-Amz-Signature"
	AmzSecurityToken = "X-Amz-Security-Token"
)

// Scope is a SigV4 credential: <key id>/<date>/<region>/<service>/<terminator>.
type Scope struct {
	KeyID      string
	Date       string
	Region     string
	Service    string
	Terminator string
}

// Auth is a SigV4 Authorization header's parsed parts.
type Auth struct {
	Scope         Scope
	SignedHeaders string
}

// Presigned is a SigV4 presigned URL's parsed signing parameters.
type Presigned struct {
	Scope Scope
}

// Diff describes one SigV4 part where the live request differs from the
// recorded request.
type Diff struct {
	Path     string
	Recorded string
	Live     string
}

// ParseScope reads a credential: five non-empty parts, the date as YYYYMMDD.
func ParseScope(s string) (Scope, bool) {
	p := strings.Split(s, "/")
	if len(p) != 5 {
		return Scope{}, false
	}
	for _, x := range p {
		if x == "" {
			return Scope{}, false
		}
	}
	if _, err := time.Parse("20060102", p[1]); err != nil {
		return Scope{}, false
	}
	return Scope{KeyID: p[0], Date: p[1], Region: p[2], Service: p[3], Terminator: p[4]}, true
}

// ParseAuthorization reads an Authorization header of SigV4: the algorithm,
// then Credential, SignedHeaders and Signature, each once, in any order, comma
// separated.
func ParseAuthorization(v string) (Auth, bool) {
	alg, rest, ok := strings.Cut(v, " ")
	if !ok || alg != Algorithm {
		return Auth{}, false
	}
	var a Auth
	seen := map[string]bool{}
	for _, kv := range strings.Split(rest, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || val == "" || seen[k] {
			return Auth{}, false
		}
		seen[k] = true
		switch k {
		case "Credential":
			if a.Scope, ok = ParseScope(val); !ok {
				return Auth{}, false
			}
		case "SignedHeaders":
			a.SignedHeaders = val
		case "Signature":
			if !IsSHA256Hex(val) {
				return Auth{}, false
			}
		default:
			return Auth{}, false
		}
	}
	return a, len(seen) == 3
}

// IsSHA256Hex reports whether s is a SHA-256 as SigV4 writes one: 64 lowercase
// hex digits.
func IsSHA256Hex(s string) bool {
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

// Day is the day of a SigV4 signing time: an X-Amz-Date (ISO 8601 basic,
// YYYYMMDD'T'HHMMSS'Z'), else an HTTP Date header, in UTC; "" when neither
// says one.
func Day(amzDate, date string) string {
	if t, err := time.Parse("20060102T150405Z", amzDate); err == nil {
		return t.Format("20060102")
	}
	if t, err := http.ParseTime(date); err == nil {
		return t.UTC().Format("20060102")
	}
	return ""
}

// ScopeDiffs are the parts in which a live credential differs from the
// recorded one, named at prefix: the region, service and terminator exactly;
// the access key id and the date not at all.
func ScopeDiffs(prefix string, rec, live Scope) []Diff {
	var out []Diff
	part := func(name, r, l string) {
		if r != l {
			out = append(out, Diff{Path: prefix + "." + name, Recorded: r, Live: l})
		}
	}
	part("region", rec.Region, live.Region)
	part("service", rec.Service, live.Service)
	part("terminator", rec.Terminator, live.Terminator)
	return out
}

// AuthorizationDiffs compares a recorded Authorization header with the live
// one (its lines joined with ",") by their SigV4 parts
// (header.authorization.<part>); ok is false when either is not SigV4, and the
// header is then compared whole, as any header.
func AuthorizationDiffs(rec, live string) (out []Diff, ok bool) {
	ra, rok := ParseAuthorization(rec)
	la, lok := ParseAuthorization(live)
	if !rok || !lok {
		return nil, false
	}
	out = ScopeDiffs("header.authorization", ra.Scope, la.Scope)
	if SignedHeadersWithoutToken(ra.SignedHeaders) != SignedHeadersWithoutToken(la.SignedHeaders) {
		out = append(out, Diff{
			Path:     "header.authorization.signed_headers",
			Recorded: ra.SignedHeaders,
			Live:     la.SignedHeaders,
		})
	}
	return out, true
}

// SignedHeadersWithoutToken is a SignedHeaders list without the session
// token's header: whether it is signed says what kind of credentials signed
// the call, which the credentials of where the replay runs decide.
func SignedHeadersWithoutToken(list string) string {
	var out []string
	for _, h := range strings.Split(list, ";") {
		if h != SecurityTokenHeader {
			out = append(out, h)
		}
	}
	return strings.Join(out, ";")
}

// SamePayloadHash reports whether a live x-amz-content-sha256 is the recorded
// one: the SHA-256 of the body the client sent (sent) when the recorded value
// is a hash, the recorded literal otherwise (UNSIGNED-PAYLOAD, STREAMING-*).
func SamePayloadHash(rec string, live []string, sent []byte) bool {
	lv := strings.Join(live, ",")
	if !IsSHA256Hex(rec) {
		return rec == lv
	}
	sum := sha256.Sum256(sent)
	return lv == hex.EncodeToString(sum[:])
}

// ParsePresigned reads a query as a SigV4 presigned URL's: X-Amz-Algorithm
// AWS4-HMAC-SHA256, and one X-Amz-Credential, X-Amz-Date and X-Amz-Signature
// that parse.
func ParsePresigned(q url.Values) (Presigned, bool) {
	one := func(name string) (string, bool) {
		v := q[name]
		if len(v) != 1 {
			return "", false
		}
		return v[0], true
	}
	if alg, ok := one(AmzAlgorithm); !ok || alg != Algorithm {
		return Presigned{}, false
	}
	cred, ok := one(AmzCredential)
	if !ok {
		return Presigned{}, false
	}
	scope, ok := ParseScope(cred)
	if !ok {
		return Presigned{}, false
	}
	date, ok := one(AmzDate)
	if !ok {
		return Presigned{}, false
	}
	if Day(date, "") == "" {
		return Presigned{}, false
	}
	if sig, ok := one(AmzSignature); !ok || !IsSHA256Hex(sig) {
		return Presigned{}, false
	}
	return Presigned{Scope: scope}, true
}

// PresignedDiffs compares the signing parameters of two presigned URLs
// (ParsePresigned): the credential by its parts
// (query.X-Amz-Credential.<part>), the signature, the signing time and the
// session token not at all. byGrammar names the parameters it read, which the
// query comparison does not compare again; nil when either query is no
// presigned URL's, and the query is compared value by value, as before.
func PresignedDiffs(rec, live url.Values) (byGrammar map[string]bool, out []Diff) {
	rp, rok := ParsePresigned(rec)
	lp, lok := ParsePresigned(live)
	if !rok || !lok {
		return nil, nil
	}
	return map[string]bool{AmzCredential: true, AmzDate: true, AmzSignature: true, AmzSecurityToken: true},
		ScopeDiffs("query."+AmzCredential, rp.Scope, lp.Scope)
}
