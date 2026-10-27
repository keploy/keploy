package bodydigest

import (
	"bytes"
	"crypto/md5" //nolint:gosec // an ETag form, not security
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"hash/crc64"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const recorded, other = `{"id":"usr_REC01"}`, `{"id":"usr_LIVE002"}`

func b64Of(sum []byte) string { return base64.StdEncoding.EncodeToString(sum) }

func md5b64(b string) string    { s := md5.Sum([]byte(b)); return b64Of(s[:]) } //nolint:gosec
func sha1b64(b string) string   { s := sha1.Sum([]byte(b)); return b64Of(s[:]) }
func sha256b64(b string) string { s := sha256.Sum256([]byte(b)); return b64Of(s[:]) }
func sha512b64(b string) string { s := sha512.Sum512([]byte(b)); return b64Of(s[:]) }
func sha1hex(b string) string   { s := sha1.Sum([]byte(b)); return hex.EncodeToString(s[:]) }
func sha256hex(b string) string { s := sha256.Sum256([]byte(b)); return hex.EncodeToString(s[:]) }
func md5hex(b string) string    { s := md5.Sum([]byte(b)); return hex.EncodeToString(s[:]) } //nolint:gosec

// EACH DIGEST FORM a server writes over the body it sends is recognised in the
// value recorded for a body, and rewritten, in the same form, for another
// body: the letter case of a hex ETag, its weakness, an Express, Rails or
// Spring tag, Content-MD5, and every item of a Digest list, sha-512 included.
func TestForm_RecomputesEachDigestFormForAnotherBody(t *testing.T) {
	for _, c := range []struct {
		name, header string
		value        func(body string) string
	}{
		{"a lower-case hex SHA-1 ETag", "etag", func(b string) string { return `"` + sha1hex(b) + `"` }},
		{"an upper-case hex SHA-256 ETag", "etag", func(b string) string { return `"` + strings.ToUpper(sha256hex(b)) + `"` }},
		{"an upper-case hex MD5 weak ETag", "etag", func(b string) string { return `W/"` + strings.ToUpper(md5hex(b)) + `"` }},
		{"Express's ETag", "etag", func(b string) string { return fmt.Sprintf(`W/"%x-%s"`, len(b), sha1b64(b)[:27]) }},
		{"Rails' Rack::ETag", "etag", func(b string) string { return `W/"` + sha256hex(b)[:32] + `"` }},
		{"Spring's ShallowEtagHeaderFilter", "etag", func(b string) string { return `"0` + md5hex(b) + `"` }},
		{"Content-MD5", "content-md5", md5b64},
		{"a Digest of sha-512", "digest", func(b string) string { return "sha-512=" + sha512b64(b) }},
		{"a Digest list of two items", "digest", func(b string) string { return "SHA-256=" + sha256b64(b) + ", md5=" + md5b64(b) }},
		{"a Digest of sha (SHA-1)", "digest", func(b string) string { return "sha=" + sha1b64(b) }},
		{"a Content-Digest", "content-digest", func(b string) string { return "sha-512=:" + sha512b64(b) + ":" }},
		{"a Repr-Digest list", "repr-digest", func(b string) string { return "sha-256=:" + sha256b64(b) + ":, sha-512=:" + sha512b64(b) + ":" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.True(t, IsHeader(c.header))
			form, ok := Form(c.header, c.value(recorded), recorded)
			require.True(t, ok, "%s is a digest of the body", c.value(recorded))
			assert.Equal(t, c.value(other), form(other))
		})
	}
}

// A VALUE THAT IS NO DIGEST OF THE BODY is not one: a resource version, a
// hash of another body, a list one item of which is not a digest of it, an
// algorithm keploy does not know, an unquoted ETag.
func TestForm_AValueThatIsNoDigestOfTheBodyIsNotOne(t *testing.T) {
	for _, c := range []struct{ name, header, value string }{
		{"a version ETag", "etag", `"7"`},
		{"a hash of another body", "etag", `"` + sha1hex(other) + `"`},
		{"an unquoted ETag", "etag", sha1hex(recorded)},
		{"a list with one foreign item", "digest", "sha-256=" + sha256b64(recorded) + ", md5=" + md5b64(other)},
		{"an unknown algorithm", "digest", "crc32c=AAAAAA=="},
		{"an item that is no alg=value", "digest", "sha-256"},
		{"Content-MD5 of another body", "content-md5", md5b64(other)},
		{"a header that is no digest", "content-type", "application/json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, ok := Form(c.header, c.value, recorded)
			assert.False(t, ok)
		})
	}
	assert.False(t, IsHeader("content-length"), "Content-Length follows the body by length alone")
}

