package record

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A Kind: Consumer test case carries an entirely empty HTTPReq. Stamping one
// produces the literal string `curl --request  --url `, which then travels
// through telemetry.ExtractDomainsFromTestCase and the BeforeTestCaseInsert
// hook on the in-memory test case.
func TestShouldStampCurl(t *testing.T) {
	httpTC := func(body string, form []models.FormData) *models.TestCase {
		tc := &models.TestCase{Kind: models.HTTP}
		tc.HTTPReq.Body = body
		tc.HTTPReq.Form = form
		return tc
	}

	tests := []struct {
		name string
		tc   *models.TestCase
		want bool
	}{
		{name: "an ordinary HTTP test case is stamped, exactly as before", tc: httpTC("{}", nil), want: true},
		{name: "gRPC keeps today's behaviour: this guard excludes the NEW kind only", tc: &models.TestCase{Kind: models.GRPC_EXPORT}, want: true},
		{name: "an empty Kind (older recordings) keeps today's behaviour", tc: &models.TestCase{}, want: true},
		{name: "a consumer test case is never stamped", tc: &models.TestCase{Kind: models.CONSUMER}, want: false},
		{name: "a body over the threshold is not stamped", tc: httpTC(strings.Repeat("x", curlBodyThreshold+1), nil), want: false},
		{name: "a body exactly at the threshold still is", tc: httpTC(strings.Repeat("x", curlBodyThreshold), nil), want: true},
		{name: "a form request is not stamped", tc: httpTC("", []models.FormData{{Key: "k"}}), want: false},
		{name: "nil is not stamped", tc: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldStampCurl(tt.tc); got != tt.want {
				t.Fatalf("shouldStampCurl = %v, want %v", got, tt.want)
			}
		})
	}
}

// The predicate is only worth anything if the capture loop asks it. The loop
// itself is a goroutine over a channel inside a ~200-line method and is not
// reachable from a unit test, so the CALL is pinned by reading the source —
// the same technique pkg/service/replay uses for RunTestSet's seams.
func TestTheCaptureLoopAsksTheCurlPredicate(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "record.go", nil, 0)
	if err != nil {
		t.Fatalf("parse record.go: %v", err)
	}

	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		var cond strings.Builder
		if err := printer.Fprint(&cond, fset, ifStmt.Cond); err != nil {
			return true
		}
		if cond.String() != "shouldStampCurl(testCase)" {
			return true
		}
		var body strings.Builder
		if err := printer.Fprint(&body, fset, ifStmt.Body); err != nil {
			return true
		}
		if strings.Contains(body.String(), "MakeCurlCommand") {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("the capture loop no longer gates pkg.MakeCurlCommand on shouldStampCurl(testCase); " +
			"without it every consumer test case is stamped `curl --request  --url `")
	}
}
