package replay

import (
	"encoding/json"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestRedactMockStatesForDebug(t *testing.T) {
	const rawJWT = "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1MSJ9.c2lnbmF0dXJl"
	const rawAPIKey = "sk_live_secret_token_999"
	const rawBasic = "Basic dXNlcjpwYXNz"
	const rawEnc = "ENC:string:k1:YWJjZGVm"

	orig := []models.MockState{
		{
			Name: "mock-1",
			Variables: models.MockVariables{
				"declared:fresh-jwt": {
					Recorded: rawJWT,
					Source:   models.VariableSourceDeclared,
					Use:      []string{"header.Authorization"},
				},
				"given:auth": {
					Recorded: rawAPIKey,
					Why:      "given at header.Authorization: recorded " + rawAPIKey,
					Use:      []string{"header.Authorization|bearer"},
				},
				"answer_cookie": {
					Recorded: "session=secret-cookie-val",
					Answer:   []string{"header.Set-Cookie#1"},
				},
				"basic_val": {
					Recorded: rawBasic,
					Use:      []string{"body.creds"},
				},
				"enc_val": {
					Recorded: rawEnc,
					Use:      []string{"body.secret"},
				},
				"learned_jwt_in_body": {
					Recorded: rawJWT,
					Use:      []string{"body.token"},
				},
				"order_id": {
					Recorded: "ord-42",
					Why:      "matched at body.order_id",
					Use:      []string{"body.order_id"},
				},
			},
			Binds: map[string]models.MockVariables{
				"mock-0": {
					"declared:fresh-jwt": {
						Recorded: rawJWT,
						Why:      "bound " + rawJWT,
						Source:   models.VariableSourceDeclared,
						Bind:     "header.Authorization",
					},
				},
			},
		},
	}

	got := redactMockStatesForDebug(orig)
	if len(got) != 1 {
		t.Fatalf("expected 1 mock state, got %d", len(got))
	}
	for _, key := range []string{"declared:fresh-jwt", "given:auth", "answer_cookie", "basic_val", "enc_val", "learned_jwt_in_body"} {
		if v := got[0].Variables[key].Recorded; v != debugRedactedPlaceholder {
			t.Errorf("Variables[%q].Recorded = %q, want %q", key, v, debugRedactedPlaceholder)
		}
	}
	if got[0].Variables["given:auth"].Why != debugRedactedPlaceholder {
		t.Errorf("Variables[given:auth].Why = %q, want %q", got[0].Variables["given:auth"].Why, debugRedactedPlaceholder)
	}
	if v := got[0].Variables["order_id"].Recorded; v != "ord-42" {
		t.Errorf("non-sensitive Variables[order_id].Recorded = %q, want %q", v, "ord-42")
	}
	if v := got[0].Variables["order_id"].Why; v != "matched at body.order_id" {
		t.Errorf("non-sensitive Variables[order_id].Why = %q, want %q", v, "matched at body.order_id")
	}
	if v := got[0].Binds["mock-0"]["declared:fresh-jwt"].Recorded; v != debugRedactedPlaceholder {
		t.Errorf("Binds[mock-0][declared:fresh-jwt].Recorded = %q, want %q", v, debugRedactedPlaceholder)
	}
	if v := got[0].Binds["mock-0"]["declared:fresh-jwt"].Why; v != debugRedactedPlaceholder {
		t.Errorf("Binds[mock-0][declared:fresh-jwt].Why = %q, want %q", v, debugRedactedPlaceholder)
	}
	// Original slice and maps must remain unmutated.
	if orig[0].Variables["declared:fresh-jwt"].Recorded != rawJWT {
		t.Errorf("original Variables map was mutated")
	}
	if orig[0].Variables["given:auth"].Why != "given at header.Authorization: recorded "+rawAPIKey {
		t.Errorf("original Variables Why was mutated")
	}
	if orig[0].Binds["mock-0"]["declared:fresh-jwt"].Recorded != rawJWT {
		t.Errorf("original Binds map was mutated")
	}

	// No-copy fast path when no Variables or Binds exist.
	plain := []models.MockState{{Name: "mock-plain"}}
	if gotPlain := redactMockStatesForDebug(plain); len(gotPlain) != 1 || gotPlain[0].Name != "mock-plain" {
		t.Fatalf("unexpected no-copy result: %+v", gotPlain)
	}

	// logConsumedMocksDebug emits redacted payload at DebugLevel and emits nothing at InfoLevel.
	dbgCore, dbgLogs := observer.New(zapcore.DebugLevel)
	logConsumedMocksDebug(zap.New(dbgCore), "consumed mocks during initial setup", orig, zap.String("testSetID", "manual"))
	entries := dbgLogs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 debug entry, got %d", len(entries))
	}
	blob, err := json.Marshal(entries[0].ContextMap())
	if err != nil {
		t.Fatalf("marshal log entry: %v", err)
	}
	for _, secret := range []string{rawJWT, rawAPIKey, rawBasic, rawEnc, "secret-cookie-val"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("debug log entry leaked %q: %s", secret, string(blob))
		}
	}

	infoCore, infoLogs := observer.New(zapcore.InfoLevel)
	logConsumedMocksDebug(zap.New(infoCore), "consumed mocks during initial setup", orig, zap.String("testSetID", "manual"))
	if infoLogs.Len() != 0 {
		t.Errorf("expected 0 log entries at InfoLevel, got %d", infoLogs.Len())
	}
}
