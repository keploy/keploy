// Package bodydigest recognises the response headers a server computes over
// the body it sends (an ETag, Content-MD5, Digest, the AWS checksums) and
// recomputes them over another body. The matcher uses it to assert a templated
// response's digest over the answer's body, keploy normalize to keep a digest
// on disk the digest of the body as recorded, and the HTTP recorder and
// replayer to serve a recorded digest over the body a replay sends (Note,
// Apply): a body obfuscation or encryption rewrote is not the one the server
// computed it over.
package bodydigest

import (
	"bytes"
	"crypto/md5"  //nolint:gosec // recomputing a digest a server sent, not securing anything
	"crypto/sha1" //nolint:gosec // as above: Express's ETag and RFC 3230 "sha" are SHA-1
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"hash/crc64"
	"sort"
	"strconv"
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
	case "etag", "content-md5", "digest", "content-digest", "repr-digest", amzCRC32Header:
		return true
	}
	return amzChecksumAlg(lowerName) != ""
}

// The AWS response checksums: x-amz-crc32, the CRC-32 (IEEE) of the body in
// decimal (DynamoDB; its SDKs check it), and x-amz-checksum-<algorithm>, the
// base64 of the big-endian checksum of the object (S3 GetObject; the SDKs
// check it by default since 2025's flexible checksums). A composite S3
// checksum ("...-<parts>", of a multipart upload's parts) is no checksum of
// the body, and not recognised.
const (
	amzCRC32Header    = "x-amz-crc32"
	amzChecksumPrefix = "x-amz-checksum-"
)

// amzChecksumAlg is the algorithm of an x-amz-checksum-<algorithm> header, or
// "" for any other.
func amzChecksumAlg(lowerName string) string {
	alg, ok := strings.CutPrefix(lowerName, amzChecksumPrefix)
	if !ok {
		return ""
	}
	switch alg {
	case "crc32", "crc32c", "crc64nvme", "sha1", "sha256":
		return alg
	}
	return ""
}

// crc64NVME is the CRC-64/NVME table (reflected polynomial), S3's crc64nvme;
// crc32C, CRC-32C's (Castagnoli).
var (
	crc64NVME = crc64.MakeTable(0x9a6c9329ac4bc9b5)
	crc32C    = crc32.MakeTable(crc32.Castagnoli)
)

// sums computes each digest of one body at most once, and only when a header
// asks for it: a body can be up to pkg.MaxDecompressedSize, and one response
// can carry several digest headers (an ETag, an x-amz-checksum and a
// Content-MD5), which some forms share (an MD5 ETag and Content-MD5).
type sums struct {
	b      []byte
	hashes map[string][]byte
}

func newSums(b []byte) *sums { return &sums{b: b} }

// observeSum, when set (tests), is told each digest of a body computed.
var observeSum func(alg string)

// lazySums returns the sums of the body body returns, made on the first call
// and the same on every call after: a value of no digest's shape asks for
// none, and the body is not even converted.
func lazySums(body func() []byte) func() *sums {
	var s *sums
	return func() *sums {
		if s == nil {
			s = newSums(body())
		}
		return s
	}
}

// sum is the raw digest of the body by algorithm name: md5, sha1, sha256,
// sha512, crc32, crc32c or crc64nvme (big-endian); nil for another.
func (s *sums) sum(alg string) []byte {
	if v, ok := s.hashes[alg]; ok {
		return v
	}
	if observeSum != nil {
		observeSum(alg)
	}
	var v []byte
	switch alg {
	case "md5":
		h := md5.Sum(s.b) //nolint:gosec
		v = h[:]
	case "sha1":
		h := sha1.Sum(s.b) //nolint:gosec
		v = h[:]
	case "sha256":
		h := sha256.Sum256(s.b)
		v = h[:]
	case "sha512":
		h := sha512.Sum512(s.b)
		v = h[:]
	case "crc32":
		v = binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(s.b))
	case "crc32c":
		v = binary.BigEndian.AppendUint32(nil, crc32.Checksum(s.b, crc32C))
	case "crc64nvme":
		v = binary.BigEndian.AppendUint64(nil, crc64.Checksum(s.b, crc64NVME))
	default:
		return nil
	}
	if s.hashes == nil {
		s.hashes = map[string][]byte{}
	}
	s.hashes[alg] = v
	return v
}

