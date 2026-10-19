package mock

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"go.keploy.io/server/v3/utils"
)

func (m *mockService) note(f func()) {
	m.heldMu.Lock()
	if m.held != nil {
		*m.held = append(*m.held, f)
		m.heldMu.Unlock()
		return
	}
	m.heldMu.Unlock()
	f()
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func without(args []string, flag string, value bool) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == flag:
			if value && i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(args[i], flag+"="):
		default:
			out = append(out, args[i])
		}
	}
	return out
}

func Again(extra ...string) string {
	args, rest := os.Args[1:], []string(nil)
	if i := slices.Index(args, "--"); i >= 0 {
		args, rest = args[:i], args[i:]
	}
	for i := 0; i < len(extra); i++ {
		if strings.HasPrefix(extra[i], "-") {
			args = without(args, extra[i], i+1 < len(extra) && !strings.HasPrefix(extra[i+1], "-"))
		}
	}
	out := append(append([]string{"keploy"}, args...), extra...)
	return utils.QuoteArgs(append(out, rest...))
}

func were(n int, word string) string {
	if n == 1 {
		return "1 " + word + " was"
	}
	return fmt.Sprintf("%d %ss were", n, word)
}
