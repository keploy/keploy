package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/md5" //nolint:gosec // a digest a server sent, not security
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	yamlLib "gopkg.in/yaml.v3"
)

// The digests a client checks over a response's body, computed here apart
// from pkg/bodydigest.
func sdMD5B64(b []byte) string { s := md5.Sum(b); return base64.StdEncoding.EncodeToString(s[:]) } //nolint:gosec
func sdMD5Hex(b []byte) string { s := md5.Sum(b); return hex.EncodeToString(s[:]) }                //nolint:gosec
func sdSHA256B64(b []byte) string {
	s := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(s[:])
}
func sdCRC32B64(b []byte) string {
	return base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(b)))
}
func sdCRC32CB64(b []byte) string {
	return base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))))
}
func sdCRC64NVMEB64(b []byte) string {
	return base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint64(nil, crc64.Checksum(b, crc64.MakeTable(0x9a6c9329ac4bc9b5))))
}
func sdCRC32Dec(b []byte) string { return strconv.FormatUint(uint64(crc32.ChecksumIEEE(b)), 10) }

// recordAndServe records the exchange with both record paths, writes each
// mock as mocks.yaml does and reads it back, rewrites its response body with
// rewrite (as obfuscation or encryption does, between record and replay), and
// serves it: what a client reads, by record path.
func recordAndServe(t *testing.T, req, resp []byte, rewrite func(string) string) map[string]*http.Response {
	t.Helper()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	v2, err := h.buildHTTPMock(&FinalHTTP{Req: req, Resp: resp, ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
		80, "test-conn", models.OutgoingOptions{})
	if err != nil || v2 == nil {
		t.Fatalf("buildHTTPMock = (%v, %v)", v2, err)
	}
	legacy := runLegacyParseFinalHTTP(t, h, req, resp, 80, "test-conn")
	out := map[string]*http.Response{}
	for path, recorded := range map[string]*models.Mock{"V2 record": v2, "legacy record": legacy} {
		doc, err := mockdb.EncodeMock(recorded, zap.NewNop())
		if err != nil {
			t.Fatalf("%s: EncodeMock: %v", path, err)
		}
		onDisk, err := yamlLib.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var readBack yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal(onDisk, &readBack); err != nil {
			t.Fatal(err)
		}
		mocks, err := mockdb.DecodeMocks([]*yaml.NetworkTrafficDoc{&readBack}, zap.NewNop())
		if err != nil || len(mocks) != 1 {
			t.Fatalf("%s: DecodeMocks = (%d mocks, %v)", path, len(mocks), err)
		}
		mocks[0].Spec.HTTPResp.Body = rewrite(mocks[0].Spec.HTTPResp.Body)
		served, err := h.buildMockResponseBytes(mocks[0])
		if err != nil {
			t.Fatalf("%s: buildMockResponseBytes: %v", path, err)
		}
		got, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(served)), nil)
		if err != nil {
			t.Fatalf("%s: the served response does not parse: %v\n%s", path, err, served)
		}
		out[path] = got
	}
	return out
}

func rawResponse(headers []string, body []byte) []byte {
	return []byte("HTTP/1.1 200 OK\r\n" + strings.Join(headers, "\r\n") + "\r\n\r\n" + string(body))
}

