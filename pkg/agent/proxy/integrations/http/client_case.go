package http

import (
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// RequestPlace is the value live, a client test's request that pairs with
// the request rec its test case recorded, holds at place, the bind place of
// a variable of the test there (models.ClientCaseBind), read as this replay
// reads a variable's bind place in a call's request (readDecls,
// reqView.read): rec decides how the place reads (variables.ReadPlace), and
// must hold the variable's recorded value there. why says, where it gives no
// value, why: the place does not read, rec holds another value there, or
// live holds none (or an empty one, which binds no variable: emptyBind).
func RequestPlace(rec, live *models.HTTPReq, place, recorded string) (value, why string) {
	if rec == nil || live == nil {
		return "", "keploy holds no copy of it"
	}
	a, err := variables.ReadPlace(place, variables.InRequest(rec))
	if err != nil {
		return "", fmt.Sprintf("its place %s does not read: %v", place, err)
	}
	vp := varPlace{raw: place, place: a.Place, at: a}
	r, rok := recordedView(rec)
	l, lok := recordedView(live)
	if !rok || !lok {
		return "", "its URL does not parse"
	}
	rh, ok := r.read(vp)
	if !ok || !variables.SameValue(recorded, rh.value, rh.number()) {
		return "", fmt.Sprintf("its recording does not hold the variable's recorded value %q at %s", recorded, place)
	}
	lh, ok := l.read(vp)
	if !ok || lh.empty() {
		return "", fmt.Sprintf("it holds no value at %s this run", place)
	}
	return lh.value, ""
}
