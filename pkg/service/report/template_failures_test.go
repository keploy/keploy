package report

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// templateOnlyFailure is a test case that failed only on a template variable
// place: every row of the comparison is normal (the id was under noise), and
// the reason is in template_failures.
func templateOnlyFailure() models.TestResult {
	return models.TestResult{
		Kind:       models.HTTP,
		Name:       "test-set-0",
		TestCaseID: "test-2",
		Status:     models.TestStatusFailed,
		Result: models.Result{
			StatusCode: models.IntResult{Normal: true, Expected: 200, Actual: 200},
			BodyResult: []models.BodyResult{{Normal: true, Type: models.JSON, Expected: `{"id":"usr_LIVE001"}`, Actual: `{"id":"usr_OTHER01"}`}},
		},
		FailureInfo: models.FailureInfo{Risk: models.High, Category: []models.FailureCategory{models.TemplateVariable}},
		TemplateFailures: []models.TemplateFailure{{
			Where: "body.id", Placeholder: "{{string .id}}", Why: `must equal id's value "usr_LIVE001"; the answer holds "usr_OTHER01"`,
		}},
	}
}

// keploy report says why a test case that failed only on a template variable
// place failed: the place, the placeholder and why, and the category.
func TestRenderSingleFailedTest_NamesTheTemplateVariablePlaces(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	var sb strings.Builder
	if err := r.renderSingleFailedTest(context.Background(), &sb, templateOnlyFailure()); err != nil {
		t.Fatalf("renderSingleFailedTest: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"Testrun failed for test-set-0/test-2 [HIGH-RISK] [TEMPLATE_VARIABLE]",
		"=== TEMPLATE VARIABLES THE ANSWER DID NOT SATISFY ===",
		`  body.id: {{string .id}} must equal id's value "usr_LIVE001"; the answer holds "usr_OTHER01"`,
		// Every row of the comparison is normal: it says so once, instead
		// of empty status, header and body sections.
		"The comparison of its status, headers and body found no difference: the template variable places above failed this test case.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	for _, never := range []string{"No differences found in status or headers.", "=== CHANGES WITHIN THE RESPONSE BODY ==="} {
		if strings.Contains(out, never) {
			t.Errorf("a test case only its variable places failed shows %q:\n%s", never, out)
		}
	}
	// A gRPC test case only its variable places failed reads the same, and
	// one whose grpc-status or a trailer also differs is not said to have no
	// difference. (The compact report renders no gRPC status row at all, as
	// on main: --full shows it.)
	grpcOnly := templateOnlyFailure()
	grpcOnly.Kind = models.GRPC_EXPORT
	sb.Reset()
	if err := r.renderSingleFailedTest(context.Background(), &sb, grpcOnly); err != nil {
		t.Fatalf("renderSingleFailedTest: %v", err)
	}
	if !strings.Contains(sb.String(), "found no difference: the template variable places above failed") {
		t.Errorf("a gRPC test case only its variable places failed:\n%s", sb.String())
	}
	grpcOnly.Result.StatusCode = models.IntResult{Normal: false, Expected: 0, Actual: 13}
	sb.Reset()
	if err := r.renderSingleFailedTest(context.Background(), &sb, grpcOnly); err != nil {
		t.Fatalf("renderSingleFailedTest: %v", err)
	}
	if strings.Contains(sb.String(), "found no difference") {
		t.Errorf("a gRPC test case whose grpc-status differs is said to have no difference:\n%s", sb.String())
	}
	grpcOnly.Result.StatusCode = models.IntResult{Normal: true}
	grpcOnly.Result.TrailerResult = []models.HeaderResult{{Normal: false, Expected: models.Header{Key: "x-trace", Value: []string{"a"}}, Actual: models.Header{Key: "x-trace", Value: []string{"b"}}}}
	sb.Reset()
	if err := r.renderSingleFailedTest(context.Background(), &sb, grpcOnly); err != nil {
		t.Fatalf("renderSingleFailedTest: %v", err)
	}
	if strings.Contains(sb.String(), "found no difference") {
		t.Errorf("a gRPC test case whose trailer differs is said to have no difference:\n%s", sb.String())
	}
	// A dependency that went missing is still said.
	lostDep := templateOnlyFailure()
	lostDep.Result.DepResult = []models.DepResult{missingDepRow()}
	sb.Reset()
	if err := r.renderSingleFailedTest(context.Background(), &sb, lostDep); err != nil {
		t.Fatalf("renderSingleFailedTest: %v", err)
	}
	if !strings.Contains(sb.String(), "found no difference") || !strings.Contains(sb.String(), "=== DEPENDENCY ASSERTIONS ===") {
		t.Errorf("a test case only its variable places failed drops its missing dependency:\n%s", sb.String())
	}
	headed := templateOnlyFailure()
	headed.Result.HeadersResult = []models.HeaderResult{{Normal: false,
		Expected: models.Header{Key: "X-User", Value: []string{"usr_LIVE001"}}, Actual: models.Header{Key: "X-User", Value: []string{"usr_OTHER02"}}}}
	sb.Reset()
	if err := r.renderSingleFailedTest(context.Background(), &sb, headed); err != nil {
		t.Fatalf("renderSingleFailedTest: %v", err)
	}
	if strings.Contains(sb.String(), "found no difference") || !strings.Contains(sb.String(), "X-User") {
		t.Errorf("a header that differs is said to have no difference:\n%s", sb.String())
	}
	sized := templateOnlyFailure()
	sized.Result.BodySizeResult = models.IntResult{Normal: false, Expected: 2048, Actual: 4096}
	sb.Reset()
	if err := r.renderSingleFailedTest(context.Background(), &sb, sized); err != nil {
		t.Fatalf("renderSingleFailedTest: %v", err)
	}
	if strings.Contains(sb.String(), "found no difference") || !strings.Contains(sb.String(), "Body Size Mismatch") {
		t.Errorf("a body of another size is said to have no difference:\n%s", sb.String())
	}
	bodied := templateOnlyFailure()
	bodied.Result.BodyResult = []models.BodyResult{{Normal: false, Type: models.JSON, Expected: `{"id":"usr_LIVE001","n":1}`, Actual: `{"id":"usr_OTHER01","n":2}`}}
	sb.Reset()
	if err := r.renderSingleFailedTest(context.Background(), &sb, bodied); err != nil {
		t.Fatalf("renderSingleFailedTest: %v", err)
	}
	if strings.Contains(sb.String(), "found no difference") || !strings.Contains(sb.String(), "Path: n") {
		t.Errorf("a body row that differs is said to have no difference:\n%s", sb.String())
	}
	plain := templateOnlyFailure()
	plain.TemplateFailures = nil
	sb.Reset()
	if err := r.renderSingleFailedTest(context.Background(), &sb, plain); err != nil {
		t.Fatalf("renderSingleFailedTest: %v", err)
	}
	if strings.Contains(sb.String(), "TEMPLATE VARIABLES") {
		t.Errorf("a test case without template failures shows the block:\n%s", sb.String())
	}
}

// JUnit's failure body names each template variable place too.
func TestBuildFailure_NamesTheTemplateVariablePlaces(t *testing.T) {
	f := buildFailure(templateOnlyFailure())
	want := `template variable body.id: {{string .id}} must equal id's value "usr_LIVE001"; the answer holds "usr_OTHER01"`
	if !strings.Contains(f.Text, want) {
		t.Errorf("the JUnit failure does not name the place: %q", f.Text)
	}
	if f.Message != "test assertion failed [HIGH-RISK]" {
		t.Errorf("message %q", f.Message)
	}
	// A test case refused when its set loaded was never sent: its message
	// says so, not that an assertion failed.
	if m := buildFailure(refusedCase()).Message; m != "test case refused when its test set loaded, not sent [HIGH-RISK]" {
		t.Errorf("a refused test case's JUnit message is %q", m)
	}
	// One that failed on a variable beside a refusal was sent.
	mixed := refusedCase()
	mixed.TemplateFailures = append(mixed.TemplateFailures, templateOnlyFailure().TemplateFailures...)
	if m := buildFailure(mixed).Message; m != "test assertion failed [HIGH-RISK]" {
		t.Errorf("a sent test case's JUnit message is %q", m)
	}
}

// keploy report --test-case shows a test case that failed on a template
// variable with its places, as keploy report does.
func TestPrintTests_NamesTheTemplateVariablePlaces(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)
	if err := r.printTests(context.Background(), []models.TestResult{templateOnlyFailure()}); err != nil {
		t.Fatal(err)
	}
	if want := `body.id: {{string .id}} must equal id's value "usr_LIVE001"; the answer holds "usr_OTHER01"`; !strings.Contains(buf.String(), want) {
		t.Errorf("keploy report --test-case does not name the place:\n%s", buf.String())
	}
}

