// Package fixture holds one of each shape the guard in
// tests/logcontent reads: TestScanFindsEachShape holds the scan to the
// "want:" comment on each line (the kinds of the sites on it, in any order),
// and to no site on a line with none.
package fixture

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/stretchr/testify/mock"

	"go.keploy.io/server/v3/tests/logcontent/testdata/fixture/sentinel"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.keploy.io/server/v3/utils/logcontent/logurl"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Kind is an enum: a string type of keploy's with constants of its own.
type Kind string

// KindHTTP is one of Kind's values.
const KindHTTP Kind = "http"

type conn struct{ ConnectionID string }

// errSentinel is a sentinel of a constant text.
var errSentinel = errors.New("a sentinel")

// errQuoted is a sentinel whose text is not a constant.
var errQuoted = errors.New(os.Getenv("X")) // want: errors.New

type payload struct{ body string }

func (p payload) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddInt("len", len(p.body))
	enc.AddString("body", p.body) // want: field
	return nil
}

func parse(s string) error {
	_, err := strconv.Atoi(s)
	return err
}

func opened() error {
	_, err := os.Open("keploy.yml")
	return fmt.Errorf("open the config: %w", err)
}

// pathError is the standard library's error of an op on a path, written
// out: its op, its path and the errno it wraps name no content.
func pathError(p string) error {
	return &fs.PathError{Op: "open", Path: p, Err: os.ErrNotExist}
}

type store interface{ Get() error }

// fileStore is store's one implementation, whose errors are os's.
type fileStore struct{}

func (fileStore) Get() error {
	_, err := os.Open("keploy.yml")
	return err
}

// mockStore is a test's stand-in for store, whose errors are the test's.
type mockStore struct{ mock.Mock }

func (m *mockStore) Get() error { return m.Called().Error(0) }

// say is a log helper: its message is its callers', who are judged by what
// they give it.
func say(logger *zap.Logger, msg string) { logger.Info(msg) }

// sayOn is a log helper too: it passes its message on to say.
func sayOn(logger *zap.Logger, what string) { say(logger, what) }

// fail is a log helper of an error, which it logs whole.
func fail(logger *zap.Logger, err error) { logger.Error("failed", zap.Error(err)) }

// bodyField is a log helper of a field: its value is its callers'.
func bodyField(body string) zap.Field { return zap.String("body", body) }

// pairFields is a log helper of two fields: each call's two values are two
// sites, whatever order the scan reads its parameters in.
func pairFields(host, body string) []zap.Field {
	return []zap.Field{zap.String("host", host), zap.String("body", body)}
}

// Said is exported: a package the scan does not read may call it, so its
// message is a site where it logs it.
func Said(logger *zap.Logger, msg string) {
	logger.Info(msg) // want: message
}

// called is used as a value as well as called, so a call the scan does not
// see may give it anything: its message is a site where it logs it.
func called(logger *zap.Logger, msg string) {
	logger.Info(msg) // want: message
}

var _ = called

// Sites logs s, a value the application sent, every way the guard reads.
func Sites(logger *zap.Logger, body []byte, s string, n int, k Kind, c conn, d time.Duration) error {
	logger.Info("a constant", zap.String("k", "v"))
	logger.Info("a value", zap.String("body", s))         // want: field
	logger.Info("bytes", zap.ByteString("body", body))    // want: field
	logger.Info("any", zap.Any("v", s))                   // want: field
	logger.Info("strings", zap.Strings("v", []string{s})) // want: field
	logger.Info("numbers", zap.Int("n", n), zap.Duration("d", d), zap.Bool("b", n > 0))
	logger.Info("an enum", zap.String("kind", string(k)))
	logger.Info("a connection id", zap.String("conn", c.ConnectionID))
	logger.Info("gated", logcontent.String("body", s), logcontent.Bytes("b", body), logurl.Field("url", s), logcontent.Any("v", s))
	logger.Info("its own marshaler", zap.Object("p", payload{s}))
	logger.Info("a shaped URL", zap.String("url", logurl.URL(s)))
	logger.Info("a message with " + s) // want: message
	logger.Info(fmt.Sprintf("a message with %d", n))
	logger.Info(fmt.Sprintf("a message with %s", s)) // want: message
	logger.Info(fmt.Sprintf("a type %T", s))
	err := parse(s)
	logger.Error("parse", zap.Error(err)) // want: error
	utils.LogError(logger, err, "parse")  // want: error
	utils.LogContentError(logger, err, "parse")
	logger.Error("parse", logcontent.Error(err))
	logger.Error("a shaped error", zap.Error(logurl.Error(err, s)))
	logger.Error("parse: " + err.Error()) // want: error
	logger.Error("open", zap.Error(opened()))
	logger.Error("a sentinel", zap.Error(errSentinel))
	logger.Error("another package's sentinel", zap.Error(sentinel.ErrConstant))
	logger.Error("a path error", zap.Error(pathError(s)))
	logger.Error("a store's error", zap.Error(store(fileStore{}).Get()))
	logger.Error("a quoted sentinel", zap.Error(errQuoted)) // want: error
	if logcontent.On() {
		logger.Debug("whole, with the switch on", zap.String("body", s))
	}
	e1 := fmt.Errorf("bad %s", s) // want: errorf
	e2 := fmt.Errorf("bad %d: %w", n, opened())
	e3 := errors.New(s)                               // want: errors.New
	e4 := fmt.Errorf("bad %v", body)                  // want: errorf
	logger.Sugar().Infof("sugar %s", s)               // want: sugar
	log.Printf("the standard library's logger %s", s) // want: sugar
	log.Printf("the standard library's logger %d", n)
	logger.Info("a field written out", zapcore.Field{Key: "k", Type: zapcore.StringType, String: s}) // want: field
	logger.Info("wrapped", zap.Error(fmt.Errorf("wrapped: %w", err)))                                // want: error
	say(logger, "a constant message")
	say(logger, "a message with "+s) // want: message
	sayOn(logger, s)                 // want: message
	fail(logger, opened())
	fail(logger, err) // want: error
	logger.Info("a helper's field", bodyField("a constant"))
	logger.Info("a helper's field", bodyField(s))                        // want: field
	logger.Info("a helper's two fields", pairFields(s, string(body))...) // want: field, field
	return errors.Join(e1, e2, e3, e4)
}

