package templates

import "testing"

// A placeholder written percent-encoded is found by its offsets in the text
// it is in, so text before it that lowercases to another width (or is not
// UTF-8) must not move them: findEncoded cut the text at the offsets of its
// lowercased copy, which found nothing after İ or the Kelvin sign and
// panicked after Ⱥ or an invalid byte.
func TestFindEncodedPlaceholderAfterTextThatLowercasesToAnotherWidth(t *testing.T) {
	const enc = "%7B%7Bstring%20.id%7D%7D"
	for _, c := range []struct {
		name, before, after string
	}{
		{"ascii", "q=", "&x=1"},
		{"a letter that lowercases to a shorter one (İ)", "q=İ", ""},
		{"the Kelvin sign, shorter", "K=", "&x=1"},
		{"the Angstrom sign, shorter", "q=Å&id=", ""},
		{"a letter that lowercases to a longer one (Ⱥ)", "Ⱥ", ""},
		{"Ⱥ with text after", "q=Ⱥ&id=", "&x=1"},
		{"Ⱦ, longer", "Ⱦ", "&x=1"},
		{"an invalid byte (Latin-1 é)", "name=caf\xe9&id=", ""},
		{"several invalid bytes", "\xff\xfe\xfd", "&x=1"},
		{"a letter after the placeholder too", "İ=", "&n=Ⱥ"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := c.before + enc + c.after
			// A panic is this case's failure, not the whole run's.
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Find(%q) panicked: %v", s, r)
				}
			}()
			got := Find(s)
			if len(got) != 1 {
				t.Fatalf("Find(%q) found %d placeholders, want 1: %+v", s, len(got), got)
			}
			p := got[0]
			if s[p.Start:p.End] != enc {
				t.Errorf("span is %q, want %q", s[p.Start:p.End], enc)
			}
			if p.Key != "id" || p.Type != TypeString || p.Capture {
				t.Errorf("placeholder is %+v, want {{string .id}}", p)
			}
			if !Contains(s) {
				t.Errorf("Contains(%q) = false", s)
			}
			if text, at, ok := FirstHeld(s); !ok || at != p.Start {
				t.Errorf("FirstHeld(%q) = %q, %d, %v; want it at %d", s, text, at, ok, p.Start)
			}
		})
	}
}

// Every placeholder in a text with more than one is found, whatever letter
// case its braces are percent-encoded in.
func TestFindEncodedPlaceholdersInMixedCaseAfterWideRunes(t *testing.T) {
	s := "İa=%7b%7Bint%20.a%7D%7d&Ⱥb=%7B%7bcapture%20string%20.b%7d%7D"
	got := Find(s)
	if len(got) != 2 {
		t.Fatalf("Find found %d placeholders, want 2: %+v", len(got), got)
	}
	if got[0].Key != "a" || got[0].Type != TypeInt || got[0].Capture {
		t.Errorf("first is %+v, want {{int .a}}", got[0])
	}
	if got[1].Key != "b" || got[1].Type != TypeString || !got[1].Capture {
		t.Errorf("second is %+v, want {{capture string .b}}", got[1])
	}
	for _, p := range got {
		if s[p.Start:p.End] == "" || s[p.Start] != '%' || s[p.End-1] != 'D' && s[p.End-1] != 'd' {
			t.Errorf("span %q is not an encoded placeholder", s[p.Start:p.End])
		}
	}
}
