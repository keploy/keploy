package variables

import (
	"fmt"
)

// MintedWhy is why b, the binding a call makes of a minted variable
// (Binding.Minted: an id keploy.yml's rule declares the application makes
// anew at that call), may not be bound: "" where it may, or where b is no
// minted variable's. The value must not be its recorded one (an id the
// application did not make anew), must keep its rule's form there
// (models.MintedForm.Keeps: the recorded value's length, the prefix, and
// after it only bytes of the form's classes; a form keploy cannot check
// fails closed), and must not be the value of any of earlier that is a
// minted variable's binding (one the run bound before at a minted place,
// or one the call binds before b): an id made anew is never one the run
// had already. Each text leads with what the replay's miss says: "minted
// value reused" or "minted value of another shape".
func MintedWhy(b Binding, earlier ...Binding) string {
	if b.Minted == nil {
		return ""
	}
	rule := "keploy.yml's rule " + b.Minted.Rule + " declares variable " + b.Name + " made anew at " + b.Place + " of " + b.Mock
	if b.Value == b.Recorded {
		return fmt.Sprintf("minted value reused: %s, and this call sends the value it was recorded with there, %q", rule, b.Value)
	}
	for _, e := range earlier {
		if e.Minted != nil && e.Value == b.Value {
			return fmt.Sprintf("minted value reused: %s, and this call sends %q there, the value variable %s was bound to earlier in this run, at %s of %s", rule, b.Value, e.Name, e.Place, e.Mock)
		}
	}
	f := b.Minted.MintedForm
	if err := f.Check(); err != nil {
		return fmt.Sprintf("minted value of another shape: %s, in %v, so keploy cannot check the value this call sends there", rule, err)
	}
	if !f.Keeps(b.Recorded, b.Value) {
		return fmt.Sprintf("minted value of another shape: %s, and this call sends %q there, which does not keep the rule's form: the recorded value's length (%d), then %s", rule, b.Value, len(b.Recorded), f.Where())
	}
	return ""
}
