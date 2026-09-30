package mock

import (
	"fmt"
	"path"
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

func repeatedScope(windows []models.ScopeWindow) error {
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
		pkg, test := "", name
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			pkg, test = name[:i], name[i+1:]
		}
		p := path.Base(pkg)
		if pkg == "" {
			pkg, p = "one package", "<p>"
		}
		return fmt.Errorf("%s ran %d times in %s; test names must be unique within a folder (check -count, or package %s and %s_test both defining it)", test, n, pkg, p, p)
	}
	return nil
}
