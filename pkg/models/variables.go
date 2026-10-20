package models

// MockVariables names, by variable, the places of a recorded dependency call
// that hold the value of a test variable: a value the application makes anew
// on every run and carries from where it first appears into later calls (an id
// it mints, a token it signs). The learn step writes it; the recording keeps
// the bytes it was recorded with. Replay reads it only under the proven switch
// (package variables says how a replay follows it).
//
// On disk, a top-level block of the mock document:
//
//	variables:
//	    orderId:
//	        recorded: o3fa9c21b7d4e
//	        bind: body.id
//	        answer: [body.id]
//
// Names are per test (a test case in keploy test, a test in keploy mock
// replay) and may repeat across tests. One namespace holds a test's mock
// variables and its test case's template keys (config.yaml `template:`), so:
//
//   - A variable that carries a template key's value is named after that key:
//     the same value when recorded and in each run the learn step watched,
//     not merely an equal recorded value (two counters both at 1 when
//     recorded are two values). A template key's name is reserved in every
//     test of the set: no other variable takes it. Its recorded value is the
//     key's config.yaml value; a dependency call that binds it is checked
//     against what the test case captures under the key, and a call that
//     names it with another recorded value is refused. Replay cannot tell,
//     from values, a misnamed variable from another value recorded equal: a
//     capture is checked only against a variable of its key's name.
//   - A value that first appears in the test case's own request (a template
//     key the request sends) is bound by the test case: the mocks name it with
//     use places only, no bind. keploy test sends those values for HTTP test
//     cases; a gRPC test case's request binds none.
type MockVariables map[string]MockVariable

// MockVariable is where one variable is in one call. A place is written in
// the grammar of variables.Parse: body.<json path>, url.path[<n>],
// url.query.<name>, header.<name>.
type MockVariable struct {
	// Recorded is the variable's value in the recording: every place below
	// held it when the call was recorded.
	Recorded string `json:"recorded" yaml:"recorded" bson:"recorded"`
	// Bind is the request place where the variable first appears in its test:
	// the live value there is the variable's value for the rest of the test.
	Bind string `json:"bind,omitempty" yaml:"bind,omitempty" bson:"bind,omitempty"`
	// Use lists request places that must hold the variable's value.
	Use []string `json:"use,omitempty" yaml:"use,omitempty" bson:"use,omitempty"`
	// Answer lists response places that hold the variable's value: they are
	// served with the value the variable has in the replay.
	Answer []string `json:"answer,omitempty" yaml:"answer,omitempty" bson:"answer,omitempty"`
	// Echo marks a value that belongs to this call alone (a request id the
	// dependency echoes): bound at Bind for this exchange, answered, and
	// never asked of another call.
	Echo bool `json:"echo,omitempty" yaml:"echo,omitempty" bson:"echo,omitempty"`
	// Outside marks a variable of the claim key, not of one test: a value a
	// call made outside every test sent first (a Playwright beforeAll that
	// seeds a name), bound there and holding for every test of the key that
	// follows, until a new claim begins (a test process run again, which
	// runs its beforeAll again). Every call that declares it says so.
	Outside bool `json:"outside,omitempty" yaml:"outside,omitempty" bson:"outside,omitempty"`
}

// Clone returns a copy of v that shares no map or slice with it; nil for an
// empty v.
func (v MockVariables) Clone() MockVariables {
	if len(v) == 0 {
		return nil
	}
	out := make(MockVariables, len(v))
	for name, mv := range v {
		mv.Use = append([]string(nil), mv.Use...)
		mv.Answer = append([]string(nil), mv.Answer...)
		out[name] = mv
	}
	return out
}

// MergeVariables is a and b as one block (neither is changed): a variable in
// both keeps a's bind and both's uses and answers, is an echo only when both
// say so, and is the claim key's (Outside) when either says so.
func MergeVariables(a, b MockVariables) MockVariables {
	out := a.Clone()
	for name, v := range b {
		if out == nil {
			out = MockVariables{}
		}
		cur, ok := out[name]
		if !ok {
			out[name] = MockVariables{name: v}.Clone()[name]
			continue
		}
		if cur.Bind == "" {
			cur.Bind = v.Bind
		}
		cur.Use = appendNewPlaces(cur.Use, v.Use...)
		cur.Answer = appendNewPlaces(cur.Answer, v.Answer...)
		cur.Echo = cur.Echo && v.Echo
		cur.Outside = cur.Outside || v.Outside
		out[name] = cur
	}
	return out
}

// MergeBinds is a and b, variables bound at other mocks by mock, as one
// (neither is changed).
func MergeBinds(a, b map[string]MockVariables) map[string]MockVariables {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]MockVariables, len(a)+len(b))
	for m, v := range a {
		out[m] = v.Clone()
	}
	for m, v := range b {
		out[m] = MergeVariables(out[m], v)
	}
	return out
}

// appendNewPlaces appends to list each of vs it does not hold yet.
func appendNewPlaces(list []string, vs ...string) []string {
	for _, v := range vs {
		dup := false
		for _, have := range list {
			if have == v {
				dup = true
				break
			}
		}
		if !dup {
			list = append(list, v)
		}
	}
	return list
}

// TestVariablesResp is the answer of GET /agent/variables.
type TestVariablesResp struct {
	Variables []VariableBinding `json:"variables"`
}

// VariableBinding is the value a test variable has in a test, and where it
// was taken: a dependency call the test made (Mock is the mock's name), or
// the test case itself (Mock is "test case <name>"). keploy test sends the
// agent the values its test case's request carries, and reads back what the
// test bound, for its report.
type VariableBinding struct {
	Name string `json:"name" yaml:"name"`
	// Value is the live value.
	Value string `json:"value" yaml:"value"`
	// Recorded is the value the recording held where it was taken.
	Recorded string `json:"recorded" yaml:"recorded"`
	Mock     string `json:"mock" yaml:"mock"`
	Place    string `json:"place" yaml:"place"`
	// Given marks a value the test case's request gave (its template
	// value), not one the application made: it binds the variable for the
	// calls that use it, and is never another variable's value.
	Given bool `json:"given,omitempty" yaml:"given,omitempty"`
	// Outside marks a variable of the claim key (MockVariable.Outside):
	// bound by a call made outside every test, for every test of the key.
	Outside bool `json:"outside,omitempty" yaml:"outside,omitempty"`
}
