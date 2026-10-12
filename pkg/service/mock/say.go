package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

type Summary struct {
	Tests    int
	Calls    int
	Requests int
	Kept     bool
}

type Summarizer interface {
	Summarize(Summary)
}

type AgentErr struct {
	What string
	Err  error
}

func (e AgentErr) Error() string { return e.What + ": " + e.Err.Error() }
func (e AgentErr) Unwrap() error { return e.Err }

func agentErr(what string, err error) error {
	var a AgentErr
	if errors.As(err, &a) {
		return err
	}
	return AgentErr{What: what, Err: err}
}

type Ended struct{ Err error }

func (e Ended) Error() string { return e.Err.Error() }
func (e Ended) Unwrap() error { return e.Err }

func (m *mockService) Record(ctx context.Context) error { return m.ended(m.record(ctx)) }

func (m *mockService) Replay(ctx context.Context) error { return m.ended(m.replay(ctx)) }

func (m *mockService) ended(err error) error {
	if err != nil && m.ends() {
		return Ended{Err: err}
	}
	return err
}

func (m *mockService) ends() bool {
	_, ok := m.store.(Summarizer)
	return ok
}

func (m *mockService) show() zap.Field {
	if m.ends() {
		return zap.Skip()
	}
	return log.Show
}

func (m *mockService) own() zap.Field {
	if m.ends() {
		return log.Hide
	}
	return zap.Skip()
}

func (m *mockService) summarize(s Summary) {
	if sm, ok := m.store.(Summarizer); ok {
		sm.Summarize(s)
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func Again(extra ...string) string {
	args, rest := os.Args[1:], []string(nil)
	if i := slices.Index(args, "--"); i >= 0 {
		args, rest = args[:i], args[i:]
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