// awsChecksum is an x-amz-checksum-<alg> value, computed here apart from the
// package: base64 of the big-endian checksum.
func awsChecksum(alg, body string) string {
	b := []byte(body)
	var sum []byte
	switch alg {
	case "crc32":
		sum = binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(b))
	case "crc32c":
		sum = binary.BigEndian.AppendUint32(nil, crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)))
	case "crc64nvme":
		sum = binary.BigEndian.AppendUint64(nil, crc64.Checksum(b, crc64.MakeTable(0x9a6c9329ac4bc9b5)))
	case "sha1":
		s := sha1.Sum(b)
		sum = s[:]
	case "sha256":
		s := sha256.Sum256(b)
		sum = s[:]
	}
	return b64Of(sum)
}

// THE AWS CHECKSUMS are digests too: S3 GetObject's x-amz-checksum-<alg>
// (every algorithm the SDKs check) and DynamoDB's x-amz-crc32 (decimal),
// recognised over the body they were computed over and recomputed over
// another. Known vectors pin the CRC-64/NVME and the CRC-32C, which a wrong
// table would silently change.
func TestTheAWSChecksumsAreDigestsOfTheBody(t *testing.T) {
	for _, alg := range []string{"crc32", "crc32c", "crc64nvme", "sha1", "sha256"} {
		name := "x-amz-checksum-" + alg
		require.True(t, IsHeader(name), name)
		form, ok := Form(name, awsChecksum(alg, recorded), recorded)
		require.True(t, ok, "%s of the body", name)
		assert.Equal(t, awsChecksum(alg, other), form(other), name)
		_, ok = Form(name, awsChecksum(alg, other), recorded)
		assert.False(t, ok, "%s of another body is no digest of this one", name)
	}
	// Vectors: the check values of "123456789".
	for spec, want := range map[string]string{
		"amz/crc64nvme": "rosUhgp5mIg=", // CRC-64/NVME 0xae8b14860a799888
		"amz/crc32c":    "4waSgw==",     // CRC-32C 0xe3069283
		"amz/crc32":     "y/Q5Jg==",     // CRC-32 0xcbf43926
		"amz-crc32":     "3421780262",   // CRC-32 0xcbf43926, in decimal
	} {
		got, ok := Compute(spec, "123456789")
		assert.True(t, ok, spec)
		assert.Equal(t, want, got, spec)
	}
	crcOf := func(b string) string { return strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(b))), 10) }
	require.True(t, IsHeader("x-amz-crc32"))
	form, ok := Form("x-amz-crc32", crcOf(recorded), recorded)
	require.True(t, ok)
	assert.Equal(t, crcOf(other), form(other))

	// A composite S3 checksum (of a multipart upload's parts) is no
	// checksum of the body.
	_, ok = Form("x-amz-checksum-crc32", awsChecksum("crc32", recorded)+"-3", recorded)
	assert.False(t, ok)
	assert.False(t, IsHeader("x-amz-checksum-type"))
	assert.False(t, IsHeader("x-amz-checksum-md5"))
}

// A digest's form, written as a spec, is read back the same: Spec and Compute
// agree with Form for every form, and a spec Compute does not know is none.
func TestASpecIsReadBackAsTheFormItNames(t *testing.T) {
	for _, c := range []struct{ header, value string }{
		{"etag", `"` + sha1hex(recorded) + `"`},
		{"etag", `W/"` + strings.ToUpper(md5hex(recorded)) + `"`},
		{"etag", fmt.Sprintf(`W/"%x-%s"`, len(recorded), sha1b64(recorded)[:27])},
		{"etag", `W/"` + sha256hex(recorded)[:32] + `"`},
		{"etag", `"0` + md5hex(recorded) + `"`},
		{"content-md5", md5b64(recorded)},
		{"digest", "SHA-256=" + sha256b64(recorded) + ", md5=" + md5b64(recorded)},
		{"repr-digest", "sha-256=:" + sha256b64(recorded) + ":, sha-512=:" + sha512b64(recorded) + ":"},
		{"x-amz-checksum-sha256", awsChecksum("sha256", recorded)},
	} {
		spec, ok := Spec(c.header, c.value, recorded)
		require.True(t, ok, "%s: %s", c.header, c.value)
		got, ok := Compute(spec, recorded)
		require.True(t, ok, spec)
		assert.Equal(t, c.value, got, "%s read back over the recorded body", spec)
		form, _ := Form(c.header, c.value, recorded)
		got, _ = Compute(spec, other)
		assert.Equal(t, form(other), got, "%s over another body", spec)
	}
	for _, spec := range []string{"", "etag", "etag/bogus/s", "list/rot13", "amz/md5", "nope"} {
		_, ok := Compute(spec, recorded)
		assert.False(t, ok, spec)
	}
}

