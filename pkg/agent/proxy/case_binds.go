package proxy

import (
	"fmt"
	"sync"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// A CLIENT TEST'S VARIABLES BOUND AT ITS REQUESTS.
//
// The learn step binds a value a client test sent the application in one
// of its requests, and that a call of the test then carries, at that
// request (models.MappedTestCase.Variables, models.CaseBind): no call binds
// it. So each replay binds it from the request of the test that pairs with
// that recorded one (learn.PairCase: by method and path, then by its words,
// then by order), read at
// the variable's place as the HTTP replay reads a bind place
// (httpint.RequestPlace), before the application is given any byte of the
// request: no call the request leads to is checked without it. A request
// that pairs with none binds nothing; a place the live request holds no
// value at, and one its recording does not hold the variable's recorded
// value at, bind nothing either. Of a request whose body keploy could not
// read, or has not yet (it is still coming), a place in its URL or a header
// binds all the same (atHead), and one in its body binds nothing. A variable
// left unbound so is noted in the test's scope (variables.Table.Note), so
// every call that uses it misses saying why, never naming a call that binds
// it.

// caseBinds pairs the running client test's requests, as they reach the
// application, with its recorded ones, for the variables bound at them.
type caseBinds struct {
	mu    sync.Mutex
	test  string
	cases []learn.ClientCase
	binds [][]models.ClientCaseBind
	used  []bool
}

// set makes cases, test's recorded requests, the ones the requests that
// reach the application from now pair with; nil pairs none.
func (c *caseBinds) set(test string, cases []models.ClientCase) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.test, c.cases, c.binds, c.used = test, nil, nil, nil
	for i := range cases {
		req := cases[i].Req
		c.cases = append(c.cases, learn.ClientCase{Name: cases[i].Name, Req: &req})
		c.binds = append(c.binds, append([]models.ClientCaseBind(nil), cases[i].Binds...))
	}
	c.used = make([]bool, len(c.cases))
}

// wants reports whether a recorded request a variable is bound at is not
// paired yet.
func (c *caseBinds) wants() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, u := range c.used {
		if !u && len(c.binds[i]) > 0 {
			return true
		}
	}
	return false
}

// pair pairs live with the recorded request not yet paired it pairs with
// (learn.PairCase, as the learn step paired it: by its body's words too
// where body says keploy read it, each read as its recording held it where
// it holds a value drifts(test), the values the test's earlier exchanges
// drifted, takes the place of), and returns the test, that request and the
// variables bound at it; ok is false where none is left.
func (c *caseBinds) pair(live *models.HTTPReq, body bool, drifts func(test string) learn.Drifts) (test string, rec learn.ClientCase, binds []models.ClientCaseBind, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i, ok := learn.PairCase(c.cases, c.used, live, body, func() learn.Drifts { return drifts(c.test) })
	if !ok {
		return "", learn.ClientCase{}, nil, false
	}
	c.used[i] = true
	return c.test, c.cases[i], c.binds[i], true
}

// setCaseBinds gives the requests that reach the application from now
// test's recorded requests to pair with (caseBinds), and notes each
// variable bound at one as not bound until its request pairs, in test's
// scope as it begins (MockManager.NoteExactTestAtOpen: the agent gives a
// test's cases before it opens the test's view): one whose request never
// comes misses saying so.
func (p *Proxy) setCaseBinds(test string, cases []models.ClientCase) {
	p.caseBinds.set(test, cases)
	m := p.getMockManager()
	if m == nil {
		return
	}
	notes := map[string]string{}
	for _, c := range cases {
		for _, b := range c.Binds {
			notes[b.Name] = fmt.Sprintf("the test binds it at test case %s's request, at %s, and no request of the test reached the application in that test case's place before this call", c.Name, b.Place)
		}
	}
	m.NoteExactTestAtOpen(test, notes)
}

