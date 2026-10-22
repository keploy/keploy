package models

import "time"

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
	// Clock is, for a variable that the learn step saw drift at its Bind
	// only with F's clock, the clock reads its recorded value held there
	// that moved by the full round's lead (learn.ClockReads): F's evidence
	// that it is a read of the app's clock, and when the read was made. Two
	// variables recorded with two values may share one live value in a
	// replay only when both are clock reads made in one second, as their
	// recordings say (ReadInOneSecond): a creation time and an expiry an hour
	// later both move with F's clock, and one written as the other is a
	// regression. Each is still asserted at every one of its places. Only the
	// learn step sets it, with a Bind; none for any other variable.
	Clock []ClockRead `json:"clock,omitempty" yaml:"clock,omitempty" bson:"clock,omitempty"`
	// Words is what the learn step learned of the words of the variable's
	// value at its Bind (WordsEntry): of an echo, from the recording and
	// record's full round; of a variable, from its recorded value and the
	// full round's at the earlier bind. The bind is admitted only through
	// it, and each Use place must hold the bound value. Learned only under
	// the proven switch; nil for none. Read and written back with the
	// variable whatever the switch says, as the variable itself is.
	Words *WordsEntry `json:"words,omitempty" yaml:"words,omitempty" bson:"words,omitempty"`
}

// ClockRead is one clock read a recorded value holds, as the learn step read
// it: the instant it says (At), to the resolution it says it at (Res: the
// read was made in [At, At+Res)), and whether it is a time of day with no
// date (Day), compared modulo a day.
type ClockRead struct {
	At  time.Time     `json:"at" yaml:"at" bson:"at"`
	Res time.Duration `json:"res" yaml:"res" bson:"res"`
	Day bool          `json:"day,omitempty" yaml:"day,omitempty" bson:"day,omitempty"`
}

// ReadInOneSecond reports whether a and b, the clock reads of two variables
// (MockVariable.Clock), were made in one second, as their recordings say:
// each holds reads, as many as the other, and at each place the earlier
// read's resolution ends less than a second before the later one begins.
// Only two such variables may share one live value though recorded with
// two values (variables.Table.BindAll): two reads of one clock in one second.
func ReadInOneSecond(a, b []ClockRead) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].inOneSecond(b[i]) {
			return false
		}
	}
	return true
}

// inOneSecond reports whether reads r and o can have been made less than a
// second apart.
func (r ClockRead) inOneSecond(o ClockRead) bool {
	if r.Day != o.Day {
		return false
	}
	d := o.At.Sub(r.At)
	if r.Day {
		for d > 12*time.Hour {
			d -= 24 * time.Hour
		}
		for d <= -12*time.Hour {
			d += 24 * time.Hour
		}
	}
	first := r.Res
	if d < 0 {
		d, first = -d, o.Res
	}
	return d-first < time.Second
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
		mv.Clock = append([]ClockRead(nil), mv.Clock...)
		if mv.Words != nil {
			w := mv.Words.Clone()
			mv.Words = &w
		}
		out[name] = mv
	}
	return out
}

// MergeVariables is a and b as one block (neither is changed): a variable in
// both keeps a's bind and both's uses and answers, and is an echo only when
// both say so. Clock goes with the bind it was read at: a's bind keeps a's,
// one taken from b keeps b's, and a variable both bind is a clock read only
// when both say so. Words go with the bind too: a's bind keeps a's, one taken
// from b takes b's, and a variable both bind at one place holds both's
// (MergeWordsEntry: one side's when the other has none, as a side without
// words never strips them; void when the two disagree).
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
		switch {
		case cur.Bind == "":
			cur.Bind, cur.Clock = v.Bind, append([]ClockRead(nil), v.Clock...)
			if v.Words != nil {
				cur.Words = mergeVariableWords(nil, v.Words)
			}
		case v.Bind != "":
			if len(v.Clock) == 0 {
				cur.Clock = nil
			}
			if v.Bind == cur.Bind {
				cur.Words = mergeVariableWords(cur.Words, v.Words)
			}
		}
		cur.Use = appendNewPlaces(cur.Use, v.Use...)
		cur.Answer = appendNewPlaces(cur.Answer, v.Answer...)
		cur.Echo = cur.Echo && v.Echo
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
	// Clock is, for a binding of a clock variable, its clock reads
	// (MockVariable.Clock): it may share its live value with another clock
	// variable's binding whose reads were made in the same second
	// (ReadInOneSecond), and with no other.
	Clock []ClockRead `json:"clock,omitempty" yaml:"clock,omitempty"`
}
