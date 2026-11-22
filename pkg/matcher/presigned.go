package matcher

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"time"
)

// A SigV4 presigned URL in a test case's response. An app that hands out an
// S3 download or upload link signs it with its AWS secret key, and a replay
// runs the app with a stand-in for that key and for its session token (the
// k8s-proxy capture and the cloud replay's synthesis scramble both,
// replay-isolation), so the replayed link's X-Amz-Signature is never the
// recording's, and an X-Amz-Security-Token is the stand-in. Compared as
// text, every such test case failed on a replay that answered right.
//
// So a response value is read by the presigned URL's grammar, as the mock
// side reads an outgoing presigned call's query (the proxy's
// strict_sigv4.go), and compared as written in every part but two:
//
//   - X-Amz-Signature, a function of the rest and the secret key: its value
//     must be a SigV4 signature (64 lowercase hex) on both sides, and is not
//     compared;
//   - X-Amz-Security-Token, the scrambled session token: present on both
//     sides or on neither, its value not compared.
//
// Everything else is compared byte for byte, in order: the scheme, the host,
// the path, the fragment, the algorithm, X-Amz-Credential (key id, date,
// region, service), X-Amz-Date, X-Amz-Expires, X-Amz-SignedHeaders and every
// other parameter. A value that does not parse by the grammar on either side
// is compared whole, as before. A presigned URL inside a longer text (a
// sentence, an HTML page) is compared whole too: only a value that is the
// URL, with any whitespace around it, is read this way.
//
// A presigned POST is read the same way (samePresignedPostField): its
// X-Amz-Signature field not compared, its X-Amz-Security-Token field present
// on both sides, and its Policy compared decoded, but for the session token
// condition the policy names.

const (
	presignedAlgorithm = "AWS4-HMAC-SHA256"
	presignedSigParam  = "X-Amz-Signature"
	presignedTokParam  = "X-Amz-Security-Token"
)

// SamePresignedSigV4 reports whether rec and live are the same SigV4
// presigned URL but for its signature and session token. It is false when
// either is no presigned URL, and for two equal strings it is not needed.
func SamePresignedSigV4(rec, live string) bool {
	r, ok := presignedURLCanon(rec)
	if !ok {
		return false
	}
	l, ok := presignedURLCanon(live)
	return ok && r == l
}

