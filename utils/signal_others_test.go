//go:build linux || darwin

package utils

import (
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// keploy says at INFO that it starts the app, in the words CI lanes read as
// that mark ("Starting Application :", under a terminal of its own
// "Starting Application:"), naming the command by its program and how many
// words follow it, never an argument; the command it runs is logged whole,
// its secrets redacted, at DEBUG alone.
func TestStartingApplicationNamesTheProgramAloneAtInfo(t *testing.T) {
	const cmd = "true scott/S3CR3T@db.keploy.test:1521/XE --password S3CR3T"
	check := func(t *testing.T, logs *observer.ObservedLogs, msg string, args []string) {
		t.Helper()
		var info, debug int
		for _, e := range logs.All() {
			switch {
			case e.Level >= zapcore.InfoLevel && strings.HasPrefix(e.Message, "Starting Application"):
				info++
				if e.Message != msg || e.Level != zapcore.InfoLevel {
					t.Errorf("logged %v %q, want INFO %q", e.Level, e.Message, msg)
				}
				if got, want := e.ContextMap(), map[string]any{"program": "true", "args": int64(3)}; !reflect.DeepEqual(got, want) {
					t.Errorf("%q names %v, want %v", e.Message, got, want)
				}
			case e.Message == WholeCommandMessage:
				debug++
				want := map[string]any{"executing_cmd": RedactCmdArgs(args)}
				if e.Level != zapcore.DebugLevel || !reflect.DeepEqual(e.ContextMap(), want) {
					t.Errorf("logged %v %v, want DEBUG %v", e.Level, e.ContextMap(), want)
				}
				if got := want["executing_cmd"].(string); !strings.Contains(got, "scott/<redacted>@db.keploy.test:1521/XE --password <redacted>") {
					t.Errorf("the whole command is %q, want its secrets redacted and the rest kept", got)
				}
			}
			if e.Level >= zapcore.InfoLevel {
				if line := e.Message + " " + fmt.Sprint(e.ContextMap()); strings.Contains(line, "S3CR3T") || strings.Contains(line, "keploy.test") || strings.Contains(line, "scott") {
					t.Errorf("a line at %v names an argument: %s", e.Level, line)
				}
			}
		}
		if info != 1 || debug != 1 {
			t.Errorf("logged %d INFO lines and %d DEBUG lines of the command, want one of each: %v", info, debug, logs.All())
		}
	}

	t.Run("ExecuteCommand", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		if res := ExecuteCommand(context.Background(), zap.New(core), cmd, Native, noCancel, time.Second, nil, nil); res.Err != nil {
			t.Fatalf("ExecuteCommand: %v", res.Err)
		}
		c, err := CommandContext(context.Background(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		check(t, logs, "Starting Application :", c.Args)
	})

	t.Run("under a terminal of its own", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		c := exec.Command("sh", "-c", cmd)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		res := executeWithPTY(context.Background(), zap.New(core), c, NewAppCommand(cmd), nil)
		if res.Type == Init {
			t.Skipf("no terminal to run the command in: %v", res.Err)
		}
		if res.Err != nil {
			t.Fatalf("executeWithPTY: %v", res.Err)
		}
		check(t, logs, "Starting Application:", c.Args)
	})
}
