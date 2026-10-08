package http

import (
	"crypto/md5"  // #nosec G501 -- recomputing digests a recorded response carried, not for security
	"crypto/sha1" // #nosec G505 -- as above
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"net/http"
	"strconv"
	"strings"
)

// reSign recomputes, over a rewritten body, every integrity header a client
// may check the body against, in place in header. It reports false — the
// caller then serves the response as recorded — when the body is served
// encoded (the digests cover bytes the replay recompresses), or when the
// response carries an integrity header or algorithm it cannot recompute: a
// response with a stale checksum must never be served.
//
// Recomputed: DynamoDB's x-amz-crc32; S3's x-amz-checksum-crc32, -crc32c,
// -crc64nvme, -sha1 and -sha256; GCS's x-goog-hash (crc32c, md5); Content-MD5;
// Digest (RFC 3230: sha-256, sha-512, sha, md5); Content-Digest and
// Repr-Digest (RFC 9530: sha-256, sha-512, md5); an ETag that is the body's
// MD5 (S3's, which its SDKs check). Any other ETag is an opaque version tag no
// client checks against the body, and is kept.
func reSign(header map[string]string, oldBody, newBody string) bool {
	// Decided on the headers as recorded, before any is recomputed below: an
	// ETag rewritten for the new body no longer reads as the old body's MD5.
	if servedEncoded(header) && hasIntegrityHeader(header, oldBody) {
		return false
	}
	body := []byte(newBody)
	for k, v := range header {
		ck := http.CanonicalHeaderKey(k)
		switch {
		case ck == "X-Amz-Crc32":
			header[k] = strconv.FormatUint(uint64(crc32.ChecksumIEEE(body)), 10)
		case strings.HasPrefix(ck, "X-Amz-Checksum-"):
			alg := strings.TrimPrefix(ck, "X-Amz-Checksum-")
			if strings.EqualFold(alg, "Type") || strings.EqualFold(alg, "Mode") {
				continue // the checksum's kind, not a checksum
			}
			sum, ok := amzChecksum(alg, body)
			if !ok {
				return false
			}
			header[k] = sum
		case ck == "X-Goog-Hash":
			sum, ok := googHash(v, body)
			if !ok {
				return false
			}
			header[k] = sum
		case ck == "Content-Md5":
			s := md5.Sum(body) // #nosec G401
			header[k] = base64.StdEncoding.EncodeToString(s[:])
		case ck == "Digest":
			sum, ok := rfc3230Digest(v, body)
			if !ok {
				return false
			}
			header[k] = sum
		case ck == "Content-Digest" || ck == "Repr-Digest":
			sum, ok := rfc9530Digest(v, body)
			if !ok {
				return false
			}
			header[k] = sum
		case ck == "Etag":
			if etagIsMD5(v, oldBody) {
				s := md5.Sum(body) // #nosec G401
				weak := ""
				if strings.HasPrefix(v, "W/") {
					weak = "W/"
				}
				header[k] = weak + `"` + hex.EncodeToString(s[:]) + `"`
			}
		}
	}
	return true
}

// servedEncoded reports whether the response carries a Content-Encoding other
// than identity.
func servedEncoded(header map[string]string) bool {
	for k, v := range header {
		if http.CanonicalHeaderKey(k) == "Content-Encoding" && v != "" && !strings.EqualFold(v, "identity") {
			return true
		}
	}
	return false
}

// hasIntegrityHeader reports whether header carries a digest of the body a
// client may check: every header reSign recomputes, an ETag only when it is
// the MD5 of oldBody.
func hasIntegrityHeader(header map[string]string, oldBody string) bool {
	for k, v := range header {
		ck := http.CanonicalHeaderKey(k)
		switch {
		case ck == "X-Amz-Checksum-Type", ck == "X-Amz-Checksum-Mode":
			// the checksum's kind, not a checksum
		case ck == "X-Amz-Crc32", ck == "Content-Md5", ck == "X-Goog-Hash", ck == "Digest",
			ck == "Content-Digest", ck == "Repr-Digest",
			strings.HasPrefix(ck, "X-Amz-Checksum-"):
			return true
		case ck == "Etag":
			if etagIsMD5(v, oldBody) {
				return true
			}
		}
	}
	return false
}

// etagIsMD5 reports whether the ETag v (strong or weak) is the hex MD5 of body.
func etagIsMD5(v, body string) bool {
	s := md5.Sum([]byte(body)) // #nosec G401
	return strings.Trim(strings.TrimPrefix(v, "W/"), `"`) == hex.EncodeToString(s[:])
}

var (
	castagnoli = crc32.MakeTable(crc32.Castagnoli)
	// nvme is CRC-64/NVME (S3's default checksum since 2025): polynomial
	// 0xad93d23594c93659, bit-reversed as package crc64 takes it.
	nvme = crc64.MakeTable(0x9a6c9329ac4bc9b5)
)

// amzChecksum is an S3 x-amz-checksum-<alg> value: the big-endian checksum,
// base64. An algorithm it cannot compute reports false.
func amzChecksum(alg string, body []byte) (string, bool) {
	switch strings.ToLower(alg) {
	case "crc32":
		return b64u32(crc32.ChecksumIEEE(body)), true
	case "crc32c":
		return b64u32(crc32.Checksum(body, castagnoli)), true
	case "crc64nvme":
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], crc64.Checksum(body, nvme))
		return base64.StdEncoding.EncodeToString(b[:]), true
	case "sha1":
		return b64sum(sha1.New(), body), true // #nosec G401
	case "sha256":
		return b64sum(sha256.New(), body), true
	}
	return "", false
}

func b64u32(v uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return base64.StdEncoding.EncodeToString(b[:])
}

func b64sum(h hash.Hash, body []byte) string {
	h.Write(body)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// googHash rewrites a GCS x-goog-hash list ("crc32c=...,md5=...").
func googHash(v string, body []byte) (string, bool) {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		name, _, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok {
			return "", false
		}
		switch strings.ToLower(name) {
		case "crc32c":
			parts[i] = "crc32c=" + b64u32(crc32.Checksum(body, castagnoli))
		case "md5":
			s := md5.Sum(body) // #nosec G401
			parts[i] = "md5=" + base64.StdEncoding.EncodeToString(s[:])
		default:
			return "", false
		}
	}
	return strings.Join(parts, ","), true
}

func digestFor(alg string, body []byte) (string, bool) {
	switch strings.ToLower(alg) {
	case "sha-256":
		return b64sum(sha256.New(), body), true
	case "sha-512":
		return b64sum(sha512.New(), body), true
	case "sha":
		return b64sum(sha1.New(), body), true // #nosec G401
	case "md5":
		s := md5.Sum(body) // #nosec G401
		return base64.StdEncoding.EncodeToString(s[:]), true
	}
	return "", false
}

// rfc3230Digest rewrites a Digest list ("SHA-256=...,MD5=...").
func rfc3230Digest(v string, body []byte) (string, bool) {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		alg, _, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok {
			return "", false
		}
		sum, ok := digestFor(alg, body)
		if !ok {
			return "", false
		}
		parts[i] = alg + "=" + sum
	}
	return strings.Join(parts, ","), true
}

// rfc9530Digest rewrites a Content-Digest / Repr-Digest dictionary
// ("sha-256=:...:, sha-512=:...:").
func rfc9530Digest(v string, body []byte) (string, bool) {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		alg, _, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || strings.EqualFold(alg, "sha") {
			return "", false
		}
		sum, ok := digestFor(alg, body)
		if !ok {
			return "", false
		}
		parts[i] = alg + "=:" + sum + ":"
	}
	return strings.Join(parts, ", "), true
}