// presignedURLCanon is s with the values of its X-Amz-Signature and
// X-Amz-Security-Token parameters cut, when s is a SigV4 presigned URL (with
// any whitespace around it): X-Amz-Algorithm AWS4-HMAC-SHA256, one
// X-Amz-Credential, X-Amz-Date and X-Amz-Signature that parse, at most one
// X-Amz-Security-Token. Every other byte of s is kept as it is.
func presignedURLCanon(s string) (string, bool) {
	if !strings.Contains(s, presignedSigParam+"=") {
		return "", false
	}
	core := strings.TrimSpace(s)
	lead := s[:strings.Index(s, core)]
	trail := s[len(lead)+len(core):]
	u, err := url.Parse(core)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery == "" {
		return "", false
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || !presignedQueryParses(q) {
		return "", false
	}
	beforeQuery, rest, _ := strings.Cut(core, "?")
	rawQuery, frag, hasFrag := strings.Cut(rest, "#")
	params := strings.Split(rawQuery, "&")
	for i, p := range params {
		rawName, _, _ := strings.Cut(p, "=")
		name, err := url.QueryUnescape(rawName)
		if err != nil {
			return "", false
		}
		if name == presignedSigParam || name == presignedTokParam {
			params[i] = rawName + "="
		}
	}
	out := lead + beforeQuery + "?" + strings.Join(params, "&")
	if hasFrag {
		out += "#" + frag
	}
	return out + trail, true
}

// presignedQueryParses reads a query by the presigned URL's grammar, as
// the proxy's parsePresignedSigV4 does.
func presignedQueryParses(q url.Values) bool {
	one := func(name string) (string, bool) {
		v := q[name]
		if len(v) != 1 {
			return "", false
		}
		return v[0], true
	}
	if alg, ok := one("X-Amz-Algorithm"); !ok || alg != presignedAlgorithm {
		return false
	}
	if cred, ok := one("X-Amz-Credential"); !ok || !presignedCredentialParses(cred) {
		return false
	}
	if date, ok := one("X-Amz-Date"); !ok || !presignedDateParses(date) {
		return false
	}
	if sig, ok := one(presignedSigParam); !ok || !isSigV4Signature(sig) {
		return false
	}
	return len(q[presignedTokParam]) <= 1
}

// presignedCredentialParses reads <key id>/<YYYYMMDD>/<region>/<service>/<terminator>.
func presignedCredentialParses(s string) bool {
	p := strings.Split(s, "/")
	if len(p) != 5 {
		return false
	}
	for _, x := range p {
		if x == "" {
			return false
		}
	}
	_, err := time.Parse("20060102", p[1])
	return err == nil
}

func presignedDateParses(s string) bool {
	_, err := time.Parse("20060102T150405Z", s)
	return err == nil
}

// isSigV4Signature: a SigV4 signature is 64 lowercase hex.
func isSigV4Signature(s string) bool {
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

// presignedPostField names, folded: the SDKs spell them X-Amz-Signature and
// Policy (JS, Go, Java) or x-amz-signature and policy (boto3).
const (
	postAlgorithm  = "x-amz-algorithm"
	postCredential = "x-amz-credential"
	postDate       = "x-amz-date"
	postSignature  = "x-amz-signature"
	postToken      = "x-amz-security-token"
	postPolicy     = "policy"
)

// presignedPostFields is the folded name of each SigV4 field of a presigned
// POST's form, and its value, when m is one: one field per folded name, the
// algorithm AWS4-HMAC-SHA256, a credential and date that parse, a signature
// that is one, and a policy that decodes. Nil otherwise.
func presignedPostFields(m map[string]interface{}) map[string]string {
	if len(m) < 5 {
		return nil
	}
	f := map[string]string{}
	for k, v := range m {
		lk := postFieldName(k)
		if lk == "" {
			continue
		}
		s, ok := v.(string)
		if _, dup := f[lk]; dup || !ok {
			return nil
		}
		f[lk] = s
	}
	if f[postAlgorithm] != presignedAlgorithm || !presignedCredentialParses(f[postCredential]) ||
		!presignedDateParses(f[postDate]) || !isSigV4Signature(f[postSignature]) {
		return nil
	}
	if _, ok := decodePostPolicy(f[postPolicy]); !ok {
		return nil
	}
	return f
}

// postFieldName is k's folded presigned POST field name, or "" when k is
// none of them. It runs on every JSON object a comparison walks, so it
// folds without allocating.
func postFieldName(k string) string {
	for _, n := range [...]string{postAlgorithm, postCredential, postDate, postSignature, postToken, postPolicy} {
		if len(k) == len(n) && strings.EqualFold(k, n) {
			return n
		}
	}
	return ""
}

// samePresignedPostField reports whether member k of two presigned POST
// forms (presignedPostFields on both, the caller's check) is the same but
// for the signature and the session token: X-Amz-Signature (already read as
// a signature on both sides) and X-Amz-Security-Token are not compared, and
// Policy is compared decoded, with its session token condition's value cut.
// Any other member is the caller's to compare.
func samePresignedPostField(k string, rec, live interface{}) bool {
	switch postFieldName(k) {
	case postSignature, postToken:
		return true
	case postPolicy:
		r, _ := rec.(string)
		l, _ := live.(string)
		rp, rok := decodePostPolicy(r)
		lp, lok := decodePostPolicy(l)
		return rok && lok && reflect.DeepEqual(rp, lp)
	}
	return false
}

// decodePostPolicy decodes a presigned POST's policy (base64 JSON with an
// expiration and a list of conditions), with the value of the session token
// condition cut: {"x-amz-security-token": <token>} or
// ["eq", "$x-amz-security-token", <token>].
func decodePostPolicy(s string) (map[string]interface{}, bool) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var p map[string]interface{}
	if err := dec.Decode(&p); err != nil || dec.More() {
		return nil, false
	}
	if _, ok := p["expiration"].(string); !ok {
		return nil, false
	}
	conds, ok := p["conditions"].([]interface{})
	if !ok {
		return nil, false
	}
	for _, c := range conds {
		switch c := c.(type) {
		case map[string]interface{}:
			for k := range c {
				if strings.ToLower(k) == postToken {
					c[k] = ""
				}
			}
		case []interface{}:
			if len(c) == 3 {
				if name, _ := c[1].(string); strings.ToLower(name) == "$"+postToken {
					c[2] = ""
				}
			}
		}
	}
	return p, true
}
