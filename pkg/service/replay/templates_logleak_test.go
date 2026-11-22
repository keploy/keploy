package replay

import (
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/templates"
)

// The lines a test set's template check logs name each place by its shape and
// say each why and next step without the values they quote: a refusal's why
// can quote a template key's recorded value (a session cookie, a token), an
// uncaptured key's next step writes it out, and a place under an object key
// is named by the key, which can be a value. The test report keeps them
// whole.
func TestTheTemplateCheckLogsNoRecordedValue(t *testing.T) {
	const s = logleak.Canary
	logger, logs := logleak.Observe()
	r := &Replayer{logger: logger, config: &config.Config{}}
	check := templates.CheckResult{
		Refused: map[string][]templates.Refusal{"test-1": {{TestCase: "test-1", Where: "response body.sessions." + s + ".id", Placeholder: "{{capture string .sid}}",
			Why: `a capture outside a JSON string: write "{{capture string .sid}}" (in quotes) where the value is a whole JSON value, or its recorded value "` + s + `" here`}}},
		Uncaptured: []templates.Undefined{{Key: "sid", TestCase: "test-2", Where: "request header Cookie", Placeholder: "{{string .sid}}", Recorded: s}},
	}
	r.logTemplateCheck("set-1", check, nil)
	if logs.FilterMessageSnippet("placeholder is refused").Len() != 1 || logs.FilterMessageSnippet("no earlier test case captures").Len() != 1 {
		t.Fatalf("precondition: the refusal and the uncaptured key were not both logged (%d lines)", logs.Len())
	}
	logleak.Check(t, logs, s, "the template check")
}