// keploy report --full names the template variable places too, under the
// test case's header, for a test case whose comparison rows are all normal.
func TestRenderSingleFullBodyFailedTest_NamesTheTemplateVariablePlaces(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	var sb strings.Builder
	if err := r.renderSingleFullBodyFailedTest(context.Background(), &sb, templateOnlyFailure()); err != nil {
		t.Fatalf("renderSingleFullBodyFailedTest: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"Testrun failed for test-set-0/test-2 [HIGH-RISK] [TEMPLATE_VARIABLE]",
		"=== TEMPLATE VARIABLES THE ANSWER DID NOT SATISFY ===",
		`  body.id: {{string .id}} must equal id's value "usr_LIVE001"; the answer holds "usr_OTHER01"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("keploy report --full does not say %q:\n%s", want, out)
		}
	}
}

// twoTemplateFailures is a test case that failed on two template variable
// places, and a test case refused when its set loaded (its refusal is its
// template failure, and no comparison row was made).
//
// The common shape: the comparison fails too (a header and the body differ,
// with no noise over them), so a report that names template failures only
// where every row is normal would drop them here.
func twoTemplateFailures() models.TestResult {
	t := templateOnlyFailure()
	t.TemplateFailures = append(t.TemplateFailures, models.TemplateFailure{
		Where: "header X-User", Placeholder: "{{string .id}}", Why: `must equal id's value "usr_LIVE001"; the answer holds "usr_OTHER02"`,
	})
	t.Result.HeadersResult = []models.HeaderResult{{Normal: false,
		Expected: models.Header{Key: "X-User", Value: []string{"usr_LIVE001"}}, Actual: models.Header{Key: "X-User", Value: []string{"usr_OTHER02"}}}}
	t.Result.BodyResult = []models.BodyResult{{Normal: false, Type: models.JSON, Expected: `{"id":"usr_LIVE001"}`, Actual: `{"id":"usr_OTHER01"}`}}
	return t
}

