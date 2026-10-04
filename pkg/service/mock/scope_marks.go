package mock

import (
	"errors"
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

func parentOf(windows []models.ScopeWindow, name string) string {
	top := ""
	for _, w := range windows {
		if strings.HasPrefix(name, w.Name+"/") && (top == "" || len(w.Name) < len(top)) {
			top = w.Name
		}
	}
	return top
}

func stepWindows(windows []models.ScopeWindow) []models.ScopeWindow {
	var out []models.ScopeWindow
	for _, w := range windows {
		top := parentOf(windows, w.Name)
		if top == "" {
			continue
		}
		step, _, _ := strings.Cut(w.Name[len(top)+1:], "/")
		out = append(out, models.ScopeWindow{Name: step, Start: w.Start, End: w.End, PID: w.PID})
	}
	return out
}

var ErrRecordRefused = errors.New("recording refused")

type refusal struct{ msg string }

func (r refusal) Error() string        { return r.msg }
func (r refusal) Is(target error) bool { return target == ErrRecordRefused }

func repeatedScope(windows []models.ScopeWindow, existed bool) error {
	runs := map[string]int{}
	var order []string
	for _, w := range windows {
		if parentOf(windows, w.Name) != "" {
			continue
		}
		if runs[w.Name] == 0 {
			order = append(order, w.Name)
		}
		runs[w.Name]++
	}
	for _, name := range order {
		n := runs[name]
		if n < 2 {
			continue
		}
		folder, test := "this run", name
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			folder, test = name[:i], name[i+1:]
		}
		last := "Nothing was changed: your previous recording is kept."
		if !existed {
			last = "Nothing was saved."
		}
		return refusal{fmt.Sprintf("Recording stopped: %s ran %d times in %s.\nEach test in a folder needs its own name. Rename one of them (or drop -count=%d), then run keploy mock record again.\n%s", test, n, folder, n, last)}
	}
	return nil
}

func splitSuites(windows []models.ScopeWindow) ([]models.ScopeWindow, []models.SuiteSpan) {
	var tests []models.ScopeWindow
	var suites []models.SuiteSpan
	for _, w := range windows {
		if w.App {
			continue
		}
		if w.Suite {
			suites = append(suites, models.SuiteSpan{Dir: w.Dir, Start: w.Start, End: w.End})
			continue
		}
		tests = append(tests, w)
	}
	return tests, suites
}

func testWindows(windows []models.ScopeWindow) []models.ScopeWindow {
	tests, _ := splitSuites(windows)
	return tests
}

func appStarts(windows []models.ScopeWindow) []models.ScopeWindow {
	var out []models.ScopeWindow
	for _, w := range windows {
		if w.App {
			out = append(out, w)
		}
	}
	return out
}

func startsByTest(tests, starts []models.ScopeWindow) map[string]int {
	var out map[string]int
	for _, w := range tests {
		if parentOf(tests, w.Name) != "" {
			continue
		}
		for _, s := range starts {
			if !s.Start.Before(w.Start) && !s.Start.After(w.End) {
				if out == nil {
					out = map[string]int{}
				}
				out[w.Name]++
			}
		}
	}
	return out
}
