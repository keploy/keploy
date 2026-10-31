package learn

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/variables"
)

// JWT is a JSON Web Token a value holds whole (RFC 7519, compact JWS): three
// base64url segments, the first a JOSE header naming its algorithm, the
// second a JSON object of claims, the third its signature. A value may hold
// one alone or after one authorization scheme word and a space ("Bearer
// <token>").
//
// A token is compared by what it says, never by its bytes: its scheme and
// JOSE header whole, each claim as a leaf of its own (Leaves:
// "<place>{jwt}.<claim>"), so the learn step learns only the claims that
// moved (word-level learning: a fresh jti, an exp a read of the clock) and
// every other claim is still compared, and its signature by its form
// (SignatureForm). A signature changes whenever a claim does and cannot be
// made again without the signer's key, so keploy cannot compare its bytes;
// the claims it signs are compared instead.
type JWT struct {
	// Scheme is the text before the token ("Bearer "), "" for none.
	Scheme string
	// Header is the JOSE header, as JSON with its keys sorted.
	Header string
	// Claims are the claims, decoded (DecodeJSON: numbers as written).
	Claims interface{}
	// Signature is the third segment as the value holds it.
	Signature string
	// head is the first segment as the value holds it.
	head string
}

// JWTMark is what a place's name gains for the token it holds: its claims are
// "<place>{jwt}.<claim>".
const JWTMark = variables.JWTMark

// ReadJWT reads s as a JWT (JWT), and reports whether it is one: three
// segments joined by '.', the first two unpadded base64url of a JSON object,
// the header naming its algorithm ("alg"), the third base64url, empty only
// for an unsecured token ("alg": "none"); alone, or after one scheme word of
// letters and a space.
func ReadJWT(s string) (JWT, bool) {
	var j JWT
	token := s
	if i := strings.IndexByte(s, ' '); i >= 0 {
		j.Scheme, token = s[:i+1], s[i+1:]
		for k := 0; k < i; k++ {
			if !isASCIILetter(s[k]) {
				return JWT{}, false
			}
		}
		if i == 0 {
			return JWT{}, false
		}
	}
	seg := strings.Split(token, ".")
	if len(seg) != 3 || seg[0] == "" || seg[1] == "" || !isB64URL(seg[2]) {
		return JWT{}, false
	}
	head, ok := jwtObject(seg[0])
	if !ok {
		return JWT{}, false
	}
	alg, ok := head["alg"].(string)
	if !ok || alg == "" || seg[2] == "" && alg != "none" {
		return JWT{}, false
	}
	claims, ok := jwtObject(seg[1])
	if !ok {
		return JWT{}, false
	}
	hb, err := json.Marshal(head)
	if err != nil {
		return JWT{}, false
	}
	j.Header, j.Claims, j.Signature, j.head = string(hb), claims, seg[2], seg[0]
	return j, true
}

// JWTClaims is the JSON text of the claims of the JWT s holds (ReadJWT), and
// whether s holds one: what a replay reads a claim's place in
// (variables.JWTMark).
func JWTClaims(s string) (string, bool) {
	if _, ok := ReadJWT(s); !ok {
		return "", false
	}
	seg := strings.Split(s[strings.IndexByte(s, ' ')+1:], ".")
	b, err := base64.RawURLEncoding.DecodeString(seg[1])
	if err != nil {
		return "", false
	}
	return string(b), true
}

// WithJWTClaims is s, a value holding a JWT (ReadJWT), with its claims
// replaced by the JSON object claims: its scheme, its JOSE header and its
// signature kept as s holds them. A replay puts a claim's recorded value back
// with it (variables.JWTMark); the signature no longer signs the claims, and
// the matcher compares it by its form alone (SignatureForm).
func WithJWTClaims(s, claims string) (string, bool) {
	j, ok := ReadJWT(s)
	if !ok {
		return "", false
	}
	if v, ok := DecodeJSON([]byte(claims)); !ok {
		return "", false
	} else if _, obj := v.(map[string]interface{}); !obj {
		return "", false
	}
	return j.Scheme + j.head + "." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + "." + j.Signature, true
}

// jwtObject is the JSON object an unpadded base64url segment holds.
func jwtObject(seg string) (map[string]interface{}, bool) {
	if !isB64URL(seg) {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return nil, false
	}
	v, ok := DecodeJSON(b)
	if !ok {
		return nil, false
	}
	m, ok := v.(map[string]interface{})
	return m, ok
}

// isB64URL reports whether s is of the base64url alphabet alone, unpadded.
func isB64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isB64URLByte(s[i]) {
			return false
		}
	}
	return true
}

// isB64URLByte reports whether c is of the base64url alphabet.
func isB64URLByte(c byte) bool {
	return isASCIILetter(c) || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// isASCIILetter reports whether c is an ASCII letter.
func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// Fixed is what of the token must stay as recorded for a call to be its
// recording's: its scheme, its JOSE header and its signature's form.
func (j JWT) Fixed() string {
	return j.Scheme + "\x00" + j.Header + "\x00" + j.SignatureForm()
}

// SignatureForm is how a replay checks a token's signature: base64url of the
// recorded length. Its bytes are never compared (a signature changes with
// every claim, and keploy holds no signer's key); the algorithm the header
// names is compared whole, so a token signed another way, or not at all, is
// another token.
func (j JWT) SignatureForm() string {
	return "base64url, " + strconv.Itoa(len(j.Signature)) + " characters"
}

// Leaves adds the token's claims to out as leaves of place (JSONLeaves):
// "<place>{jwt}.<claim>", each compared as a leaf of its own.
func (j JWT) Leaves(place string, out map[string]Leaf) {
	JSONLeaves(place+JWTMark, j.Claims, out)
}

// ClaimOf is the place whose token a claim leaf's path names, and whether
// path names one ("header.authorization" of "header.authorization{jwt}.exp").
func ClaimOf(path string) (string, bool) {
	i := strings.Index(path, JWTMark+".")
	if i <= 0 {
		return "", false
	}
	return path[:i], true
}