// A NOTE names the digests the body matched as its server sent it, decoded or
// on the wire (encoded, "~"), and no other header: a digest that matched
// neither (a server's own wrong one, a resource version) is not noted. Apply
// recomputes exactly those over the body sent, in the bytes they were noted
// over, and leaves every other header as recorded: one the note does not
// name, one sent with two values, one sent under two spellings.
func TestANoteRecomputesWhatTheBodyMatchedAndNothingElse(t *testing.T) {
	wire := "gzipped:" + recorded
	header := map[string]string{
		"Content-MD5":          md5b64(recorded),
		"ETag":                 `"` + md5hex(wire) + `"`,
		"X-Amz-Checksum-Crc32": awsChecksum("crc32", recorded),
		"Digest":               "sha-256=" + sha256b64("a body it was not computed over"),
		"X-Amz-Crc32":          "7",
		"Content-Type":         "application/json",
	}
	note := Note(header, []byte(recorded), []byte(wire))
	assert.Equal(t, `content-md5=content-md5;etag=~etag/md5/s;x-amz-checksum-crc32=amz/crc32`, note)
	assert.Empty(t, Note(map[string]string{"X-Amz-Crc32": "7"}, []byte(recorded), []byte(recorded)))

	sentWire := "gzipped:" + other
	wireHeader := map[string][]string{
		"Content-Md5":          {md5b64(recorded)},
		"Etag":                 {`"` + md5hex(wire) + `"`},
		"X-Amz-Checksum-Crc32": {awsChecksum("crc32", recorded)},
		"Digest":               {header["Digest"]},
		"X-Amz-Crc32":          {"7"},
	}
	Apply(note, wireHeader, other, sentWire)
	assert.Equal(t, []string{md5b64(other)}, wireHeader["Content-Md5"])
	assert.Equal(t, []string{`"` + md5hex(sentWire) + `"`}, wireHeader["Etag"], "over the bytes sent, encoded")
	assert.Equal(t, []string{awsChecksum("crc32", other)}, wireHeader["X-Amz-Checksum-Crc32"])
	assert.Equal(t, []string{header["Digest"]}, wireHeader["Digest"], "not noted: as recorded")
	assert.Equal(t, []string{"7"}, wireHeader["X-Amz-Crc32"], "not noted: as recorded")

	twice := map[string][]string{"Content-Md5": {"a", "b"}}
	Apply(note, twice, other, other)
	assert.Equal(t, []string{"a", "b"}, twice["Content-Md5"])

	// A Digest list on two lines is one list (RFC 9110 5.3): noted as the
	// recorder joins it, and sent again as one line over the body sent.
	twoLines := "sha-256=" + sha256b64(recorded) + ", md5=" + md5b64(recorded)
	listNote := Note(map[string]string{"Digest": twoLines}, []byte(recorded), []byte(recorded))
	assert.Equal(t, "digest=list/sha-256,md5", listNote)
	lines := map[string][]string{"Digest": {"sha-256=" + sha256b64(recorded), "md5=" + md5b64(recorded)}}
	Apply(listNote, lines, other, other)
	assert.Equal(t, []string{"sha-256=" + sha256b64(other) + ", md5=" + md5b64(other)}, lines["Digest"])
	spelled := map[string][]string{"Content-Md5": {"a"}, "content-md5": {"b"}}
	Apply(note, spelled, other, other)
	assert.Equal(t, map[string][]string{"Content-Md5": {"a"}, "content-md5": {"b"}}, spelled)
	Apply("", wireHeader, recorded, recorded) // no note: nothing
}

// countSums counts each digest the package computes while f runs.
func countSums(t *testing.T, f func()) map[string]int {
	t.Helper()
	got := map[string]int{}
	observeSum = func(alg string) { got[alg]++ }
	t.Cleanup(func() { observeSum = nil })
	f()
	observeSum = nil
	return got
}

