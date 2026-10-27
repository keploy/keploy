package http

import (
	"bytes"
	"mime/multipart"
	"net/textproto"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// An upload a test makes under an id it mints (atg-features' unlink specs:
// createRecordedTestSet sends testSetId as a form field, and a mocks.yaml
// that names its mock after it), then a call that links the set by the id.
const (
	uploadRec = "e2e-linkable-ab7cdc51"
	uploadF   = "e2e-linkable-1761279e"
	uploadCI  = "e2e-linkable-cafe0001"
)

func uploadYAML(id string) string {
	return "version: api.keploy.io/v1beta1\nkind: Http\nname: mock-" + id + "-0\nspec:\n  req:\n    method: GET\n"
}

// uploadForm is the upload's body by boundary: appId, testSetId and the file.
func uploadForm(boundary, field, file string) (string, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.SetBoundary(boundary)
	_ = w.WriteField("appId", "app-1")
	_ = w.WriteField("testSetId", field)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="mocks.yaml"`)
	h.Set("Content-Type", "application/x-yaml")
	pw, _ := w.CreatePart(h)
	_, _ = pw.Write([]byte(file))
	_ = w.Close()
	return b.String(), w.FormDataContentType()
}

func linkBody(id string) string {
	return `{"operationName":"LinkTestSet","variables":{"testSetId":"` + id + `"}}`
}

func linkAnswer(id string) string { return `{"data":{"linkTestSet":{"testSetId":"` + id + `"}}}` }

// uploadAnswer is the upload's answer, which names where it stored the file
// by the id, inside a longer text.
func uploadAnswer(id string) string {
	return `{"success":true,"objectKey":"mocks/app-1/` + id + `/mocks.bin","mockCount":1}`
}

func uploadRecording() []*models.Mock {
	body, ct := uploadForm("----WebKitFormBoundaryw04qWrYADoA1jKDw", uploadRec, uploadYAML(uploadRec))
	return []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/agent-proxy/mocks/upload", body, map[string]string{"Content-Type": ct}, uploadAnswer(uploadRec)),
		gql("mock-1", 1, linkBody(uploadRec), linkAnswer(uploadRec)),
	}
}

func uploadCall(boundary, field, file string) *req {
	body, ct := uploadForm(boundary, field, file)
	return live("POST", "/agent-proxy/mocks/upload", body, map[string]string{"Content-Type": ct})
}

// The id the test mints first appears in the upload, in its form field and
// inside the file it uploads: F binds it at the field and follows it inside
// the file, exactly, then the link carries it whole and its answer is given
// this run's id. The replays after F answer the upload only when its file
// holds the field's id where the recording held it, and nothing else changed.
func TestLearnMatchAnUploadedFileCarriesTheIdItsFormSends(t *testing.T) {
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	recs := uploadRecording()
	db := &learnDb{perTest: copies(recs), observe: tr.ServedMock}
	var served []*models.Mock
	for i, c := range []*req{uploadCall("----WebKitFormBoundaryF0F0F0F0F0F0F0F0", uploadF, uploadYAML(uploadF)), gqlLive(linkBody(uploadF))} {
		out, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || miss != nil || out == nil {
			t.Fatalf("F call %d: served %v miss %+v err %v", i, out, miss, err)
		}
		served = append(served, out)
	}
	if got := served[0].Spec.HTTPResp.Body; got != uploadAnswer(uploadF) {
		t.Fatalf("F answered the upload with %s", got)
	}
	if got := served[1].Spec.HTTPResp.Body; got != linkAnswer(uploadF) {
		t.Fatalf("F answered the link with %s", got)
	}
	up := db.consumed[0].Spec
	if want := (models.MockVariables{"testSetId": {Recorded: uploadRec, Bind: "form.testSetId", Use: []string{"form.file"}, Answer: []string{"body.objectKey"}}}); !reflect.DeepEqual(up.Learned(), want) || len(up.ReqBodyNoise) != 0 {
		t.Fatalf("the upload learned %+v and noise %v, want %+v", up.Learned(), up.ReqBodyNoise, want)
	}

	// The replays after F.
	mocks := learnedOnto(recs, db.consumed)
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	ok, stub, diag := matchVar(t, h, ci, uploadCall("----WebKitFormBoundaryC1C1C1C1C1C1C1C1", uploadCI, uploadYAML(uploadCI)))
	if !ok {
		t.Fatalf("the upload in CI: %+v", diag)
	}
	if got := stub.Spec.HTTPResp.Body; got != uploadAnswer(uploadCI) {
		t.Fatalf("the upload in CI was answered %s", got)
	}
	ok, stub, diag = matchVar(t, h, ci, gqlLive(linkBody(uploadCI)))
	if !ok {
		t.Fatalf("the link in CI: %+v", diag)
	}
	if got := stub.Spec.HTTPResp.Body; got != linkAnswer(uploadCI) {
		t.Fatalf("the link in CI was answered %s", got)
	}

	// A file that holds another id than its form field is a miss, never
	// answered.
	ci = &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	ok, _, diag = matchVar(t, h, ci, uploadCall("----WebKitFormBoundaryC2C2C2C2C2C2C2C2", uploadCI, uploadYAML(uploadRec)))
	if p := wantProblem(t, ok, diag, "so this call sends"); p.place != "form.file" {
		t.Fatalf("evidence at %s, want form.file", p.place)
	}
}

// A file whose content changed by more than the id its form sends (another
// line) is refused in F, for the file's change: a file is compared whole, and
// keploy learns no part of it as noise.
func TestLearnMatchAFileThatChangedOtherwiseIsRefused(t *testing.T) {
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	db := &learnDb{perTest: copies(uploadRecording()), observe: tr.ServedMock}
	out, miss, err := h.learnMatch(ctx, uploadCall("----WebKitFormBoundaryF0F0F0F0F0F0F0F0", uploadF, uploadYAML(uploadF)+"extra: line\n"), db, n, opts)
	if err != nil || out != nil || miss == nil {
		t.Fatalf("served %v miss %+v err %v; want a refusal", out, miss, err)
	}
	if !strings.HasPrefix(miss.res.Why, "body.file ") {
		t.Fatalf("refused for %q, want the file's change", miss.res.Why)
	}
}