func (s *sums) b64(alg string) string { return base64.StdEncoding.EncodeToString(s.sum(alg)) }
func (s *sums) hex(alg string) string { return hex.EncodeToString(s.sum(alg)) }

// b64Len is the length of the base64 of a digest of n bytes: a value of
// another length is no such digest, and is told so without hashing.
func b64Len(n int) int { return base64.StdEncoding.EncodedLen(n) }

// digestSize is the size of each digest a header can carry, by algorithm.
var digestSize = map[string]int{"md5": 16, "sha1": 20, "sha256": 32, "sha512": 64, "crc32": 4, "crc32c": 4, "crc64nvme": 8}

// amzChecksum is the x-amz-checksum-<alg> value of the body.
func (s *sums) amzChecksum(alg string) string {
	if digestSize[alg] == 0 || alg == "md5" || alg == "sha512" {
		return ""
	}
	return s.b64(alg)
}

// amzCRC32 is the x-amz-crc32 value of the body.
func (s *sums) amzCRC32() string {
	return strconv.FormatUint(uint64(binary.BigEndian.Uint32(s.sum("crc32"))), 10)
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
//     SHA-1, SHA-256 or SHA-512 of the body;
//   - x-amz-checksum-<alg> and x-amz-crc32 (the AWS checksums above).
//
// Anything else (a resource version such as "7", a hash of something other
// than this body) is not a digest of it, and false.
func Form(lowerName, value, body string) (Of, bool) {
	spec, ok := Spec(lowerName, value, body)
	if !ok {
		return nil, false
	}
	return func(b string) string {
		v, _ := Compute(spec, b)
		return v
	}, true
}

// Spec is Form's finding written as text, which a recording can carry and
// Compute reads back: how header lowerName's value was computed from body, or
// false when it is no digest of it. Its forms:
//
//   - "content-md5";
//   - "etag/<tag>/<w|s>[/U]": tag express, rack, spring, md5, sha1 or sha256,
//     weak or strong, U a hex tag in upper case;
//   - "list/<alg>[:],...": a Digest, Content-Digest or Repr-Digest list, each
//     item's algorithm as written, ":" when its value is in colons;
//   - "amz/<alg>": an x-amz-checksum-<alg>;
//   - "amz-crc32": an x-amz-crc32.
func Spec(lowerName, value, body string) (string, bool) {
	if !IsHeader(lowerName) {
		return "", false
	}
	return specOf(lowerName, value, len(body), lazySums(func() []byte { return []byte(body) }))
}

// specOf is Spec over a body of size n whose sums are made by of, only once a
// value has the shape of a digest of it.
func specOf(lowerName, value string, n int, of func() *sums) (string, bool) {
	switch lowerName {
	case "etag":
		return etagSpec(value, n, of)
	case "content-md5":
		if len(value) == b64Len(16) && value == of().b64("md5") {
			return "content-md5", true
		}
	case "digest", "content-digest", "repr-digest":
		return digestListSpec(value, of)
	case amzCRC32Header:
		if len(value) == 0 || len(value) > 10 || strings.Trim(value, "0123456789") != "" {
			return "", false
		}
		if value == of().amzCRC32() {
			return "amz-crc32", true
		}
	default:
		if alg := amzChecksumAlg(lowerName); alg != "" && len(value) == b64Len(digestSize[alg]) && value == of().amzChecksum(alg) {
			return "amz/" + alg, true
		}
	}
	return "", false
}

// Compute is the value of the digest spec (Spec) describes, over body; false
// for a spec it does not read.
func Compute(spec, body string) (string, bool) {
	return computeOf(spec, len(body), lazySums(func() []byte { return []byte(body) }))
}

// computeOf is Compute over a body of size n whose sums are made by of.
func computeOf(spec string, n int, of func() *sums) (string, bool) {
	kind, rest, _ := strings.Cut(spec, "/")
	switch kind {
	case "content-md5":
		return of().b64("md5"), true
	case "amz-crc32":
		return of().amzCRC32(), true
	case "amz":
		if v := of().amzChecksum(rest); v != "" {
			return v, true
		}
	case "etag":
		return etagCompute(rest, n, of)
	case "list":
		return digestListCompute(rest, of)
	}
	return "", false
}

// isHex reports whether s is n hex digits, in either case.
func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// etagSpec reads an ETag's tag. Each form is tried only when the tag has its
// shape, so a tag of no form keploy knows (a version, an S3 multipart
// "<md5>-<parts>") is told so without hashing the body.
func etagSpec(value string, n int, of func() *sums) (string, bool) {
	weak := strings.HasPrefix(value, "W/")
	tag := strings.TrimPrefix(value, "W/")
	if len(tag) < 2 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return "", false
	}
	tag = tag[1 : len(tag)-1]
	strength := "s"
	if weak {
		strength = "w"
	}
	if lenHex, sha, ok := strings.Cut(tag, "-"); ok && lenHex == strconv.FormatInt(int64(n), 16) && len(sha) == 27 {
		if sha == expressTag(n, of())[len(lenHex)+1:] {
			return "etag/express/" + strength, true
		}
		return "", false
	}
	if isHex(tag, 32) && tag == rackTag(of()) {
		return "etag/rack/" + strength, true
	}
	if len(tag) == 33 && tag[0] == '0' && isHex(tag[1:], 32) && tag == springTag(of()) {
		return "etag/spring/" + strength, true
	}
	for _, name := range []string{"md5", "sha1", "sha256"} {
		if !isHex(tag, 2*digestSize[name]) {
			continue
		}
		sum := of().hex(name)
		if !strings.EqualFold(tag, sum) {
			continue
		}
		spec := "etag/" + name + "/" + strength
		if tag != sum {
			spec += "/U"
		}
		return spec, true
	}
	return "", false
}

