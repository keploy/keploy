package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"gopkg.in/yaml.v3"
)

// declaredHoldsStep is holdsStep as variables.go declares it: package
// variables are all set before any init runs, so it holds the default even
// where another file's init registers a reading of a Step.
var declaredHoldsStep = holdsStep

// The ClockOffset and ClockWord of a keploy before Why, Step and Form, field
// for field and tag for tag: what every mock file holds today.
type clockWordBefore struct {
	Start        int    `json:"start" yaml:"start" bson:"start"`
	End          int    `json:"end" yaml:"end" bson:"end"`
	After        int    `json:"after" yaml:"after" bson:"after"`
	BeforeSHA256 string `json:"before_sha256" yaml:"before_sha256" bson:"before_sha256"`
	AfterSHA256  string `json:"after_sha256" yaml:"after_sha256" bson:"after_sha256"`
}

func (w clockWordBefore) IsZero() bool { return w == clockWordBefore{} }

type clockOffsetBefore struct {
	Min     time.Duration   `json:"min" yaml:"min" bson:"min"`
	Max     time.Duration   `json:"max" yaml:"max" bson:"max"`
	Res     time.Duration   `json:"res" yaml:"res" bson:"res"`
	Zone    string          `json:"zone,omitempty" yaml:"zone,omitempty" bson:"zone,omitempty"`
	Layout  string          `json:"layout,omitempty" yaml:"layout,omitempty" bson:"layout,omitempty"`
	AppZone string          `json:"app_zone,omitempty" yaml:"app_zone,omitempty" bson:"app_zone,omitempty"`
	Since   string          `json:"since,omitempty" yaml:"since,omitempty" bson:"since,omitempty"`
	Word    clockWordBefore `json:"word,omitzero" yaml:"word,omitempty" bson:"word,omitempty"`
}

type codec struct {
	name      string
	marshal   func(interface{}) ([]byte, error)
	unmarshal func([]byte, interface{}) error
}

var clockCodecs = []codec{
	{"yaml", yaml.Marshal, yaml.Unmarshal},
	{"json", json.Marshal, json.Unmarshal},
	{"bson", bson.Marshal, bson.Unmarshal},
}

// TestClockFieldsRoundTripAndLeaveOldFilesAlone (TY5): Why, Step and Form
// round-trip through YAML, JSON and BSON, and at their zero values a
// ClockOffset encodes byte for byte as one did before them.
func TestClockFieldsRoundTripAndLeaveOldFilesAlone(t *testing.T) {
	word := ClockWordOf("at=2026-11-01T10:00:00Z;", 3, 23)
	full := ClockOffset{Min: time.Hour, Max: time.Hour + time.Second, Res: time.Second, Zone: ZoneUTC, Since: SinceTestBegin, Word: word, Why: "learned: a read of the clock plus one hour", Step: time.Hour}
	full.Word.Form = ClockWordDecoded
	before := clockOffsetBefore{Min: time.Hour, Max: time.Hour + time.Second, Res: time.Second, Zone: ZoneUTC, Since: SinceTestBegin}
	now := ClockOffset{Min: before.Min, Max: before.Max, Res: before.Res, Zone: before.Zone, Since: before.Since}
	bw := clockWordBefore{Start: word.Start, End: word.End, After: word.After, BeforeSHA256: word.BeforeSHA256, AfterSHA256: word.AfterSHA256}
	for _, c := range clockCodecs {
		for _, v := range []ClockOffset{full, {Step: StepApart, Min: 1}} {
			b, err := c.marshal(v)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			var back ClockOffset
			if err := c.unmarshal(b, &back); err != nil || back != v {
				t.Errorf("%s round trip: %+v, %v; want %+v\n%s", c.name, back, err, v, b)
			}
		}
		for i, pair := range [][2]interface{}{
			{before, now},
			{map[string]clockOffsetBefore{"body.at": before}, map[string]ClockOffset{"body.at": now}},
			{clockOffsetBefore{Res: time.Millisecond, Word: bw}, ClockOffset{Res: time.Millisecond, Word: word}},
		} {
			old, err1 := c.marshal(pair[0])
			cur, err2 := c.marshal(pair[1])
			if err1 != nil || err2 != nil || string(old) != string(cur) {
				t.Errorf("%s #%d: a zero Why, Step and Form encode as\n%s\nnot as before\n%s", c.name, i, cur, old)
			}
		}
	}
	if b, _ := yaml.Marshal(full); !strings.Contains(string(b), "why: ") || !strings.Contains(string(b), "step: ") || !strings.Contains(string(b), "form: decoded") {
		t.Errorf("yaml names the fields otherwise:\n%s", b)
	}
}