// allocatedBy is how many bytes f allocates.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A NOTE COSTS NOTHING for a response with no digest header: the body is not
// copied or hashed, however large. A recorded body can be as large as
// pkg.MaxDecompressedSize (100 MiB). Before, each record path converted the
// body and its encoded bytes to strings first, about 74 MB on a 32 MiB body.
func TestANoteOfAResponseWithNoDigestHeaderCostsNothing(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 16<<20)
	header := map[string]string{"Content-Type": "application/octet-stream", "Content-Length": strconv.Itoa(len(body)), "X-Request-Id": "r-1"}
	var note string
	sums := countSums(t, func() {
		if n := allocatedBy(func() { note = Note(header, body, body) }); n > 64<<10 {
			t.Errorf("Note allocated %d bytes for a %d-byte body with no digest header", n, len(body))
		}
	})
	assert.Empty(t, note)
	assert.Empty(t, sums, "no digest is computed")
}

// A VALUE OF NO DIGEST'S SHAPE is told no digest without hashing the body: a
// resource version, an S3 multipart ETag ("<md5>-<parts>") or composite
// checksum, a Content-MD5 or a Digest item of another length, an x-amz-crc32
// that is no number, an Express-like tag of another length. Before, an
// opaque multipart ETag had the body hashed six times (every ETag form),
// once more over the encoded bytes: about 196 ms and 275 MB on 32 MiB.
func TestAValueOfNoDigestsShapeIsNotHashed(t *testing.T) {
	body := bytes.Repeat([]byte("y"), 4<<20)
	for name, h := range map[string]map[string]string{
		"a version ETag":            {"ETag": `"7"`},
		"an S3 multipart ETag":      {"ETag": `"0123456789abcdef0123456789abcdef-12"`},
		"a weak multipart ETag":     {"ETag": `W/"0123456789abcdef0123456789abcdef-3"`},
		"an Express tag of another": {"ETag": `W/"1f-0123456789abcdefghijklmnopq"`},
		"a composite checksum":      {"X-Amz-Checksum-Crc32": "AAAAAA==-3"},
		"a short Content-MD5":       {"Content-MD5": "AAAA"},
		"a Digest item too short":   {"Digest": "sha-256=AAAA"},
		"an x-amz-crc32 of letters": {"X-Amz-Crc32": "abc"},
	} {
		t.Run(name, func(t *testing.T) {
			wire := append([]byte("gz:"), body...)
			var note string
			sums := countSums(t, func() { note = Note(h, body, wire) })
			assert.Empty(t, note)
			assert.Empty(t, sums, "no digest is computed for a value of no digest's shape")
		})
	}
}

// EACH DIGEST IS COMPUTED ONCE over a body, however many headers carry it: an
// MD5 ETag, Content-MD5 and a Digest md5 item share one MD5, Rails' tag and
// a SHA-256 ETag one SHA-256, Express's tag and an x-amz-checksum-sha1 one
// SHA-1. Apply computes each once too.
func TestEachDigestIsComputedOnceOverABody(t *testing.T) {
	header := map[string]string{
		"ETag":                  `"` + md5hex(recorded) + `"`,
		"Content-MD5":           md5b64(recorded),
		"Digest":                "md5=" + md5b64(recorded) + ", sha-256=" + sha256b64(recorded),
		"X-Amz-Checksum-Sha256": awsChecksum("sha256", recorded),
		"X-Amz-Checksum-Sha1":   awsChecksum("sha1", recorded),
	}
	var note string
	sums := countSums(t, func() { note = Note(header, []byte(recorded), []byte(recorded)) })
	assert.Equal(t, map[string]int{"md5": 1, "sha256": 1, "sha1": 1}, sums)
	wire := map[string][]string{}
	for k, v := range header {
		wire[k] = []string{v}
	}
	sums = countSums(t, func() { Apply(note, wire, other, other) })
	assert.Equal(t, map[string]int{"md5": 1, "sha256": 1, "sha1": 1}, sums)
	assert.Equal(t, []string{md5b64(other)}, wire["Content-MD5"])
	assert.Equal(t, []string{`"` + md5hex(other) + `"`}, wire["ETag"])
}

// A NOTE WITHOUT the headers a replay sets itself names the others, and Noted
// reads a note.
func TestANoteWithoutTheHeadersAReplaySets(t *testing.T) {
	note := "content-md5=content-md5;etag=~etag/md5/s;x-amz-checksum-crc32=amz/crc32"
	assert.Equal(t, "content-md5=content-md5;x-amz-checksum-crc32=amz/crc32", Without(note, map[string]bool{"etag": true}))
	assert.Equal(t, note, Without(note, nil))
	assert.Equal(t, "", Without("etag=etag/md5/s", map[string]bool{"etag": true}))
	assert.True(t, Noted(note, "etag"))
	assert.False(t, Noted(note, "digest"))
	assert.False(t, Noted("", "etag"))
}
