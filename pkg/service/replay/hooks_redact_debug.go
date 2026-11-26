package replay

import (
	"regexp"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const debugRedactedPlaceholder = "[REDACTED]"

var debugJWTRe = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

// logConsumedMocksDebug emits a debug log carrying mocks with sensitive
// variables and reasons redacted, avoiding any slice/map allocation when debug
// logging is disabled.
func logConsumedMocksDebug(logger *zap.Logger, msg string, mocks []models.MockState, fields ...zap.Field) {
	if logger == nil {
		return
	}
	if ce := logger.Check(zapcore.DebugLevel, msg); ce != nil {
		all := make([]zap.Field, 0, len(fields)+1)
		all = append(all, fields...)
		all = append(all, zap.Any("mocks", redactMockStatesForDebug(mocks)))
		ce.Write(all...)
	}
}

// redactMockStatesForDebug returns a shallow copy of mocks whose Variables and
// Binds have sensitive/declared/credential Recorded and Why values replaced
// with "[REDACTED]" so `--debug` logs never leak bearer tokens, JWTs, API keys,
// or declared transform bindings. The caller's slice and underlying maps are
// never mutated.
func redactMockStatesForDebug(mocks []models.MockState) []models.MockState {
	if len(mocks) == 0 {
		return mocks
	}
	needsCopy := false
	for i := range mocks {
		if len(mocks[i].Variables) > 0 || len(mocks[i].Binds) > 0 {
			needsCopy = true
			break
		}
	}
	if !needsCopy {
		return mocks
	}
	out := make([]models.MockState, len(mocks))
	copy(out, mocks)
	for i := range out {
		if len(out[i].Variables) > 0 {
			out[i].Variables = redactMockVariablesForDebug(out[i].Variables)
		}
		if len(out[i].Binds) > 0 {
			redactedBinds := make(map[string]models.MockVariables, len(out[i].Binds))
			for mockName, vars := range out[i].Binds {
				redactedBinds[mockName] = redactMockVariablesForDebug(vars)
			}
			out[i].Binds = redactedBinds
		}
	}
	return out
}

func redactMockVariablesForDebug(vars models.MockVariables) models.MockVariables {
	if len(vars) == 0 {
		return vars
	}
	out := make(models.MockVariables, len(vars))
	for name, mv := range vars {
		if isSensitiveMockVarForDebug(name, mv) {
			if mv.Recorded != "" {
				mv.Recorded = debugRedactedPlaceholder
			}
			if mv.Why != "" {
				mv.Why = debugRedactedPlaceholder
			}
		}
		out[name] = mv
	}
	return out
}

func isSensitiveMockVarForDebug(name string, mv models.MockVariable) bool {
	if mv.Source == models.VariableSourceDeclared || strings.HasPrefix(name, "declared:") {
		return true
	}
	if isCredentialPlaceForDebug(mv.Bind) {
		return true
	}
	for _, p := range mv.Use {
		if isCredentialPlaceForDebug(p) {
			return true
		}
	}
	for _, p := range mv.Answer {
		if isCredentialPlaceForDebug(p) {
			return true
		}
	}
	for _, val := range []string{mv.Recorded, mv.Why} {
		rec := strings.TrimSpace(val)
		if rec == "" {
			continue
		}
		low := strings.ToLower(rec)
		if strings.HasPrefix(low, "bearer ") ||
			strings.HasPrefix(low, "basic ") ||
			strings.HasPrefix(rec, "ENC:") ||
			debugJWTRe.MatchString(rec) {
			return true
		}
	}
	return false
}

func isCredentialPlaceForDebug(place string) bool {
	p := strings.ToLower(strings.TrimSpace(place))
	if !strings.HasPrefix(p, "header.") {
		return false
	}
	hdr := strings.TrimPrefix(p, "header.")
	if idx := strings.IndexAny(hdr, "|#"); idx >= 0 {
		hdr = hdr[:idx]
	}
	switch hdr {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "x-auth-token":
		return true
	default:
		return false
	}
}