// A DIGEST A CLIENT CHECKS is served over the body a replay sends. When the
// recorded body is rewritten between record and replay (obfuscation redacts a
// secret in it, encryption gives it back re-serialised, a variable changes
// it), each digest header the body matched as its server sent it is
// recomputed over the body sent: Content-MD5, an MD5 ETag (the S3 Java SDK v1
// checks it), a Digest, S3 GetObject's x-amz-checksum-* (the AWS SDKs check
// them by default) and DynamoDB's x-amz-crc32 (aws-sdk-go v1 and boto3 check
// it). Served as recorded, aws-sdk-go-v2 failed GetObject ("checksum did not
// match"), and aws-sdk-go v1 and boto3 failed GetItem. A header the body did
// not match at record (a server's own wrong digest, a resource version) is no
// digest of it, and stays as recorded.
func TestARecordedDigestIsServedOverTheBodySent(t *testing.T) {
	secret := "sk_live_" + "Fz9QkT3mWx7LpR2vNc8Hb4Jd"
	body := []byte(`{"service":"billing","key":"` + secret + `"}`)
	wrongMD5 := sdMD5B64([]byte("a body it was not computed over"))
	resp := rawResponse([]string{
		"Content-Type: application/json",
		"Content-Length: " + strconv.Itoa(len(body)),
		"Content-MD5: " + sdMD5B64(body),
		`ETag: "` + sdMD5Hex(body) + `"`,
		"Digest: sha-256=" + sdSHA256B64(body),
		"X-Amz-Checksum-Crc32: " + sdCRC32B64(body),
		"X-Amz-Checksum-Crc32c: " + sdCRC32CB64(body),
		"X-Amz-Checksum-Crc64nvme: " + sdCRC64NVMEB64(body),
		"X-Amz-Checksum-Sha256: " + sdSHA256B64(body),
		"X-Amz-Crc32: " + sdCRC32Dec(body),
		"X-Request-Version: 7",
	}, body)
	req := []byte("GET /cfg-bucket/app/config HTTP/1.1\r\nHost: s3.us-east-1.amazonaws.com\r\n\r\n")
	redact := func(b string) string { return strings.ReplaceAll(b, secret, "REDACTED-1") }
	for path, got := range recordAndServe(t, req, resp, redact) {
		sent, err := io.ReadAll(got.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(sent), "REDACTED-1") {
			t.Fatalf("%s: the rewritten body was not served: %s", path, sent)
		}
		for name, want := range map[string]string{
			"Content-MD5":              sdMD5B64(sent),
			"ETag":                     `"` + sdMD5Hex(sent) + `"`,
			"Digest":                   "sha-256=" + sdSHA256B64(sent),
			"X-Amz-Checksum-Crc32":     sdCRC32B64(sent),
			"X-Amz-Checksum-Crc32c":    sdCRC32CB64(sent),
			"X-Amz-Checksum-Crc64nvme": sdCRC64NVMEB64(sent),
			"X-Amz-Checksum-Sha256":    sdSHA256B64(sent),
			"X-Amz-Crc32":              sdCRC32Dec(sent),
			"X-Request-Version":        "7",
		} {
			if v := got.Header.Get(name); v != want {
				t.Errorf("%s: served %s %q; over the body sent it is %q", path, name, v, want)
			}
		}
	}

	// A server's own wrong digest is no digest of the body: served as
	// recorded, wrong, as the server sent it.
	resp = rawResponse([]string{"Content-Type: application/json", "Content-Length: " + strconv.Itoa(len(body)), "Content-MD5: " + wrongMD5}, body)
	for path, got := range recordAndServe(t, req, resp, redact) {
		if v := got.Header.Get("Content-MD5"); v != wrongMD5 {
			t.Errorf("%s: a Content-MD5 the body did not match at record was served %q; want it as recorded, %q", path, v, wrongMD5)
		}
	}
}

// A digest over the body as it went on the wire, encoded (an ETag a server
// computes after gzip), is recomputed over the bytes keploy sends, which its
// own compressor writes: the body recorded decoded is encoded again.
func TestADigestOverTheEncodedBodyIsServedOverTheBytesSent(t *testing.T) {
	body := []byte(`{"items":[1,2,3],"note":"` + strings.Repeat("x", 64) + `"}`)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(body)
	_ = w.Close()
	resp := rawResponse([]string{
		"Content-Type: application/json",
		"Content-Encoding: gzip",
		"Content-Length: " + strconv.Itoa(gz.Len()),
		`ETag: "` + sdMD5Hex(gz.Bytes()) + `"`,
	}, gz.Bytes())
	req := []byte("GET /items HTTP/1.1\r\nHost: api\r\nAccept-Encoding: gzip\r\n\r\n")
	for path, got := range recordAndServe(t, req, resp, func(b string) string { return b }) {
		sent, err := io.ReadAll(got.Body)
		if err != nil {
			t.Fatal(err)
		}
		if v, want := got.Header.Get("ETag"), `"`+sdMD5Hex(sent)+`"`; v != want {
			t.Errorf("%s: served ETag %s; over the gzip keploy sent it is %s", path, v, want)
		}
	}
}