func refusedCase() models.TestResult {
	return models.TestResult{
		Kind: models.HTTP, Name: "test-set-0", TestCaseID: "test-1", Status: models.TestStatusFailed,
		Result:      models.Result{StatusCode: models.IntResult{Normal: true}, HeadersResult: []models.HeaderResult{}, BodyResult: []models.BodyResult{}},
		FailureInfo: models.FailureInfo{Risk: models.High, Category: []models.FailureCategory{models.TemplateVariable}},
		TemplateFailures: []models.TemplateFailure{{
			Where: "request body", Placeholder: "{{capture string .id}}", Why: "refused when the test set loaded: a capture says what the app ANSWERS", Refused: true,
		}},
	}
}

// EVERY REPORT FORMAT says every template variable place a test case failed
// on, and for a refused test case its refusal, with nothing it did not
// compare: keploy report, --full, JUnit and --format json.
func TestReports_SayEveryTemplateFailureAndARefusal(t *testing.T) {
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)
	for _, c := range []struct {
		name string
		test models.TestResult
		want []string
	}{
		{"two places beside failing rows", twoTemplateFailures(), []string{"body.id: {{string .id}}", "header X-User: {{string .id}}", "usr_OTHER02"}},
		{"a refusal", refusedCase(), []string{"request body: {{capture string .id}} refused when the test set loaded"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var compact, full strings.Builder
			if err := r.renderSingleFailedTest(context.Background(), &compact, c.test); err != nil {
				t.Fatal(err)
			}
			if err := r.renderSingleFullBodyFailedTest(context.Background(), &full, c.test); err != nil {
				t.Fatal(err)
			}
			junit := buildFailure(c.test).Text
			if c.name == "a refusal" && !strings.Contains(junit, "template variable request body: {{capture string .id}}") {
				t.Errorf("JUnit does not say a refused placeholder is a template variable's: %q", junit)
			}
			data, err := json.Marshal(buildTestVerdict("run", "set", c.test, nil))
			if err != nil {
				t.Fatal(err)
			}
			var verdict struct {
				Categories []string                 `json:"failure_categories"`
				Failures   []models.TemplateFailure `json:"template_failures"`
			}
			if err := json.Unmarshal(data, &verdict); err != nil {
				t.Fatal(err)
			}
			// A refusal carries refused: true, and a failure of a sent
			// test case does not.
			if len(verdict.Failures) != len(c.test.TemplateFailures) {
				t.Fatalf("--format json carries %d template failures; want %d: %s", len(verdict.Failures), len(c.test.TemplateFailures), data)
			}
			for i, f := range verdict.Failures {
				if f.Refused != c.test.TemplateFailures[i].Refused {
					t.Errorf("--format json says refused=%v for %s; want %v: %s", f.Refused, f.Where, c.test.TemplateFailures[i].Refused, data)
				}
			}
			if refused := strings.Contains(string(data), `"refused":true`); refused != (c.name == "a refusal") {
				t.Errorf("--format json's refused flag: %s", data)
			}
			var asJSON strings.Builder
			for _, f := range verdict.Failures {
				asJSON.WriteString(f.Where + ": " + f.Placeholder + " " + f.Why + "\n")
			}
			for format, out := range map[string]string{"keploy report": compact.String(), "--full": full.String(), "junit": junit, "--format json": asJSON.String()} {
				for _, want := range c.want {
					if !strings.Contains(out, want) {
						t.Errorf("%s does not say %q:\n%s", format, want, out)
					}
				}
			}
			if len(verdict.Categories) != 1 || verdict.Categories[0] != string(models.TemplateVariable) {
				t.Errorf("--format json categories %v", verdict.Categories)
			}
			if c.name == "two places beside failing rows" {
				// The failing rows are said too, beside the template lines.
				if !strings.Contains(junit, "body mismatch") || !strings.Contains(junit, "header X-User") {
					t.Errorf("JUnit drops the failing rows:\n%s", junit)
				}
				if !strings.Contains(compact.String(), "=== CHANGES WITHIN THE RESPONSE BODY ===") {
					t.Errorf("keploy report drops the body diff:\n%s", compact.String())
				}
			}
			if c.name == "a refusal" {
				// A refusal is said as one: no answer was compared.
				for format, out := range map[string]string{"keploy report": compact.String(), "--full": full.String()} {
					if !strings.Contains(out, "REFUSED WHEN THE TEST SET LOADED (THE TEST CASE WAS NOT SENT)") || strings.Contains(out, "THE ANSWER DID NOT SATISFY") {
						t.Errorf("%s does not say the placeholder was refused, unsent:\n%s", format, out)
					}
				}
				for format, out := range map[string]string{"keploy report": compact.String(), "--full": full.String(), "junit": junit} {
					for _, never := range []string{"status_code", "status:", "header ", "No differences found", "found no difference", "CHANGES WITHIN THE RESPONSE BODY", "EXPECT", "ACTUAL"} {
						if strings.Contains(out, never) {
							t.Errorf("%s shows %q for a test case never sent:\n%s", format, never, out)
						}
					}
				}
			}
		})
	}
	plain, err := json.Marshal(buildTestVerdict("run", "set", models.TestResult{Kind: models.HTTP, TestCaseID: "t", Status: models.TestStatusPassed}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "template_failures") {
		t.Errorf("a verdict without template failures carries the field: %s", plain)
	}
}

