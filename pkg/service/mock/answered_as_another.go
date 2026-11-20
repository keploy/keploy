package mock

import (
	"fmt"
	"io"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// AnsweredAsAnother is one call of the run that the agent answered with the
// recording of another call (models.AnsweredAsAnother), with the proven
// switch off (Receipt.AnsweredAsAnother): Call and Answered say it whole, as
// the receipt holds the run's calls; Mock is the recording served, Answer
// how it was chosen (models.AnswerOtherID: its path differs at an id
// segment; models.AnswerClosestBody: its body was the closest of several,
// or the one left, which holds a field the call's does not or lacks one),
// Differs where the call differs from it, without values, and NextStep how
// to give the call its own recording.
type AnsweredAsAnother struct {
	Call     string   `yaml:"call" json:"call"`
	Mock     string   `yaml:"mock" json:"mock"`
	Answer   string   `yaml:"answer" json:"answer"`
	Differs  []string `yaml:"differs,omitempty" json:"differs,omitempty"`
	Answered string   `yaml:"answered,omitempty" json:"answered,omitempty"`
	NextStep string   `yaml:"nextStep,omitempty" json:"nextStep,omitempty"`
}

// splitAnsweredAsAnother takes, out of a read of the calls no mock answered,
// the calls the agent answered with another call's recording: they were
// served, so they are no miss, and the replay counts them apart
// (replayCounts.asAnother). Every other entry stays as it came, in order.
func splitAnsweredAsAnother(read []models.UnmatchedCall) (misses, asAnother []models.UnmatchedCall) {
	for _, c := range read {
		if models.AnsweredAsAnother(c.Answer) {
			asAnother = append(asAnother, c)
			continue
		}
		misses = append(misses, c)
	}
	return misses, asAnother
}

// answeredAsAnotherReceipt is the receipt's account of calls
// (Receipt.AnsweredAsAnother); nil for none.
func answeredAsAnotherReceipt(calls []models.UnmatchedCall) []AnsweredAsAnother {
	var out []AnsweredAsAnother
	for _, c := range calls {
		out = append(out, AnsweredAsAnother{
			Call:     c.ActualSummary,
			Mock:     c.ClosestMock,
			Answer:   c.Answer,
			Differs:  c.Differs,
			Answered: c.Diff,
			NextStep: c.NextSteps,
		})
	}
	return out
}

// answeredAsAnotherMax is how many such calls the isolation note names
// whole; the receipt's AnsweredAsAnother holds every one.
const answeredAsAnotherMax = 5

// answeredAsAnotherNote is why a run with calls answered with another
// call's recording is not counted as proven (isolation), naming the first
// answeredAsAnotherMax of them whole.
func answeredAsAnotherNote(calls []models.UnmatchedCall) string {
	named := make([]string, 0, min(len(calls), answeredAsAnotherMax))
	for _, c := range calls[:min(len(calls), answeredAsAnotherMax)] {
		named = append(named, answeredText(c))
	}
	more := ""
	if n := len(calls) - len(named); n > 0 {
		more = fmt.Sprintf("; and %d more (the receipt's answeredAsAnother lists each)", n)
	}
	return fmt.Sprintf("%d call(s) were answered with the recording of another call, not their own: %s%s", len(calls), strings.Join(named, "; "), more)
}

// answeredAsAnotherLogNote is answeredAsAnotherNote as a log line carries
// it: each call by its shape and path tag (models.UnmatchedCallLogName), the
// recording served by name and how it was chosen, in keploy's log form
// (models.UnmatchedCallLogText), never a path or body value of either call.
// The receipt and the console keep the note whole.
func answeredAsAnotherLogNote(calls []models.UnmatchedCall) string {
	named := make([]string, 0, min(len(calls), answeredAsAnotherMax))
	for _, c := range calls[:min(len(calls), answeredAsAnotherMax)] {
		named = append(named, models.UnmatchedCallLogText(c, models.UnmatchedCallLogName(c)+" was answered with recording "+c.ClosestMock+" ("+c.Answer+")"))
	}
	more := ""
	if n := len(calls) - len(named); n > 0 {
		more = fmt.Sprintf("; and %d more (the receipt's answeredAsAnother lists each)", n)
	}
	return fmt.Sprintf("%d call(s) were answered with the recording of another call, not their own: %s%s", len(calls), strings.Join(named, "; "), more)
}

// differsLogShape is differs, where a call differs from the recording it
// was answered with, as a log line names them: a path segment by its place
// (url.path.<i>, keploy's own name), a body field in its shape
// (models.FieldShape: a map's key in a path can be a value).
func differsLogShape(differs []string) []string {
	out := make([]string, 0, len(differs))
	for _, d := range differs {
		if strings.HasPrefix(d, "url.path.") {
			out = append(out, d)
			continue
		}
		out = append(out, models.FieldShape(d))
	}
	return out
}

// answeredText is the call and the recording it was answered with, whole:
// the agent's own words (UnmatchedCall.Diff), or, from an agent that gave
// none, the call and the recording's name.
func answeredText(c models.UnmatchedCall) string {
	if c.Diff != "" {
		return c.Diff
	}
	return c.ActualSummary + " was answered with recording " + c.ClosestMock
}

// sayAnsweredAsAnother tells, for each call answered with another call's
// recording, a WARN that names it by its shape and path tag, the recording
// served by name, where the two differ (differsLogShape) and the next step
// in log form, and on the console the call, the recording's call and the
// next step whole.
func sayAnsweredAsAnother(logger *zap.Logger, console io.Writer, calls []models.UnmatchedCall) {
	for _, c := range calls {
		next := c.NextSteps
		if next == "" {
			next = "record this call (keploy mock record) so it is answered with its own recording"
		}
		logger.Warn("an outgoing call was answered with the recording of another call, not its own: this replay is not counted as proven, and --strict fails it",
			zap.String("protocol", c.Protocol),
			zap.String("call", models.UnmatchedCallLogShape(c)),
			utils.PathTagField(c.PathTag),
			zap.String("answer", c.Answer),
			zap.String("recording", c.ClosestMock),
			zap.Strings("differs", differsLogShape(c.Differs)),
			zap.String("next_step", models.UnmatchedCallLogText(c, next)))
		if console != nil {
			_, _ = fmt.Fprintf(console, "answered with another call's recording: %s\n  next step: %s\n", answeredText(c), next)
		}
	}
}
