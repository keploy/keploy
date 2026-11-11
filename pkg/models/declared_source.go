package models

import (
	"errors"
	"fmt"
	"time"
)

// The kinds of DeclaredSource.Kind this keploy knows.
const (
	// DeclaredSourceClock: the application reads the field's value from
	// its clock for the call, within DeclaredSource.Clock of the call's
	// span (keploy.yml's kind: clock).
	DeclaredSourceClock = "clock"
	// DeclaredSourceMinted: the application makes the field's whole value
	// anew at the call (keploy.yml's kind: minted).
	DeclaredSourceMinted = "minted"
)

// DeclaredSource is the source a rule the user declared in keploy.yml gives
// one field of a recording's request (MockEntry.Sources): keploy mock record
// marks each recording a source rule matched before its full round, and the
// learn step decides the field by it where learning alone could not tell
// which source the value has (learn.Source). Rule is the rule as
// "<id>@<first 8 hex of its digest>", Kind one of DeclaredSourceClock and
// DeclaredSourceMinted, Field the place it names (keploy's place grammar:
// body.<path>, url.path[n], url.query.<name>, header.<name>), and Clock, on
// a clock source only, the range the read lies in around the call's span
// and the cut it is read at.
type DeclaredSource struct {
	Rule  string       `json:"rule" yaml:"rule" bson:"rule"`
	Kind  string       `json:"kind" yaml:"kind" bson:"kind"`
	Field string       `json:"field" yaml:"field" bson:"field"`
	Clock *ClockOffset `json:"clock,omitempty" yaml:"clock,omitempty" bson:"clock,omitempty"`
}

// Check is why s is no source this keploy can decide a field by, nil when
// it is one: a rule and a field named, a kind this keploy knows, a clock
// range with a cut on a clock source, no wider than two cuts and a second
// (keploy.yml's own bound), and none on a minted one. A source a
// newer keploy wrote of a kind this one does not know is refused, never
// read as no source: the recording was judged with it.
func (s DeclaredSource) Check() error {
	switch {
	case s.Rule == "":
		return errors.New("a declared source names no rule")
	case s.Field == "":
		return fmt.Errorf("rule %s's declared source names no field", s.Rule)
	}
	switch s.Kind {
	case DeclaredSourceClock:
		var c ClockOffset // none: no cut
		if s.Clock != nil {
			c = *s.Clock
		}
		if c.Res <= 0 || c.Min > c.Max {
			return fmt.Errorf("rule %s declares %s a read of the clock with no range and cut to read it at", s.Rule, s.Field)
		}
		if c.Max-c.Min > 2*c.Res+time.Second {
			return fmt.Errorf("rule %s declares %s a read of the clock over a range wider than two cuts and a second", s.Rule, s.Field)
		}
	case DeclaredSourceMinted:
		if s.Clock != nil {
			return fmt.Errorf("rule %s declares %s minted with a clock range", s.Rule, s.Field)
		}
	default:
		return fmt.Errorf("rule %s declares %s of a kind this keploy does not know (%q)", s.Rule, s.Field, s.Kind)
	}
	return nil
}

// CheckSources is why sources, one recording's (MockEntry.Sources), are no
// sources this keploy can decide fields by, nil when they are: each one is
// (DeclaredSource.Check), and no two name one field, which would leave the
// field's source to their order.
func CheckSources(sources []DeclaredSource) error {
	fields := make(map[string]string, len(sources))
	for _, s := range sources {
		if err := s.Check(); err != nil {
			return err
		}
		if other, ok := fields[s.Field]; ok {
			return fmt.Errorf("rules %s and %s both declare the source of %s", other, s.Rule, s.Field)
		}
		fields[s.Field] = s.Rule
	}
	return nil
}