// etagHashes are the hashes of a plain hex ETag, by their name in a spec.
var etagHashes = map[string]bool{"md5": true, "sha1": true, "sha256": true}

func etagCompute(rest string, n int, of func() *sums) (string, bool) {
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return "", false
	}
	var tag string
	switch parts[0] {
	case "express":
		tag = expressTag(n, of())
	case "rack":
		tag = rackTag(of())
	case "spring":
		tag = springTag(of())
	default:
		if !etagHashes[parts[0]] {
			return "", false
		}
		tag = of().hex(parts[0])
		if len(parts) > 2 && parts[2] == "U" {
			tag = strings.ToUpper(tag)
		}
	}
	prefix := ""
	if parts[1] == "w" {
		prefix = "W/"
	}
	return prefix + `"` + tag + `"`, true
}

// expressTag is the tag of Express's default ETag (the etag package) of a
// body of n bytes: its length in hex and its SHA-1 in base64, cut to 27
// characters.
func expressTag(n int, s *sums) string {
	return fmt.Sprintf("%x-%s", n, s.b64("sha1")[:27])
}

// rackTag is the tag of Rack::ETag, in Rails' default middleware: the first
// 32 hex digits of the body's SHA-256 (sent weak).
func rackTag(s *sums) string { return s.hex("sha256")[:32] }

// springTag is the tag of Spring's ShallowEtagHeaderFilter: "0" and the
// body's hex MD5.
func springTag(s *sums) string { return "0" + s.hex("md5") }

// digestListSpec reads "sha-256=X, md5=Y" (Digest) or "sha-256=:X:"
// (Content-Digest, Repr-Digest): every item must be a digest of body. An item
// whose value has not its algorithm's length is told no digest without
// hashing.
func digestListSpec(value string, of func() *sums) (string, bool) {
	var items []string
	for _, part := range strings.Split(value, ",") {
		alg, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return "", false
		}
		name := digestAlg(alg)
		if name == "" {
			return "", false
		}
		colons := len(v) >= 2 && v[0] == ':' && v[len(v)-1] == ':'
		if colons {
			v = v[1 : len(v)-1]
		}
		if len(v) != b64Len(digestSize[name]) || v != of().b64(name) {
			return "", false
		}
		if colons {
			alg += ":"
		}
		items = append(items, alg)
	}
	if len(items) == 0 {
		return "", false
	}
	return "list/" + strings.Join(items, ","), true
}

func digestListCompute(rest string, of func() *sums) (string, bool) {
	var out []string
	for _, item := range strings.Split(rest, ",") {
		alg, colons := strings.CutSuffix(item, ":")
		name := digestAlg(alg)
		if name == "" {
			return "", false
		}
		v := of().b64(name)
		if colons {
			v = ":" + v + ":"
		}
		out = append(out, alg+"="+v)
	}
	return strings.Join(out, ", "), true
}

