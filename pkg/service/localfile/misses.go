package localfile

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	setfile "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// MissesFile names, for the last run of a set, each call no recorded mock
// answered: whole, as the user's own record of the run, beside the path tag
// keploy's log named it by (models.UnmatchedCall.PathTag), with how many
// times the run made it and each next step keploy gave it
// (MissedCall.OtherNextSteps). keploy mock replay writes every call its
// replay missed, and keploy test each call a set's application made outside
// every test (its report names those by their shape, as its log does); the
// file says which wrote it (MissesReport.By). The log points here, by path,
// so the call behind each tag it names is found here. Each run writes it
// anew, over the last one's, and a run that missed no such call, or failed
// before it judged its calls, removes the last one's (it would name an
// earlier run's calls). It is written only where it is git-ignored
// (missesIgnored: keploy/.gitignore's "/*/last-replay-misses.yaml" and the
// temporary file's entry, added before anything is written, and, where git
// can say, git itself ignoring both), as the receipts are: it holds the
// calls whole. For the same reason nothing that sends the keploy folder away
// carries it (HoldsCallsWhole).
const MissesFile = "last-replay-misses.yaml"

// missesTempPrefix begins the name of the temporary file a MissesFile write
// goes through (TempPrefixOf): it holds the calls whole until it is renamed
// into place, and a kill before that leaves it behind.
const missesTempPrefix = TempPrefix + "misses-"

// missesIgnore are the keploy/.gitignore entries MissesFile is written only
// under: its own, and its temporary file's (TempPrefix covers it). Set names
// are single path segments, so /*/ reaches all.
var missesIgnore = []string{"/*/" + MissesFile, "/*/" + TempPrefix + "*"}

// HoldsCallsWhole says whether name, a file's base name, is one keploy writes
// the calls a run missed into whole: MissesFile, or the temporary file a
// write of it goes through (TempPrefixOf), which a kill mid-write leaves
// behind. Such a file is the user's own, local and git-ignored, and anything
// that sends the keploy folder away (a bundle, an upload) leaves it out.
func HoldsCallsWhole(name string) bool {
	return name == MissesFile || strings.HasPrefix(name, TempPrefixOf(MissesFile))
}

// Writer is the command that writes a set's MissesFile, as the file names it.
type Writer struct {
	// Command is the command, as the file's header and its by field
	// (MissesReport.By) name it: "keploy test".
	Command string
	// Holds says which calls the file holds, for its header.
	Holds string
	// NextStep is a call's next step where its matcher gave none.
	NextStep string
	// Unwritten is the line that says the file could not be written, and
	// what names the calls then.
	Unwritten string
}

// MissesReport is MissesFile's content.
type MissesReport struct {
	Set string `yaml:"set"`
	// By is the command that wrote it (Writer.Command): keploy mock replay
	// and keploy test write the same file, and a reader of one command's
	// misses reads only that command's.
	By     string       `yaml:"by"`
	At     time.Time    `yaml:"at"`
	Misses []MissedCall `yaml:"misses"`
}

// WriteMisses writes MissesFile for a run of set by w whose calls misses no
// mock answered, in place of the last run's, and returns its path, for the
// log or console to point to; or removes a stale one when the run missed no
// call (it would describe an earlier run), and returns "". "" too when it was
// not written: when it would not be git-ignored (missesIgnored: it holds the
// calls whole, and must never be committed), or the write failed.
func WriteMisses(logger *zap.Logger, keployDir, set string, w Writer, misses []models.UnmatchedCall) string {
	dir := filepath.Join(keployDir, set)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return ""
	}
	calls := MissedCalls(misses, w.NextStep)
	if len(calls) == 0 {
		RemoveMisses(keployDir, set)
		return ""
	}
	next := "check that " + dir + " and keploy/.gitignore are writable by the user running keploy"
	unignored, err := missesIgnored(logger, keployDir, dir)
	if unignored {
		next = "remove the .gitignore line that un-ignores it (a later \"!\" line, such as \"!*.yaml\"), so keploy can keep the calls it names local"
	}
	if err == nil {
		var body []byte
		body, err = yaml.Marshal(MissesReport{Set: set, By: w.Command, At: time.Now().UTC().Truncate(time.Second), Misses: calls})
		if err == nil {
			err = Write(logger, keployDir, dir, MissesFile,
				append([]byte("# Written by `"+w.Command+"`: "+w.Holds+", whole, beside the path tag keploy's log names it by. Local and git-ignored.\n"), body...))
		}
	}
	if err != nil {
		RemoveMisses(keployDir, set)
		logger.Warn(w.Unwritten, zap.String("dir", dir), zap.Error(err),
			zap.String("next_step", next))
		return ""
	}
	return filepath.Join(dir, MissesFile)
}

