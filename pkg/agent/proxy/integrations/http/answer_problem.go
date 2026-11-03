package http

import (
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// AnswerProblem says why a replay that serves m could not write its
// answer, each of m's variables at its recorded value: the variable, the
// answer place and why; all "" when it can, and when m names no answer
// place (a mock with no variables block is not even loaded). A variables
// block the replay cannot read, or an answer kept apart that does not
// load, is a why with no variable. It is the replay's own reading of the block (readDecls) and
// its own writing of the answer (renderAnswers), exported so that an
// authoring tool that changes a recorded answer refuses one a replay of
// it would refuse at every call.
func AnswerProblem(m *models.Mock) (variable, place, why string) {
	if m == nil || len(m.Spec.Variables) == 0 {
		return "", "", ""
	}
	decls, why := readDecls(m)
	if why != "" {
		return "", "", why
	}
	hit := &varHit{mock: m, decls: decls, local: map[string]variables.Binding{}}
	for _, d := range decls {
		hit.local[d.name] = variables.Binding{Name: d.name, Value: d.recorded, Recorded: d.recorded, Mock: m.Name}
	}
	loaded, err := withResponse(m)
	if err != nil {
		return "", "", err.Error()
	}
	h := &HTTP{Logger: zap.NewNop()}
	if _, p := h.renderAnswers(loaded, hit, &variables.Table{}); p != nil {
		return p.name, p.place, p.why
	}
	return "", "", ""
}