// lost is a printf wrapper: the error it makes formats its values with %v.
func lost(format string, args ...any) error {
	return fmt.Errorf("lost: %s", fmt.Sprintf(format, args...))
}

// Locals logs what a local string variable holds, which is what is assigned
// to it: a variable of constants is no site, and an error's text put in one
// is the error, at the place it is put.
func Locals(logger *zap.Logger, s string, n int) {
	what := "a constant"
	if n > 0 {
		what = fmt.Sprintf("a number %d", n)
	}
	logger.Info("a local of constants", zap.String("what", what))
	logger.Info(what)
	held := s
	logger.Info("a local of a value", zap.String("held", held)) // want: field
	kept := "a constant"
	p := &kept
	*p = s
	logger.Info("a local written through a pointer", zap.String("kept", kept)) // want: field
	for _, item := range []string{s} {
		logger.Info("a range's value", zap.String("item", item)) // want: field
	}
	err := parse(s)
	why := err.Error() // want: error
	logger.Warn("an error's text in a local", zap.String("why", why))
	msg := fmt.Sprintf("parse failed: %v", err) // want: error
	logger.Warn(msg)
	logger.Warn("two errors' texts", zap.String("both", "parse: "+err.Error()+"; open: "+opened().Error())) // want: error
	stop := "stopped"
	if n > 1 {
		stop = fmt.Sprintf("failed to parse: %v", err)
	}
	stopped := fmt.Errorf("%s", stop)
	logger.Error("stopped", zap.Error(stopped)) // want: error
	safeStop := "stopped"
	if n > 2 {
		safeStop = fmt.Sprintf("open: %v", opened())
	}
	logger.Error("stopped", zap.Error(fmt.Errorf("%s", safeStop)))
	logger.Error("a wrapper's error", zap.Error(lost("parse: %v", err))) // want: error
	logger.Error("a wrapper's error", zap.Error(lost("open: %v", opened())))
	logger.Error("a wrapper's error", zap.Error(errors.New(why))) // want: error
}

// Flow logs a local variable where its values reach: one assigned after the
// line reads it, or on a path that returns first, is not what the line logs;
// one assigned round a loop, or read or assigned in a closure, may be.
func Flow(logger *zap.Logger, s string, n int) error {
	why := "no match"
	utils.LogError(logger, nil, why)
	if err := parse(s); err != nil {
		why += " (closest: " + err.Error() + ")"
		return errors.New(why)
	}
	logger.Warn(why)
	once := "start"
	logger.Info(once)
	once = parse(s).Error()
	_ = once
	last := "none"
	for i := 0; i < n; i++ {
		logger.Info(last)
		last = parse(s).Error() // want: error
	}
	stop := "stopped"
	defer func() { logger.Info(stop) }()
	if n > 3 {
		stop = parse(s).Error() // want: error
		return nil
	}
	held := "none"
	twice := func() {
		logger.Info(held)
		held = parse(s).Error() // want: error
	}
	twice()
	twice()
	return nil
}

// unverified is a text of keploy's that holds an error's text.
func unverified(err error) string {
	return fmt.Sprintf("capture unverified (%v)", err)
}

// quoted is a text of keploy's that holds its parameter.
func quoted(v string) string { return "'" + v + "'" }

// loss is a value whose texts hold its fields.
type loss struct {
	Kind  Kind
	Count int
	What  string
}

// Text holds its enum and its number alone.
func (l loss) Text() string { return fmt.Sprintf("%d of %s", l.Count, l.Kind) }

// Where holds a string field.
func (l loss) Where() string { return "at " + l.What }