// bindCase binds, before live is forwarded, each variable of the running
// client test bound at the recorded request live pairs with (caseBinds),
// from live at its place; unread, where not "", says why keploy could not
// read live's body, which then binds nothing at a place in it. A variable
// it cannot bind is noted with why in the test's scope
// (MockManager.NoteExactTest).
func (p *Proxy) bindCase(live *models.HTTPReq, unread string) {
	if bind := p.pairCase(live, unread); bind != nil {
		bind(live, unread)
	}
}

// atHead reports whether place, a bind place of a variable in the request
// rec, is in the request's URL or one of its headers, which keploy reads as
// the request arrives, whatever becomes of its body. One that does not read
// is not.
func atHead(rec *models.HTTPReq, place string) bool {
	a, err := variables.ReadPlace(place, variables.InRequest(rec))
	return err == nil && a.Place.Part != variables.PartBody
}

// pairCase pairs live, a request of the running client test, with the
// recorded request it pairs with (caseBinds), and is what binds that
// request's variables from live as bindCase does: nil where it pairs with
// none. unread, where not "", says why keploy has not read live's body as
// it pairs, which then pairs by its URL alone, as the learn step paired it
// (learn.Tracker.Incoming). Called as the request arrives with its body
// still coming, it binds the variables bound in its URL and headers
// (atHead) and notes the others; called again once keploy has the body, it
// binds those, and leaves the ones it bound.
func (p *Proxy) pairCase(live *models.HTTPReq, unread string) func(*models.HTTPReq, string) {
	test, rec, binds, ok := p.caseBinds.pair(live, unread == "", p.boundDrifts)
	if !ok {
		return nil
	}
	bound := make([]bool, len(binds))
	return func(live *models.HTTPReq, unread string) {
		m := p.getMockManager()
		if m == nil {
			return
		}
		for i, b := range binds {
			if bound[i] {
				continue
			}
			value, why := "", "keploy could not read it: "+unread
			if unread == "" || atHead(rec.Req, b.Place) {
				value, why = httpint.RequestPlace(rec.Req, live, b.Place, b.Recorded)
			}
			if why == "" {
				bound[i] = true
				binding := models.VariableBinding{Name: b.Name, Value: value, Recorded: b.Recorded, Mock: "test case " + rec.Name, Place: "request " + b.Place}
				if err := m.GiveExactTest(test, binding); err != nil {
					p.warnCaseBind(test, rec.Name, b, err.Error())
				}
				continue
			}
			note := fmt.Sprintf("the test binds it at test case %s's request, at %s, which bound nothing this run: %s", rec.Name, b.Place, why)
			if err := m.NoteExactTest(test, b.Name, note); err != nil {
				p.warnCaseBind(test, rec.Name, b, err.Error())
			}
		}
	}
}

// boundDrifts are the values test's variables bound so far this run in
// another value than their recording's (learn.Drifts: each binding's live
// value, standing for its recorded one), the run's among them: what a value
// the application or the test made earlier in the test, and the test sends
// again, held in its recording. None where keploy does not serve test now.
// It is what the learn step read as the test's earlier exchanges' drifted
// values (learn.Tracker's pairing), at the places it bound them.
func (p *Proxy) boundDrifts(test string) learn.Drifts {
	d := learn.Drifts{}
	m := p.getMockManager()
	if m == nil {
		return d
	}
	if v := m.exactView(); v == nil || v.test != test {
		return d
	}
	for _, b := range m.BoundVariables() {
		d.Add(b.Value, b.Recorded)
	}
	return d
}

// warnCaseBind says that keploy could not bind, or note, variable b of
// test at its test case's request.
func (p *Proxy) warnCaseBind(test, testCase string, b models.ClientCaseBind, why string) {
	if p.logger == nil {
		return
	}
	p.logger.Warn("keploy could not bind a variable of the test at its test case's request, so each call that uses it misses",
		zap.String("test", test), zap.String("testCase", testCase), zap.String("variable", b.Name), zap.String("place", b.Place), zap.String("why", why))
}