// A DIGEST OVER THE DECODED BODY, sent gzip-encoded (Express's or Spring's
// ETag and a Content-MD5, computed before the compression middleware), is
// recomputed over the decoded body sent, not over keploy's gzip of it.
func TestADigestOverTheDecodedBodyIsServedOverTheDecodedBodySent(t *testing.T) {
	secret := "sk_live_" + "Fz9QkT3mWx7LpR2vNc8Hb4Jd"
	body := []byte(`{"key":"` + secret + `","pad":"` + strings.Repeat("y", 80) + `"}`)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(body)
	_ = w.Close()
	resp := rawResponse([]string{
		"Content-Type: application/json",
		"Content-Encoding: gzip",
		"Content-Length: " + strconv.Itoa(gz.Len()),
		`ETag: "` + sdMD5Hex(body) + `"`,
		"Content-MD5: " + sdMD5B64(body),
	}, gz.Bytes())
	req := []byte("GET /x HTTP/1.1\r\nHost: api\r\nAccept-Encoding: gzip\r\n\r\n")
	for path, got := range recordAndServe(t, req, resp, func(b string) string { return strings.ReplaceAll(b, secret, "REDACTED-1") }) {
		raw, _ := io.ReadAll(got.Body)
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		sent, _ := io.ReadAll(zr)
		if !strings.Contains(string(sent), "REDACTED-1") {
			t.Fatalf("%s: the rewritten body was not served", path)
		}
		if v, want := got.Header.Get("ETag"), `"`+sdMD5Hex(sent)+`"`; v != want {
			t.Errorf("%s: served ETag %s; over the decoded body sent it is %s", path, v, want)
		}
		if v, want := got.Header.Get("Content-MD5"), sdMD5B64(sent); v != want {
			t.Errorf("%s: served Content-MD5 %s; over the decoded body sent it is %s", path, v, want)
		}
		if cl := got.Header.Get("Content-Length"); cl != strconv.Itoa(len(raw)) {
			t.Errorf("%s: Content-Length %s, sent %d", path, cl, len(raw))
		}
	}
}

// A DIGEST OF NO BODY KEPLOY CAN COMPUTE stays as recorded: an S3 multipart
// ETag ("<md5 of the parts' md5s>-<parts>") and a composite checksum.
func TestACompositeDigestIsServedAsRecorded(t *testing.T) {
	body := []byte(`{"a":"sk_live_` + `Fz9QkT3mWx7LpR2vNc8Hb4Jd"}`)
	multipart := `"` + sdMD5Hex(body) + `-3"`
	composite := sdCRC32B64(body) + "-3"
	resp := rawResponse([]string{"Content-Type: application/json", "Content-Length: " + strconv.Itoa(len(body)), "ETag: " + multipart,
		"X-Amz-Checksum-Crc32: " + composite, "X-Amz-Checksum-Type: COMPOSITE"}, body)
	req := []byte("GET /b/k HTTP/1.1\r\nHost: s3\r\n\r\n")
	for path, got := range recordAndServe(t, req, resp, func(b string) string { return strings.ReplaceAll(b, "sk_live_", "REDACTED") }) {
		if v := got.Header.Get("ETag"); v != multipart {
			t.Errorf("%s: multipart ETag served %s; want it as recorded, %s", path, v, multipart)
		}
		if v := got.Header.Get("X-Amz-Checksum-Crc32"); v != composite {
			t.Errorf("%s: composite checksum served %s; want it as recorded, %s", path, v, composite)
		}
	}
}

// A BODY ITS CLIENT READ ONLY IN PART notes no digest: no digest is of a part.
func TestABodyReadInPartNotesNoDigest(t *testing.T) {
	body := []byte(`{"items":[1,2,3]}`)
	resp := rawResponse([]string{"Content-Type: application/json", "Content-Length: " + strconv.Itoa(len(body)), "Content-MD5: " + sdMD5B64(body)}, body)
	req := []byte("GET /items HTTP/1.1\r\nHost: api\r\n\r\n")
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	for _, inPart := range []bool{false, true} {
		m, err := h.buildHTTPMock(&FinalHTTP{Req: req, Resp: resp, RespReadInPart: inPart, ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
			80, "test-conn", models.OutgoingOptions{})
		if err != nil || m == nil {
			t.Fatalf("buildHTTPMock = (%v, %v)", m, err)
		}
		note, noted := m.Spec.Metadata[models.MetadataBodyDigestsKey]
		if inPart && noted {
			t.Errorf("a body read in part noted %q", note)
		}
		if !inPart && note != "content-md5=content-md5" {
			t.Errorf("a whole body noted %q; want its Content-MD5", note)
		}
	}
}
