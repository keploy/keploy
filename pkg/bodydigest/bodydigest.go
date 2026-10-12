// Package bodydigest recognises the response headers a server computes over
// the body it sends (an ETag, Content-MD5, Digest) and recomputes them over
// another body. The matcher uses it to assert a templated response's digest
// over the answer's body, and keploy normalize to keep a digest on disk the
// digest of the body as recorded.
package bodydigest

import (
	"crypto/md5"  //nolint:gosec // recomputing a digest a server sent, not securing anything
	"crypto/sha1" //nolint:gosec // as above: Express's ETag and RFC 3230 "sha" are SHA-1
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

// Of is how a header value was computed from a body: given a body, it writes
// the value the same server would send for it.
type Of func(body string) string

// IsHeader reports whether a response header (its name in lower case) is one
// a server computes over the body's bytes. Content-Length is not one: it
// follows the body by length alone.
func IsHeader(lowerName string) bool {
	switch lowerName {
	case "etag", "content-md5", "digest", "content-digest", "repr-digest":
		return true
	}
	return false
}

// Form reports how a header value was computed from body, when it is one of
// the digests a server writes over the body it sends:
//
//   - ETag, weak (W/"...") or strong ("..."), whose tag is Express's
//     "<length in hex>-<SHA-1 in base64, 27 chars>" (the etag package),
//     Rails' "<first 32 hex digits of the SHA-256>" (Rack::ETag), Spring's
//     "0<hex MD5>" (ShallowEtagHeaderFilter), or a hex MD5, SHA-1 or
//     SHA-256 in either letter case;
//   - Content-MD5: the base64 MD5 of the body;
//   - Digest, Content-Digest, Repr-Digest: each alg=value item a base64 MD5,
//     SHA-1, SHA-256 or SHA-512 of the body.
//
// Anything else (a resource version such as "7", a hash of something other
// than this body) is not a digest of it, and false.
func Form(lowerName, value, body string) (Of, bool) {
	switch lowerName {
	case "etag":
		return etagForm(value, body)
	case "content-md5":
		if value == b64(md5.New(), body) { //nolint:gosec
			return func(b string) string { return b64(md5.New(), b) }, true //nolint:gosec
		}
	case "digest", "content-digest", "repr-digest":
		return digestListForm(value, body)
	}
	return nil, false
}

func etagForm(value, body string) (Of, bool) {
	weak := strings.HasPrefix(value, "W/")
	tag := strings.TrimPrefix(value, "W/")
	if len(tag) < 2 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return nil, false
	}
	tag = tag[1 : len(tag)-1]
	prefix := ""
	if weak {
		prefix = "W/"
	}
	for _, f := range []func(string) string{expressTag, rackTag, springTag} {
		if tag == f(body) {
			return func(b string) string { return prefix + `"` + f(b) + `"` }, true
		}
	}
	for _, h := range []func() hash.Hash{md5.New, sha1.New, sha256.New} { //nolint:gosec
		sum := hexSum(h(), body)
		if !strings.EqualFold(tag, sum) {
			continue
		}
		upper := tag != sum
		return func(b string) string {
			s := hexSum(h(), b)
			if upper {
				s = strings.ToUpper(s)
			}
			return prefix + `"` + s + `"`
		}, true
	}
	return nil, false
}

// expressTag is the tag of Express's default ETag (the etag package): the
// body's length in hex and its SHA-1 in base64, cut to 27 characters.
func expressTag(body string) string {
	return fmt.Sprintf("%x-%s", len(body), b64(sha1.New(), body)[:27]) //nolint:gosec
}

// rackTag is the tag of Rack::ETag, in Rails' default middleware: the first
// 32 hex digits of the body's SHA-256 (sent weak).
func rackTag(body string) string {
	return hexSum(sha256.New(), body)[:32]
}

// springTag is the tag of Spring's ShallowEtagHeaderFilter: "0" and the
// body's hex MD5.
func springTag(body string) string {
	return "0" + hexSum(md5.New(), body) //nolint:gosec
}

// digestListForm reads "sha-256=X, md5=Y" (Digest) or "sha-256=:X:"
// (Content-Digest, Repr-Digest): every item must be a digest of body.
func digestListForm(value, body string) (Of, bool) {
	type item struct {
		alg     string
		colons  bool
		newHash func() hash.Hash
	}
	var items []item
	for _, part := range strings.Split(value, ",") {
		alg, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, false
		}
		newHash := digestAlg(alg)
		if newHash == nil {
			return nil, false
		}
		colons := len(v) >= 2 && v[0] == ':' && v[len(v)-1] == ':'
		if colons {
			v = v[1 : len(v)-1]
		}
		if v != b64(newHash(), body) {
			return nil, false
		}
		items = append(items, item{alg: alg, colons: colons, newHash: newHash})
	}
	if len(items) == 0 {
		return nil, false
	}
	return func(b string) string {
		out := make([]string, 0, len(items))
		for _, it := range items {
			v := b64(it.newHash(), b)
			if it.colons {
				v = ":" + v + ":"
			}
			out = append(out, it.alg+"="+v)
		}
		return strings.Join(out, ", ")
	}, true
}

func digestAlg(alg string) func() hash.Hash {
	switch strings.ToLower(alg) {
	case "md5":
		return md5.New //nolint:gosec
	case "sha":
		return sha1.New //nolint:gosec
	case "sha-256":
		return sha256.New
	case "sha-512":
		return sha512.New
	}
	return nil
}

func b64(h hash.Hash, body string) string {
	h.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func hexSum(h hash.Hash, body string) string {
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}
