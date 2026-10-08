package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// In a templated test set, an expected field whose value a template holds is
// learned from the answer and never a difference. A value the run has settled
// is not learned again: the app answering with another in its place fails.
func TestMatch_ASettledValueIsNotRelearnedFromTheAnswer(t *testing.T) {
	const live, other = "7c9e6679-7425-40de-944b-e07fc1f90ae7", "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
	saved := utils.TemplatizedValues
	t.Cleanup(func() { utils.TemplatizedValues = saved })
	match := func(answered string, opts ...MatchOption) (bool, interface{}) {
		utils.TemplatizedValues = map[string]interface{}{"id": live}
		tc := &models.TestCase{Name: "read", HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"asked":"` + live + `","n":1}`}}
		pass, _ := Match(tc, &models.HTTPResp{StatusCode: 200, Body: `{"asked":"` + answered + `","n":1}`}, nil, false, false, zap.NewNop(), false, opts...)
		return pass, utils.TemplatizedValues["id"]
	}
	settled := WithSettledValues(map[string]bool{live: true})

	if pass, tmpl := match(other); !pass || tmpl != other {
		t.Fatalf("a template's value is learned from the answer: pass=%v template=%v", pass, tmpl)
	}
	if pass, tmpl := match(other, settled); pass || tmpl != live {
		t.Fatalf("a settled value answered as another must fail and leave the template alone: pass=%v template=%v", pass, tmpl)
	}
	if pass, _ := match(live, settled); !pass {
		t.Fatal("a settled value answered as itself passes")
	}
	// Settling one value leaves the other templates learning as before.
	if pass, tmpl := match(other, WithSettledValues(map[string]bool{other: true})); !pass || tmpl != other {
		t.Fatalf("only the settled values are kept: pass=%v template=%v", pass, tmpl)
	}
}