// RemoveMisses removes set's MissesFile, if there is one: it names the calls
// of a run before the one that is ending, which keploy did not judge, or
// which missed none.
func RemoveMisses(keployDir, set string) {
	dir := filepath.Join(keployDir, set)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return
	}
	_ = setfile.RemoveInRecording(keployDir, filepath.Join(dir, MissesFile))
}

// missesIgnored makes keploy/.gitignore ignore every set's MissesFile and the
// temporary file its write goes through, before anything is written
// (Ignore), and says why either is not ignored, for the file not to be
// written: both entries must be in keploy/.gitignore, and, where git can say
// (utils.GitIgnores: git is installed and dir is in a work tree), git must
// ignore both, so a later line of the user's that un-ignores one (a
// "!*.yaml" after them) keeps the file from being written. unignored: git
// said so, though keploy/.gitignore holds the entries.
func missesIgnored(logger *zap.Logger, keployDir, dir string) (unignored bool, err error) {
	failed := Ignore(logger, keployDir, missesIgnore)
	for _, entry := range missesIgnore {
		if failed[entry] != nil {
			return false, fmt.Errorf("keploy/.gitignore could not be made to ignore %s: %w", entry, failed[entry])
		}
	}
	// A name the temporary file a MissesFile write goes through takes.
	for _, name := range []string{MissesFile, TempPrefixOf(MissesFile) + "0.tmp"} {
		if ignored, known := utils.GitIgnores(dir, name); known && !ignored {
			return true, fmt.Errorf("git does not ignore %s, though keploy/.gitignore lists it: a later .gitignore line un-ignores it", filepath.Join(dir, name))
		}
	}
	return false, nil
}

// MissedCall is a call no mock answered, as a console and MissesFile name
// it: once, however many times the run made it (Count).
type MissedCall struct {
	Protocol    string `yaml:"protocol"`
	Call        string `yaml:"call"`
	PathTag     string `yaml:"path_tag,omitempty"`
	Destination string `yaml:"destination,omitempty"`
	HostTag     string `yaml:"host_tag,omitempty"`
	Count       int    `yaml:"count"`
	NextStep    string `yaml:"next_step"`
	// OtherNextSteps is every other next step keploy gave the call in this
	// run, whole, each once, in the order first given: a call made again
	// can be told another (one that quotes how long after a recording's
	// answer it came, say). MissesFile names each, so every miss's next
	// step is there whole; a console names NextStep alone.
	OtherNextSteps []string `yaml:"other_next_steps,omitempty"`
}

// missedCallKey is what makes two misses one call (MissedCalls).
type missedCallKey struct {
	Protocol, Call, PathTag, Destination, HostTag string
}

// MissedCalls is misses, each call once, in the order the run first made it,
// with how many times it made it: a call of one protocol, summary, path tag
// and destination is one call. A connection that ended with nothing sent
// (models.MatchPhaseNoRequest) made no call, and is left out: its line says
// so. A miss whose matcher gave no next step is told next.
func MissedCalls(misses []models.UnmatchedCall, next string) []MissedCall {
	var out []MissedCall
	at := map[missedCallKey]int{}
	for _, miss := range misses {
		if miss.MatchPhase == models.MatchPhaseNoRequest {
			continue
		}
		call, step := WholeCall(miss, next)
		step = strings.ReplaceAll(step, "\n", " ")
		key := missedCallKey{Protocol: miss.Protocol, Call: call, PathTag: miss.PathTag, Destination: miss.Destination, HostTag: miss.HostTag}
		if i, ok := at[key]; ok {
			c := &out[i]
			c.Count++
			if step != c.NextStep && !slices.Contains(c.OtherNextSteps, step) {
				c.OtherNextSteps = append(c.OtherNextSteps, step)
			}
			continue
		}
		at[key] = len(out)
		out = append(out, MissedCall{Protocol: key.Protocol, Call: key.Call, PathTag: key.PathTag, Destination: key.Destination, HostTag: key.HostTag,
			Count: 1, NextStep: step})
	}
	return out
}

// WholeCall is miss's call and next step as a console and MissesFile name
// them: whole. A miss the agent charged to a test holds them in its Whole
// (models.UnmatchedCall.Charged), its own fields shaped as evidence keeps
// them; every other miss, read from the proxy, holds them in its own. next
// is the next step where the miss's matcher gave none.
func WholeCall(miss models.UnmatchedCall, next string) (call, step string) {
	if w := miss.Whole; w != nil {
		miss.ActualSummary, miss.Operation, miss.NextSteps = w.Call, "", w.NextStep
	}
	if miss.NextSteps != "" {
		next = miss.NextSteps
	}
	return miss.Named(), next
}