// digestAlg is the name (sums) of a Digest list item's algorithm, as written
// in any case, or "" for one keploy does not know.
func digestAlg(alg string) string {
	switch strings.ToLower(alg) {
	case "md5":
		return "md5"
	case "sha":
		return "sha1"
	case "sha-256":
		return "sha256"
	case "sha-512":
		return "sha512"
	}
	return ""
}

// A NOTE OF THE DIGESTS A RESPONSE CARRIES.
//
// A replay does not always send the body a server computed a digest header
// over: obfuscation rewrites a secret in it, encryption gives it back
// re-serialised, a variable changes it, and a body recorded decoded is sent
// encoded again by keploy's own compressor. A client that checks the digest (an
// AWS SDK's S3 GetObject or DynamoDB call, a Java S3 client's ETag check, a
// Content-MD5 check) refuses such a response. So the recorder notes, as it
// records, which of the response's digest headers its body matched as the
// server sent it (Note), and the replayer recomputes exactly those over the
// bytes it sends (Apply). A digest that did not match at record (a server's own
// wrong one, a resource version, an S3 composite checksum) is noted not, and
// is served as recorded.

// Note is the note of the digest headers of header (a recorded response's,
// names in any case) that the response's body matched as the server sent it:
// body, decoded of its Content-Encoding, or wire, the encoded bytes (the same
// as body when it had none). Each "<lower-case name>=<spec>" (Spec), "~" before
// a spec over the encoded bytes, joined by ";" in name order; "" when none.
//
// It costs nothing for a response with no digest header, and hashes the body
// only for a value that has a digest's shape, each digest at most once over
// each of body and wire: a recorded body can be as large as
// pkg.MaxDecompressedSize.
func Note(header map[string]string, body, wire []byte) string {
	var notes []string
	var overBody, overWire func() *sums
	for k, v := range header {
		name := strings.ToLower(k)
		if !IsHeader(name) {
			continue
		}
		if overBody == nil {
			overBody = lazySums(func() []byte { return body })
			overWire = lazySums(func() []byte { return wire })
		}
		if spec, ok := specOf(name, v, len(body), overBody); ok {
			notes = append(notes, name+"="+spec)
		} else if !bytes.Equal(wire, body) {
			if spec, ok := specOf(name, v, len(wire), overWire); ok {
				notes = append(notes, name+"=~"+spec)
			}
		}
	}
	sort.Strings(notes)
	return strings.Join(notes, ";")
}

// Noted reports whether note (Note) names the digest header lowerName.
func Noted(note, lowerName string) bool {
	for _, item := range strings.Split(note, ";") {
		if name, _, ok := strings.Cut(item, "="); ok && name == lowerName {
			return true
		}
	}
	return false
}

// Without is note (Note) without the digest headers drop names (lower case):
// a header a replay sets itself (a variable's value) is no digest to
// recompute.
func Without(note string, drop map[string]bool) string {
	if note == "" || len(drop) == 0 {
		return note
	}
	var kept []string
	for _, item := range strings.Split(note, ";") {
		if name, _, ok := strings.Cut(item, "="); ok && drop[name] {
			continue
		}
		kept = append(kept, item)
	}
	return strings.Join(kept, ";")
}

// Apply sets each digest header note (Note) names in header (a response's
// wire headers) to its value over the bytes a replay sends: body, decoded, or
// wire, encoded (the same as body when it is sent with no Content-Encoding).
// A name that header has under two spellings, or none, is left alone, and so
// is one with more than one value, except a Digest list: a list on two lines
// is one list, sent again as one. A spec it does not read is left as
// recorded. Each digest is computed at most once over each of body and wire.
func Apply(note string, header map[string][]string, body, wire string) {
	if note == "" {
		return
	}
	overBody := lazySums(func() []byte { return []byte(body) })
	overWire := lazySums(func() []byte { return []byte(wire) })
	for _, item := range strings.Split(note, ";") {
		name, spec, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		n, of := len(body), overBody
		if s, encoded := strings.CutPrefix(spec, "~"); encoded {
			spec, n, of = s, len(wire), overWire
		}
		key := ""
		for k := range header {
			if strings.EqualFold(k, name) {
				if key != "" { // two spellings of one name
					key = ""
					break
				}
				key = k
			}
		}
		if key == "" || len(header[key]) == 0 || len(header[key]) > 1 && !strings.HasPrefix(spec, "list/") {
			continue
		}
		if v, ok := computeOf(spec, n, of); ok {
			header[key] = []string{v}
		}
	}
}