// Texts logs what keploy's own texts are made of: the arguments each puts in
// it, and its own values.
func Texts(logger *zap.Logger, s string, l loss) {
	logger.Warn("unverified", zap.String("reason", unverified(parse(s)))) // want: error
	logger.Warn("unverified", zap.String("reason", unverified(opened())))
	logger.Info("quoted", zap.String("q", quoted(s))) // want: field
	logger.Info("quoted", zap.String("q", quoted("a constant")))
	logger.Info("a loss", zap.String("loss", l.Text()))
	logger.Info("a loss", zap.String("where", l.Where())) // want: field
	reason := unverified(parse(s))                        // want: error
	logger.Warn("unverified", zap.String("reason", reason))
}

// Origins logs an error by every value that can reach the line: a branch's,
// and one a closure assigns; a value an assignment replaces on every path to
// the line does not reach it.
func Origins(logger *zap.Logger, s string, n int) {
	err := parse(s)
	if n > 0 {
		err = opened()
	}
	logger.Error("either", zap.Error(err)) // want: error
	err = opened()
	logger.Error("replaced", zap.Error(err))
	var lerr error
	read := func() { lerr = parse(s) }
	read()
	lerr = opened()
	logger.Error("a closure's", zap.Error(lerr)) // want: error
	for i := 0; i < n; i++ {
		logger.Error("round a loop", zap.Error(err)) // want: error
		err = parse(s)
	}
}

// Deferred logs an error a deferred closure assigns: it runs as the function
// returns, so what it assigns reaches its own lines, not the body's.
func Deferred(logger *zap.Logger, s string) (err error) {
	defer func() {
		err = parse(s)
		logger.Error("in the defer", zap.Error(err)) // want: error
	}()
	err = opened()
	logger.Error("the body's", zap.Error(err))
	go func() {
		logger.Error("a goroutine's, which may run after the defer", zap.Error(err)) // want: error
	}()
	return err
}

// Replaced logs an error an assignment replaces on every path to the line:
// an if's, a switch's or a for's Init before its body, and an assignment
// inside the closure that logs it, which control enters only at its start.
// One the closure does not replace is any value the function gives it.
func Replaced(logger *zap.Logger, s string, n int) {
	err := parse(s)
	if err = opened(); err != nil {
		logger.Error("an if's init replaces it", zap.Error(err))
	}
	err = parse(s)
	switch err = opened(); {
	case err != nil:
		logger.Error("a switch's init replaces it", zap.Error(err))
	}
	err = parse(s)
	for err = opened(); n > 0; n-- {
		logger.Error("a for's init replaces it", zap.Error(err))
		err = opened()
	}
	err = parse(s)
	if err = opened(); err != nil {
		err = parse(s)
		logger.Error("the if's body assigns it after its init", zap.Error(err)) // want: error
	}
	err = parse(s)
	defer func() {
		err = opened()
		if err != nil {
			logger.Error("the closure replaces it", zap.Error(err))
		}
	}()
	defer func() {
		if err != nil {
			logger.Error("the closure reads it", zap.Error(err)) // want: error
		}
	}()
	if err != nil {
		logger.Error("the body's", zap.Error(err)) // want: error
	}
}

// Once logs an error in a closure called once where it is written, which
// assigns the variable again after the line: it has no next call for that
// to reach. One called round a loop does.
func Once(logger *zap.Logger, s string, items []string) {
	var err error
	defer func() {
		err = opened()
		if err != nil {
			logger.Error("the closure's own value", zap.Error(err))
		}
		err = parse(s)
		_ = err
	}()
	var lerr error
	for range items {
		func() {
			if lerr != nil {
				logger.Error("a closure called round a loop", zap.Error(lerr)) // want: error
			}
			lerr = parse(s)
		}()
	}
}

// Calls logs in closures called more than once: what one call assigns, the
// next reads, unless the closure replaces it before the line.
func Calls(logger *zap.Logger, s string, items []string) {
	var err error
	save := func() {
		err = opened()
		if err != nil {
			logger.Error("replaced before the line on every call", zap.Error(err))
		}
		err = parse(s)
	}
	for range items {
		save()
	}
	var lerr error
	keep := func() {
		if lerr != nil {
			logger.Error("what the last call assigned", zap.Error(lerr)) // want: error
		}
		lerr = parse(s)
	}
	keep()
	keep()
}

// Jumps logs where a goto can carry a value: an assignment just before the
// line still replaces what came before it, unless a label the goto names
// lies between them.
func Jumps(logger *zap.Logger, s string, n int) {
	err := parse(s)
	if n > 0 {
		goto again
	}
	err = opened()
	if err != nil {
		logger.Error("replaced just before, no label between", zap.Error(err))
	}
	err = opened()
again:
	if err != nil {
		logger.Error("a label between", zap.Error(err)) // want: error
	}
}

// Breaks logs a value a loop's body assigns before a break and a return: the
// return is not what every path from the value runs.
func Breaks(logger *zap.Logger, s string, n int) {
	var lerr error
	for i := 0; i < n; i++ {
		lerr = parse(s)
		if i > 1 {
			break
		}
		return
	}
	logger.Error("after a break", zap.Error(lerr)) // want: error
}

// Nils logs an error a branch sets to nil, which is no error and no text.
func Nils(logger *zap.Logger, n int) {
	err := opened()
	if n > 0 {
		err = nil
	}
	logger.Error("opened, or nil", zap.Error(err))
}
