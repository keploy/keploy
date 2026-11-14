package testdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const (
	binaryReqBody  = "\x89PNG\r\n\x1a\n\x00\xff\xfe\x01upload"
	binaryRespBody = "\x08\x96\x01\x12\x04\xc3\x28\xff\xfeproto"
)

func bodyCase(req, resp string) *models.TestCase {
	return &models.TestCase{
		Version: models.GetVersion(), Kind: models.HTTP, Noise: map[string][]string{},
		HTTPReq:  models.HTTPReq{Method: "POST", URL: "http://app/bin", Body: req},
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: resp},
	}
}

func storedTests(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "test-set-0", "tests"))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A test case file has no reader to ask for a body object, and encoding/json
// writes U+FFFD for each byte of a string that is not UTF-8 and returns no
// error. So the JSON store refuses such a test case, says so, and leaves no file
// of it in tests/, not even the name it claimed; the test cases around it are
// stored.
func TestJSONStoreRefusesABodyThatIsNotUTF8(t *testing.T) {
	for _, c := range []struct {
		name, field string
		tc          *models.TestCase
	}{
		{"request", "the request body", bodyCase(binaryReqBody, "ok")},
		{"response", "the response body", bodyCase("ok", binaryRespBody)},
		{"both", "the request body", bodyCase(binaryReqBody, binaryRespBody)},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			core, logs := observer.New(zap.ErrorLevel)
			ts := NewWithFormat(zap.New(core), dir, yaml.FormatJSON)
			ctx := context.Background()

			before, after := bodyCase("before", "ok"), bodyCase("after", "ok")
			require.NoError(t, ts.InsertTestCase(ctx, before, "test-set-0", false))
			err := ts.InsertTestCase(ctx, c.tc, "test-set-0", false)
			require.ErrorIs(t, err, models.ErrTestCaseEncode)
			require.ErrorContains(t, err, c.field)
			require.ErrorContains(t, err, "storageFormat yaml")
			require.Empty(t, c.tc.Name, "a refused test case was given a name no file backs")
			require.NoError(t, ts.InsertTestCase(ctx, after, "test-set-0", false))

			require.Equal(t, []string{before.Name + ".json", after.Name + ".json"}, storedTests(t, dir))
			said := logs.FilterMessage("refusing to write a binary body as JSON").All()
			require.Len(t, said, 1)
			// The call by its shape (utils.URLLogMeta), never its URL.
			require.Equal(t, map[string]string{"method": "POST", "host": "app", "path": "/{}", "scheme": "http"}, said[0].ContextMap()["metadata"])
			for _, f := range storedTests(t, dir) {
				b, err := os.ReadFile(filepath.Join(dir, "test-set-0", "tests", f))
				require.NoError(t, err)
				require.False(t, strings.ContainsRune(string(b), '�'), "%s holds U+FFFD", f)
			}
		})
	}
}

// Only a body that is not UTF-8 is refused: a text body stays, the replacement
// character itself among them, and YAML takes the bytes as it always did.
func TestJSONStoreKeepsWhatItCanHold(t *testing.T) {
	dir := t.TempDir()
	ts := NewWithFormat(zap.NewNop(), dir, yaml.FormatJSON)
	tc := bodyCase("plain � é", "")
	require.NoError(t, ts.InsertTestCase(context.Background(), tc, "test-set-0", false))
	require.Equal(t, []string{tc.Name + ".json"}, storedTests(t, dir))

	dir = t.TempDir()
	ts = NewWithFormat(zap.NewNop(), dir, yaml.FormatYAML)
	require.NoError(t, ts.InsertTestCase(context.Background(), bodyCase(binaryReqBody, binaryRespBody), "test-set-0", false))
	got, err := ts.GetTestCases(context.Background(), "test-set-0")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, binaryReqBody, got[0].HTTPReq.Body)
	require.Equal(t, binaryRespBody, got[0].HTTPResp.Body)
}

// A body over the size bound is not in the document by the time it is encoded:
// a request's bytes go to an asset file, which holds them exactly, and a
// response's are dropped by size. Neither is refused.
func TestJSONStoreStoresALargeBinaryBodyOutsideTheDocument(t *testing.T) {
	big := strings.Repeat(binaryReqBody, LargeBodyThreshold/len(binaryReqBody)+1)
	dir := t.TempDir()
	ts := NewWithFormat(zap.NewNop(), dir, yaml.FormatJSON)
	tc := bodyCase(big, big)
	require.NoError(t, ts.InsertTestCase(context.Background(), tc, "test-set-0", false))
	require.Equal(t, []string{tc.Name + ".json"}, storedTests(t, dir))
	require.True(t, tc.HTTPResp.BodySkipped)
	b, err := os.ReadFile(filepath.Join(dir, tc.HTTPReq.BodyRef.Path))
	require.NoError(t, err)
	require.Equal(t, big, string(b))
}

// Each writer of a JSON test case refuses it, not only the recording's: one read
// back from YAML and written out as JSON would lose its bytes the same way. The
// file already there is left as it was.
func TestJSONStoreRefusesOnEveryWrite(t *testing.T) {
	dir := t.TempDir()
	ts := NewWithFormat(zap.NewNop(), dir, yaml.FormatJSON)
	ctx := context.Background()
	stored := bodyCase("text", "ok")
	require.NoError(t, ts.InsertTestCase(ctx, stored, "test-set-0", false))
	path := filepath.Join(dir, "test-set-0", "tests", stored.Name+".json")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	binary := bodyCase(binaryReqBody, "ok")
	binary.Name = stored.Name
	require.ErrorIs(t, ts.UpdateTestCase(ctx, binary, "test-set-0", false), models.ErrTestCaseEncode)
	_, err = ts.TestCaseFile(ctx, binary, "test-set-0")
	require.ErrorIs(t, err, models.ErrTestCaseEncode)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

// The refusal is the test case's own fault and no other error is, so a recorder
// tells it from a full disk; a test case of another kind is untouched by it.
func TestEncodeTestcaseJSONTagsOnlyTheBody(t *testing.T) {
	_, err := EncodeTestcaseJSON(*bodyCase(binaryReqBody, "ok"), zap.NewNop())
	require.ErrorIs(t, err, models.ErrTestCaseEncode)
	_, err = EncodeTestcaseJSON(*bodyCase("ok", binaryRespBody), zap.NewNop())
	require.ErrorIs(t, err, models.ErrTestCaseEncode)
	_, err = EncodeTestcaseJSON(models.TestCase{Kind: "nope"}, zap.NewNop())
	require.Error(t, err)
	require.False(t, errors.Is(err, models.ErrTestCaseEncode))
	doc, err := EncodeTestcaseJSON(*bodyCase("ok", "ok"), zap.NewNop())
	require.NoError(t, err)
	require.NotEmpty(t, doc.Spec)
}
