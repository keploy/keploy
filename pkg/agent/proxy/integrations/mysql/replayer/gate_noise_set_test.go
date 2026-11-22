package replayer

import (
	"maps"
	"reflect"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// bodyNoiseAsBefore is the noise compare built for every candidate before
// the gate kept its noise sets: executeBodyNoise's as it was, then a copy
// with the paths in leftOut ignored too.
func bodyNoiseAsBefore(user map[string][]string, mock *models.Mock, live *mysql.PacketBundle, leftOut []string) map[string][]string {
	noise := user
	if mock != nil && live != nil && len(mock.Spec.MySQLRequests) == 1 {
		act, _ := live.Message.(*mysql.StmtExecutePacket)
		exp, _ := mock.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket)
		if act != nil && exp != nil && !typesSentByBoth(exp, act) {
			n := max(len(exp.Parameters), len(act.Parameters))
			out := make(map[string][]string, len(noise)+3*n)
			maps.Copy(out, noise)
			for i := 0; i < n; i++ {
				for _, field := range []string{"type", "unsigned", "name"} {
					out["parameters."+strconv.Itoa(i)+"."+field] = []string{}
				}
			}
			noise = out
		}
	}
	if len(leftOut) > 0 {
		out := make(map[string][]string, len(noise)+len(leftOut))
		maps.Copy(out, noise)
		for _, p := range leftOut {
			out[p] = []string{}
		}
		noise = out
	}
	return noise
}

// TestAGatesNoiseSetIsMadeOnceForEachParameterCount: the strict gate compares
// each candidate under the noise it built for every candidate before
// (bodyNoiseAsBefore), and executeBodyNoise gives the noise it gave before,
// with and without the literal paths left out, in any
// order the candidates come in; it makes one noise set for each parameter
// count and leftOut it meets, gives the user's own noise to a candidate that
// widens nothing, and never writes to the user's noise.
func TestAGatesNoiseSetIsMadeOnceForEachParameterCount(t *testing.T) {
	type param = mysql.Parameter
	p := func(v string) param { return param{Type: 253, Value: v} }
	mockOf := func(name string, bundles ...mysql.PacketBundle) *models.Mock {
		m := &models.Mock{Name: name, Kind: models.MySQL}
		for _, b := range bundles {
			m.Spec.MySQLRequests = append(m.Spec.MySQLRequests, mysql.Request{PacketBundle: b})
		}
		return m
	}
	query := mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 9}, Type: "COM_QUERY"},
		Message: &mysql.QueryPacket{Command: 0x03, Query: "SELECT 1"},
	}
	one, two, three := executeBundle(0, p("a")), executeBundle(0, p("a"), p("b")), executeBundle(1, p("a"), p("b"), p("c"))
	sentTypes := executeBundle(1, p("a"), p("b"))
	candidates := []*models.Mock{
		mockOf("two", two),
		mockOf("one", one),
		mockOf("query", query),
		mockOf("three", three),
		mockOf("two-again", two),
		mockOf("two-requests", two, one),
		mockOf("none"),
		nil,
		mockOf("one-again", one),
	}
	for _, tc := range []struct {
		name string
		live mysql.PacketBundle
		user map[string][]string
		sets int // noise sets made, each kept for the candidates after it
	}{
		// Counts 2, 1 and 3, each with and without leftOut, and leftOut
		// alone for the candidates that widen nothing: 3*2+1.
		{"execute with no types", executeBundle(0, p("x")), map[string][]string{"parameters.0.value": {}, "Name": {"^a"}}, 7},
		{"execute with no types, no user noise", executeBundle(0, p("x")), nil, 7},
		// three sent its types as the live one did, so it widens nothing;
		// one and two widen the live one's 2: 1*2+1.
		{"execute with types", sentTypes, map[string][]string{"x": {}}, 3},
		{"query", query, map[string][]string{"query": {}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := tc.live
			made := 0
			g := newStrictGate(nil, zap.NewNop(), live.Header.Type, &live, -1, "", tc.user)
			g.noiseSetsMade = &made
			userBefore := maps.Clone(tc.user)
			for round := 0; round < 2; round++ {
				for _, mock := range candidates {
					for _, leftOut := range [][]string{nil, literalsLeftOut} {
						got, want := g.bodyNoise(mock, leftOut), bodyNoiseAsBefore(tc.user, mock, &live, leftOut)
						if !reflect.DeepEqual(got, want) {
							t.Errorf("%s, leftOut %v: noise %v, before %v", mockName(mock), leftOut, got, want)
						}
						if leftOut == nil {
							if got := executeBodyNoise(tc.user, mock, &live); !reflect.DeepEqual(got, want) {
								t.Errorf("%s: executeBodyNoise %v, before %v", mockName(mock), got, want)
							}
						}
						if executeNoiseParams(mock, &live) < 0 && len(leftOut) == 0 && reflect.ValueOf(got).Pointer() != reflect.ValueOf(tc.user).Pointer() {
							t.Errorf("%s widens nothing, yet got a noise set of its own", mockName(mock))
						}
					}
				}
			}
			if made != tc.sets {
				t.Errorf("made %d noise sets, want %d (one for each parameter count and leftOut)", made, tc.sets)
			}
			if !reflect.DeepEqual(tc.user, userBefore) {
				t.Errorf("the gate wrote to the user's noise: %v, was %v", tc.user, userBefore)
			}
		})
	}
}

func mockName(m *models.Mock) string {
	if m == nil {
		return "<nil>"
	}
	return m.Name
}
