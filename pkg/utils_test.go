package pkg

import (
	// "encoding/json"
	// "fmt"
	"fmt"
	"net/http"
	"testing"

	// "time"
	"github.com/go-test/deep"
	"go.keploy.io/server/pkg/service/run"
	"go.uber.org/zap"
)

func TestCompareHeader(t *testing.T) {
	for _, tt := range []struct {
		exp       http.Header
		actual    http.Header
		hdrResult []run.HeaderResult
		result    bool
	}{
		//keys and values matches
		{
			exp: http.Header{
				"id":  {"1234"},
				"app": {"sports", "study"},
			},
			actual: http.Header{
				"id":  {"1234"},
				"app": {"sports", "study"},
			},
			hdrResult: []run.HeaderResult{
				{
					Normal: true,
					Expected: run.Header{
						Key:   "id",
						Value: []string{"1234"},
					},
					Actual: run.Header{
						Key:   "id",
						Value: []string{"1234"},
					},
				},
				{
					Normal: true,
					Expected: run.Header{
						Key:   "app",
						Value: []string{"sports", "study"},
					},
					Actual: run.Header{
						Key:   "app",
						Value: []string{"sports", "study"},
					},
				},
			},
			result: true,
		},
		//key present in actual but not in exp
		{
			exp: http.Header{
				"Content-Length": {"gg"},
				"id":             {"1234"},
			},
			actual: http.Header{
				"Content-Length": {"sj"},
				"id":             {"1234"},
				"app":            {"sports", "study"},
			},
			hdrResult: []run.HeaderResult{
				{
					Normal: true,
					Expected: run.Header{
						Key:   "id",
						Value: []string{"1234"},
					},
					Actual: run.Header{
						Key:   "id",
						Value: []string{"1234"},
					},
				},
				{
					Normal: false,
					Expected: run.Header{
						Key:   "app",
						Value: nil,
					},
					Actual: run.Header{
						Key:   "app",
						Value: []string{"sports", "study"},
					},
				},
			},
			result: false,
		},
		//key present in exp but not in actual
		{
			exp: http.Header{
				"id":  {"1234"},
				"app": {"sports", "study"},
			},
			actual: http.Header{
				"app": {"sports", "study"},
			},
			hdrResult: []run.HeaderResult{
				{
					Normal: false,
					Expected: run.Header{
						Key:   "id",
						Value: []string{"1234"},
					},
					Actual: run.Header{
						Key:   "id",
						Value: nil,
					},
				},
				{
					Normal: true,
					Expected: run.Header{
						Key:   "app",
						Value: []string{"sports", "study"},
					},
					Actual: run.Header{
						Key:   "app",
						Value: []string{"sports", "study"},
					},
				},
			},
			result: false,
		},
		//key present in both but value array aren't equal
		{
			exp: http.Header{
				"id":  {"1234"},
				"app": {"sports", "study", "code"},
			},
			actual: http.Header{
				"id":  {"1234"},
				"app": {"sports", "eat", "code"},
			},
			hdrResult: []run.HeaderResult{
				{
					Normal: true,
					Expected: run.Header{
						Key:   "id",
						Value: []string{"1234"},
					},
					Actual: run.Header{
						Key:   "id",
						Value: []string{"1234"},
					},
				},
				{
					Normal: false,
					Expected: run.Header{
						Key:   "app",
						Value: []string{"sports", "study", "code"},
					},
					Actual: run.Header{
						Key:   "app",
						Value: []string{"sports", "eat", "code"},
					},
				},
			},
			result: false,
		},
		//key present but length of value array aren't equal
		{
			exp: http.Header{
				"id":  {"1234"},
				"app": {"sports", "code"},
			},
			actual: http.Header{
				"id":  {"1234"},
				"app": {"sports", "eat", "code"},
			},
			hdrResult: []run.HeaderResult{
				{
					Normal: true,
					Expected: run.Header{
						Key:   "id",
						Value: []string{"1234"},
					},
					Actual: run.Header{
						Key:   "id",
						Value: []string{"1234"},
					},
				},
				{
					Normal: false,
					Expected: run.Header{
						Key:   "app",
						Value: []string{"sports", "code"},
					},
					Actual: run.Header{
						Key:   "app",
						Value: []string{"sports", "eat", "code"},
					},
				},
			},
			result: false,
		},
		//key present but length of value array are empty
		{
			exp: http.Header{
				"app": {},
			},
			actual: http.Header{
				"app": {},
			},
			hdrResult: []run.HeaderResult{
				{
					Normal: true,
					Expected: run.Header{
						Key:   "app",
						Value: []string{},
					},
					Actual: run.Header{
						Key:   "app",
						Value: []string{},
					},
				},
			},
			result: true,
		},
		{
			exp:       http.Header{},
			actual:    http.Header{},
			hdrResult: []run.HeaderResult{},
			result:    true,
		},
	} {
		logger, _ := zap.NewProduction()
		defer logger.Sync()
		hdrResult := []run.HeaderResult{}
		res := CompareHeaders(tt.exp, tt.actual, &hdrResult)
		if res != tt.result {
			t.Fatal(tt.exp, tt.actual, "THIS IS EXP", tt.hdrResult, " \n THIS IS ACT", hdrResult)
		}
		diff := isEqual(hdrResult, tt.hdrResult)
		if diff != nil {
			fmt.Printf("This is diff %v\n", diff)
			t.Fatal("THIS IS EXP", tt.hdrResult, " \n THIS IS ACT", hdrResult)
		}
	}
}

func isEqual(x, y []run.HeaderResult) []string {

	expected := make(map[string]run.HeaderResult)
	actual := make(map[string]run.HeaderResult)
	for _, i := range x {
		expected[i.Expected.Key] = i
	}
	for _, i := range y {
		actual[i.Expected.Key] = i
	}

	return deep.Equal(expected, actual)
}
