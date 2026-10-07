package agent

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestASuiteMarkComesBackAsASuiteWindow(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	ctx := context.Background()
	a.NoteScope("/repo/e2e/orders", 42, "/repo/e2e/orders", true)
	if err := a.BeginScope(ctx, "/repo/e2e/orders", 42); err != nil {
		t.Fatal(err)
	}
	a.NoteScope("orders.TestA", 42, "/repo/e2e/orders", false)
	if err := a.BeginScope(ctx, "orders.TestA", 42); err != nil {
		t.Fatal(err)
	}
	if err := a.EndScope(ctx, "orders.TestA", 42); err != nil {
		t.Fatal(err)
	}
	if err := a.EndScope(ctx, "/repo/e2e/orders", 42); err != nil {
		t.Fatal(err)
	}
	windows, err := a.GetScopeWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 {
		t.Fatalf("windows = %+v", windows)
	}
	for _, w := range windows {
		if w.Dir != "/repo/e2e/orders" || w.Suite != (w.Name == "/repo/e2e/orders") {
			t.Fatalf("window %+v", w)
		}
	}
}

func TestAnEndMarkKeepsTheTestsStatus(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	ctx := context.Background()
	for _, name := range []string{"orders.TestSkip", "orders.TestOld"} {
		if err := a.BeginScope(ctx, name, 42); err != nil {
			t.Fatal(err)
		}
	}
	a.NoteStatus("orders.TestSkip", 42, "skipped")
	for _, name := range []string{"orders.TestSkip", "orders.TestOld"} {
		if err := a.EndScope(ctx, name, 42); err != nil {
			t.Fatal(err)
		}
	}
	windows, _ := a.GetScopeWindows(ctx)
	got := map[string]string{}
	for _, w := range windows {
		got[w.Name] = w.Status
	}
	if got["orders.TestSkip"] != "skipped" || got["orders.TestOld"] != "" {
		t.Fatalf("statuses %+v", got)
	}
}