// TestClockFieldsMeetAndHold (TY5): Meet keeps the first Why, meets two
// Steps as StepApart (Empty); Holds of a Step above zero is holdsStep's,
// which holds nothing until a keploy reads it; Valid reads a decoded Form.
func TestClockFieldsMeetAndHold(t *testing.T) {
	read := ClockOffset{Res: time.Millisecond}
	for _, c := range []struct {
		name     string
		a, b     ClockOffset
		why      string
		step     time.Duration
		empty    bool
		inString string
	}{
		{"the first why", ClockOffset{Res: 1, Why: "a"}, ClockOffset{Res: 1, Why: "b"}, "a", 0, false, ""},
		{"the why there is", ClockOffset{Res: 1}, ClockOffset{Res: 1, Why: "b"}, "b", 0, false, ""},
		{"one step", ClockOffset{Res: 1, Step: time.Hour}, ClockOffset{Res: 1, Step: time.Hour}, "", time.Hour, false, "cut to 1h0m0s before the offset"},
		{"two steps", ClockOffset{Res: 1, Step: time.Hour}, ClockOffset{Res: 1}, "", StepApart, true, "no one step"},
		{"a step and apart", ClockOffset{Res: 1, Step: StepApart}, ClockOffset{Res: 1, Step: StepApart}, "", StepApart, true, "no one step"},
	} {
		m := c.a.Meet(c.b)
		if m.Why != c.why || m.Step != c.step || m.Empty() != c.empty || !strings.Contains(m.String(), c.inString) {
			t.Errorf("%s: %+v (%s)", c.name, m, m)
		}
		if n := c.b.Meet(c.a); n.Step != m.Step || n.Empty() != m.Empty() {
			t.Errorf("%s: the meet's step depends on its order: %+v and %+v", c.name, m, n)
		}
	}

	from := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	to, at := from.Add(time.Second), from.Add(500*time.Millisecond)
	if !read.Holds(at, time.Millisecond, from, to) {
		t.Fatal("a plain read does not hold in its window")
	}
	stepped := read
	stepped.Step = time.Hour
	defer func(f func(ClockOffset, time.Time, time.Duration, time.Time, time.Time) bool) { holdsStep = f }(holdsStep)
	holdsStep = declaredHoldsStep
	if stepped.Holds(at, time.Millisecond, from, to) {
		t.Error("a read with a Step holds with no keploy to read it")
	}
	apart := read
	apart.Step = StepApart
	var asked ClockOffset
	holdsStep = func(o ClockOffset, _ time.Time, _ time.Duration, _, _ time.Time) bool { asked = o; return true }
	if !stepped.Holds(at, time.Millisecond, from, to) || asked != stepped {
		t.Error("Holds does not ask holdsStep of a read with a Step")
	}
	if apart.Holds(at, time.Millisecond, from, to) {
		t.Error("a StepApart holds")
	}

	w := ClockWordOf("17", 0, 2)
	if w.Valid() {
		t.Error("a word with no text beside it is valid as it stands")
	}
	w.Form = ClockWordDecoded
	if !w.Valid() {
		t.Error("a word of a decoded form is not valid")
	}
	for _, w := range []ClockWord{ClockWordOf("17", 0, 2), ClockWordOf("a=17", 2, 4)} {
		if w.Form = "base64"; w.Valid() || reflect.DeepEqual(w, ClockWord{}) {
			t.Errorf("a form this keploy does not read is valid: %+v", w)
		}
	}
}