// A TEST SET REFUSED WHOLE (one a killed keploy templatize left half-written)
// gives each of its test cases a refusal that names no placeholder: JUnit
// and keploy report say where and why, with no empty placeholder between
// them, and JUnit says the test case was refused, not sent.
func TestBuildFailure_NamesARefusedSetsReason(t *testing.T) {
	refused := refusedCase()
	refused.FailureInfo.Category = []models.FailureCategory{models.TestSetRefused}
	refused.TemplateFailures = []models.TemplateFailure{{Where: "test set test-set-0", Why: "refused when the test set loaded: an interrupted write left its files behind", Refused: true}}
	f := buildFailure(refused)
	if want := "test set test-set-0: refused when the test set loaded: an interrupted write left its files behind"; !strings.Contains(f.Text, want) {
		t.Errorf("the JUnit failure does not give the set's reason as %q: %q", want, f.Text)
	}
	// It is no template variable's failure (TEST_SET_REFUSED).
	if strings.Contains(f.Text, "template variable") {
		t.Errorf("the JUnit failure files a refused test set as a template variable: %q", f.Text)
	}
	if f.Message != "test case refused when its test set loaded, not sent [HIGH-RISK]" {
		t.Errorf("message %q", f.Message)
	}
	var sb strings.Builder
	renderTemplateFailures(&sb, refused.TemplateFailures)
	if want := "  test set test-set-0: refused when the test set loaded: an interrupted write left its files behind\n"; !strings.Contains(sb.String(), want) {
		t.Errorf("keploy report says %q, want %q", sb.String(), want)
	}
	if !strings.Contains(sb.String(), "=== TEST SET REFUSED WHEN IT LOADED") || strings.Contains(sb.String(), "TEMPLATE PLACEHOLDERS") {
		t.Errorf("keploy report files a refused test set under template placeholders: %q", sb.String())
	}
}
