package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A minted value is checked new and of its shape where it is bound, which
// keploy does on MongoDB calls alone: an HTTP recording whose variable is
// declared minted is refused, never served with the value unchecked. The
// same recording with no minted mark binds the new value and answers.
func TestVariables_AMintedVariableOfAnHTTPCallIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const rec, live = "0f8a6c2e-6b1d-4c3a-9e5f-2a7b8c9d0e1f", "5c1e7a9b-3d2f-4e8a-b6c4-9f0a1b2c3d4e"
	mock := func(minted *models.MintedShape) *varDb {
		v := bindV(rec, "body.id")
		v.Minted = minted
		return &varDb{perTest: []*models.Mock{at(varMock("mock-1", "POST", "http://store/audits", `{"id":"`+rec+`"}`, 201, `{}`,
			models.MockVariables{"auditId": v}), 1)}}
	}
	ok, _, diag := matchVar(t, newHTTP(), mock(&models.MintedShape{Rule: "audit-id", MintedForm: models.MintedForm{MinLen: 36, MaxLen: 36}}), liveReq("POST", "/audits", `{"id":"`+live+`"}`))
	wantProblem(t, ok, diag, "variable auditId is declared minted by keploy.yml's rule audit-id, and keploy checks a minted value on MongoDB calls only")
	if ok, _, diag := matchVar(t, newHTTP(), mock(nil), liveReq("POST", "/audits", `{"id":"`+live+`"}`)); !ok {
		t.Fatalf("the recording with no minted mark should answer the call, got %+v", diag)
	}
}
