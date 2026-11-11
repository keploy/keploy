package models

import (
	"testing"
	"time"
)

// A declared source is one keploy can decide a field by only with a rule,
// a field and a kind it knows: a clock source with a range no wider than
// two cuts and a second and no minted form, a minted one with no range and
// a form keploy can check a value by. A recording's sources
// also name each field once.
func TestDeclaredSourceCheck(t *testing.T) {
	sec := time.Second
	clock := func(min, max, res time.Duration) DeclaredSource {
		return DeclaredSource{Rule: "at@0a1b2c3d", Kind: DeclaredSourceClock, Field: "body.at", Clock: &ClockOffset{Min: min, Max: max, Res: res}}
	}
	form := &MintedForm{Classes: []string{"-", "digit", "lower"}}
	minted := DeclaredSource{Rule: "id@0a1b2c3d", Kind: DeclaredSourceMinted, Field: "body._id", Minted: form}
	mintedAs := func(f MintedForm) DeclaredSource {
		s := minted
		s.Minted = &f
		return s
	}
	withForm := clock(-sec, 0, sec)
	withForm.Minted = form
	for name, c := range map[string]struct {
		s  DeclaredSource
		ok bool
	}{
		"a clock read":                                  {clock(-sec, 0, sec), true},
		"a clock read of zero width":                    {clock(0, 0, time.Millisecond), true},
		"a range of two cuts and a sec":                 {clock(-2*sec, sec, sec), true},
		"a range a nanosecond wider":                    {clock(-2*sec-1, sec, sec), false},
		"a minted value":                                {minted, true},
		"no rule":                                       {DeclaredSource{Kind: DeclaredSourceMinted, Field: "body._id"}, false},
		"no field":                                      {DeclaredSource{Rule: "id@0a1b2c3d", Kind: DeclaredSourceMinted}, false},
		"a kind keploy does not know":                   {DeclaredSource{Rule: "id@0a1b2c3d", Kind: "answer", Field: "body._id"}, false},
		"no kind":                                       {DeclaredSource{Rule: "id@0a1b2c3d", Field: "body._id"}, false},
		"a clock with no range":                         {DeclaredSource{Rule: "at@0a1b2c3d", Kind: DeclaredSourceClock, Field: "body.at"}, false},
		"a clock with no cut":                           {clock(-sec, 0, 0), false},
		"a clock with a negative cut":                   {clock(-sec, 0, -sec), false},
		"a clock whose max is its min's before":         {clock(0, -time.Millisecond, sec), false},
		"a minted value with a range":                   {DeclaredSource{Rule: "id@0a1b2c3d", Kind: DeclaredSourceMinted, Field: "body._id", Clock: &ClockOffset{Res: sec}, Minted: form}, false},
		"a minted value with no form":                   {DeclaredSource{Rule: "id@0a1b2c3d", Kind: DeclaredSourceMinted, Field: "body._id"}, false},
		"a minted form with no class":                   {mintedAs(MintedForm{}), false},
		"a minted form of a class keploy does not know": {mintedAs(MintedForm{Classes: []string{"hex"}}), false},
		"a minted form of a letter as a class":          {mintedAs(MintedForm{Classes: []string{"a"}}), false},
		"a minted form of a space as a class":           {mintedAs(MintedForm{Classes: []string{" "}}), false},
		"a minted form of no name":                      {mintedAs(MintedForm{Classes: []string{""}}), false},
		"a minted form of a symbol":                     {mintedAs(MintedForm{Classes: []string{"!", "~", "upper", "digit", "lower"}}), true},
		"a minted form of a prefix":                     {mintedAs(MintedForm{Prefix: "kepreq_", Classes: []string{"lower"}}), true},
		"a minted prefix that ends at a letter":         {mintedAs(MintedForm{Prefix: "kepreq", Classes: []string{"lower"}}), false},
		"a minted prefix of a space":                    {mintedAs(MintedForm{Prefix: "a _", Classes: []string{"lower"}}), false},
		"a clock read with a minted form":               {withForm, false},
	} {
		if err := c.s.Check(); (err == nil) != c.ok {
			t.Errorf("%s: Check() = %v, want ok %v", name, err, c.ok)
		}
	}
	if err := CheckSources([]DeclaredSource{clock(-sec, 0, sec), minted}); err != nil {
		t.Errorf("a clock and a minted source on two fields: %v", err)
	}
	if err := CheckSources(nil); err != nil {
		t.Errorf("no sources: %v", err)
	}
	other := minted
	other.Rule = "id2@0a1b2c3d"
	if err := CheckSources([]DeclaredSource{minted, other}); err == nil {
		t.Error("two sources of one field were taken")
	}
	if err := CheckSources([]DeclaredSource{minted, clock(-sec, 0, 0)}); err == nil {
		t.Error("a source keploy cannot decide a field by was taken beside a good one")
	}
}
