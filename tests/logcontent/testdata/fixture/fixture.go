// Package fixture holds one of each shape the guard in
// tests/logcontent reads: TestScanFindsEachShape holds the scan to the
// "want:" comment on each line (the kinds of the sites on it, in any order),
// and to no site on a line with none.
package fixture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
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
	enc.AddString("body", p.body) // an unexported field: each write of it is judged instead
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
	logger.Info("its own marshaler", zap.Object("p", payload{s})) // want: field
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

// Report is exported: a package the scan does not read may call it, so the
// parameter it logs is a site where it logs it, for those callers; and each
// call of it the scan reads is judged by what it gives it.
func Report(logger *zap.Logger, reason string) {
	logger.Warn("left out", zap.String("reason", reason)) // want: field
}

// Session logs through methods.
type Session struct{ logger *zap.Logger }

// StoppedOn is exported, and passes its reason on to leftOut, after a mark
// on one path.
func (s *Session) StoppedOn(reason string, marked bool) {
	if marked {
		reason = "marked: " + reason
	}
	s.leftOut(reason) // want: field
}

// leftOut is a method that no other package and no function value can call:
// its callers are judged by what they give it.
func (s *Session) leftOut(reason string) {
	s.logger.Warn("left out", zap.String("reason", "the exchange: "+reason))
}

// reporter is an interface keploy calls Session's say through.
type reporter interface{ say(why string) }

func (s *Session) say(why string) { s.logger.Info(why) }

// logFn hands back a logger's method, which a line calls as a value.
func (s *Session) logFn() func(string, ...zap.Field) { return s.logger.Debug }

// directive is a value whose Reason a line logs: every write of Reason the
// scan reads is judged by what it writes.
type directive struct {
	Reason string
	n      int
}

func (d directive) log(logger *zap.Logger) {
	logger.Warn("a directive", zap.String("reason", d.Reason), zap.Int("n", d.n)) // want: field
}

// setReason writes Reason from its parameter: its callers are judged.
func setReason(d *directive, why string) { d.Reason = why }

// Callers calls each function above that logs a parameter, and writes the
// field a line logs, with a constant and with a value the application sent.
func Callers(logger *zap.Logger, s string, k Kind) {
	Report(logger, "a constant")
	Report(logger, "read failed: "+parse(s).Error()) // want: error
	Report(logger, s)                                // want: field
	sess := &Session{logger}
	sess.StoppedOn("a constant", true)
	sess.StoppedOn(s, false) // want: field
	sess.leftOut("a constant")
	sess.leftOut(s) // want: field
	var r reporter = sess
	r.say("a constant")
	r.say("a message with " + s) // want: message
	d := directive{Reason: "a constant"}
	d.Reason = "another constant"
	d.Reason = s               // want: field
	d2 := directive{Reason: s} // want: field
	d3 := directive{s, 1}      // want: field
	setReason(&d, "a constant")
	setReason(&d, s) // want: field
	d.log(logger)
	d2.log(logger)
	d3.log(logger)
	logf := logger.Info
	logf("a constant")
	logf("a message with " + s)                                                                 // want: message
	sess.logFn()("a message with " + s)                                                         // want: message
	logger.Info("an error's text, fingerprinted", logcontent.String("error", parse(s).Error())) // want: error
	logger.Info("an error, by its kind", logcontent.Error(parse(s)))
	logger.Info("an opened file's error, fingerprinted", logcontent.String("error", opened().Error()))
	logger.Info("a named value", zap.String("variable", string(k)), logcontent.String("value", s)) // want: field
	logger.Info("a named value", zap.String("variable", string(k)), logcontent.NamedString("value", string(k), s))
}

// dbg is an interface a line goes through: a *zap.Logger is one.
type dbg interface{ Debug(string, ...zap.Field) }

// shown is a marshaler of keploy's whose String writes its payload.
type shown struct{ b []byte }

func (m shown) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddInt("len", len(m.b))
	return nil
}

func (m shown) String() string { return string(m.b) }

// counted is a Stringer of keploy's whose String writes a number alone.
type counted struct{ n int }

func (c counted) String() string { return strconv.Itoa(c.n) + " calls" }

// errKind is an error type of a string's kind, with a constant: converted
// from a value, it is that value's text.
type errKind string

const errKindConst errKind = "a constant error"

func (e errKind) Error() string { return string(e) }

// Shapes logs a value every other way a line can carry it: a logger's method
// as a value, through an interface, as a logger's name or a field's key; a
// zap.Reflect or zap.Stringer of a marshaler of keploy's, which neither
// marshals; a conversion into an enum, and an error type of a string's kind;
// a local merely named as a connection id; time's errors, which quote their
// input.
func Shapes(logger *zap.Logger, s string, h map[string][]string, k Kind, d time.Duration) {
	var d1 dbg = logger
	d1.Debug("a constant")
	d1.Debug("a message with " + s) // want: message
	lg := logger.Debug
	lg("a message with " + s) // want: message
	logger.Named("a constant").Info("named")
	logger.Named(s).Info("named")           // want: message
	logger.Info("a key", zap.Bool(s, true)) // want: field
	logger.Info("a key", zap.Bool("k"+string(k), true))
	logger.Info("a key", logcontent.String(s, s))                           // want: field
	logger.Info("a header's values", zap.Reflect("h", utils.SortedKeys(h))) // want: field
	logger.Info("a header's names", zap.Array("h", utils.SortedKeys(h)))
	logger.Info("a payload by its String", zap.Stringer("p", shown{[]byte(s)})) // want: field
	logger.Info("a payload by its marshaler", zap.Object("p", shown{[]byte(s)}))
	logger.Info("a count by its String", zap.Stringer("c", counted{len(s)}))
	logger.Info("an enum converted from a value", zap.String("kind", string(Kind(s)))) // want: field
	logger.Info("an enum converted from a value", zap.Any("kind", Kind(s)))            // want: field
	logger.Info("an enum", zap.Any("kind", k), zap.String("kind", string(KindHTTP)))
	logger.Info("an error type's value", zap.Any("e", errKind(s))) // want: error
	logger.Error("an error type's value", zap.Error(errKind(s)))   // want: error
	logger.Error("an error type's constant", zap.Error(errKindConst))
	connID := s
	logger.Info("a local named as a connection id", zap.String("conn", connID)) // want: field
	ctxID, _ := ctxOf(s).Value(connIDKey).(string)
	logger.Info("a context's connection id", zap.String("conn", ctxID))
	held := ""
	if v := ctxOf(s).Value(connIDKey); v != nil {
		if id, ok := v.(string); ok {
			held = id
		}
	}
	logger.Info("a context's connection id, by way of a local", zap.String("conn", held))
	if v := ctxOf(s).Value("a key"); v != nil {
		if id, ok := v.(string); ok {
			logger.Info("another value of a context", zap.String("v", id)) // want: field
		}
	}
	idx, _ := map[string]string{"k": s}["k"]
	logger.Info("a comma-ok index of a value", zap.String("held", idx)) // want: field
	_, err := time.Parse(time.RFC3339, s)
	logger.Error("time's error quotes its input", zap.Error(err)) // want: error
	logger.Info("a duration", zap.Duration("d", d))
}

// Bound logs through closures bound to a local, only ever called: each call
// is judged by what it gives the closure's parameter; one that escapes is a
// site where it logs it.
func Bound(logger *zap.Logger, s string) {
	fail := func(err error, msg string) {
		logger.Error(msg, zap.Error(err))
	}
	fail(opened(), "a constant")
	fail(opened(), "a message with "+s) // want: message
	fail(parse(s), "a constant")        // want: error
	escapes := func(msg string) {
		logger.Info(msg) // want: message
	}
	escapes("a constant")
	run(escapes)
}

func run(f func(string)) { f("from elsewhere") }

// Overgated logs by its kind an error whose origin carries no content: the
// guard says to log it whole.
func Overgated(logger *zap.Logger, s string) {
	logger.Error("open", logcontent.Error(opened())) // want: overgated
	utils.LogContentError(logger, opened(), "open")  // want: overgated
	logger.Error("parse", logcontent.Error(parse(s)))
	logger.Error("a nil error", logcontent.Error(nil))
	logger.Error("a closed store's error", logcontent.Error(store(fileStore{}).Get())) // want: overgated
	logger.Error("an open store's error", logcontent.Error(Store(fileStore{}).Get()))
}

// Store is an exported interface, which another module may implement with
// errors of its own: one of its errors is not over-gated.
type Store interface{ Get() error }

// Panics logs a recovered panic's value, whatever code raised it, and the
// connection id under its key in a context and a map, where every value put
// is judged.
func Panics(logger *zap.Logger, s string, id int64, ctx context.Context) context.Context {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("recovered", zap.Any("panic", r)) // want: field
			logger.Error("recovered", logcontent.Panic("panic", r))
			_ = errors.New(logcontent.PanicText(r))
		}
	}()
	meta := map[string]string{"connID": strconv.FormatInt(id, 10)}
	meta["connID"] = s                 // want: field
	_ = map[string]string{"connID": s} // want: field
	logger.Info("a mock's connection", zap.String("conn", meta["connID"]))
	ctx = context.WithValue(ctx, connIDKey, strconv.FormatInt(id, 10))
	return context.WithValue(ctx, connIDKey, s) // want: field
}

type ctxKey string

// connIDKey is the context's key of keploy's connection id.
const connIDKey ctxKey = "connID"

// held is a value whose unexported fields a line logs: every write of one is
// judged where it is made, and the read is no site, unless its address is
// taken, through which it may be written unseen.
type held struct{ why, also string }

func (h held) log(logger *zap.Logger) {
	logger.Info("held", zap.String("why", h.why))
	logger.Info("held", zap.String("also", h.also)) // want: field
}

// Held writes held's fields.
func Held(logger *zap.Logger, s string) {
	h := held{why: "a constant"}
	h.log(logger)
	h.why = s // want: field
	p := &h.also
	*p = s
	h = held{"a constant", "another"}
	h = held{s, "another"} // want: field
	h.log(logger)
}

func ctxOf(string) context.Context { return context.Background() }

// shaped is a value a line logs whose text a grammar shapes.
type shaped struct{ Text string }

// shape makes one from s: the write is a site, and so is each call that
// gives it a value the application sent, unless the allowlist lists the
// write as constructed, when what it is made from is not judged.
func shape(s string) shaped {
	return shaped{Text: s[:min(3, len(s))]} // want: field
}

// Shaped logs a shaped value.
func Shaped(logger *zap.Logger, s string) {
	logger.Info("a shape", zap.String("shape", shape(s).Text)) // want: field, field
}

// labelled is a value whose unexported field a text of keploy's reads: a
// line or an error that holds the text has each write of the field judged.
type labelled struct{ label string }

// Label is labelled's text.
func (l labelled) Label() string { return "label " + l.label }

// Labels logs labelled's text.
func Labels(logger *zap.Logger, s string) error {
	logger.Info("a label", zap.String("label", labelled{"a constant"}.Label()))
	logger.Info("a label", zap.String("label", labelled{s}.Label())) // want: field
	return fmt.Errorf("a label %s", labelled{s}.Label())             // want: field
}

// optedIn logs its parameter only where the switch's opt-in does: its callers
// give it what they like.
func optedIn(logger *zap.Logger, s string) {
	if logcontent.On() {
		logger.Info("whole, with the switch on", zap.String("s", s))
	}
}

// OptedIn calls it with a value the application sent: no site.
func OptedIn(logger *zap.Logger, s string) { optedIn(logger, s) }

// Prints writes to the process's stdout and stderr, which a user reads beside
// the log, and to a crash report, which leaves the machine: each value is
// judged as a field's is.
func Prints(logger *zap.Logger, w io.Writer, s string, n int) {
	fmt.Println("a constant", n)
	fmt.Println("a value", s) // want: print
	fmt.Printf("%d calls\n", n)
	fmt.Printf("a value %s\n", s) // want: print
	fmt.Printf("a type %T\n", s)
	fmt.Fprintln(os.Stderr, "a value", s) // want: print
	fmt.Fprintf(os.Stdout, "%d\n", n)
	fmt.Fprintln(w, "a value", s)
	fmt.Println("an error", parse(s)) // want: error
	fmt.Println("an error", opened())
	_, err := fmt.Fprintf(w, "a value %s", s)
	logger.Error("a print's error is its writer's", zap.Error(err))
	sentry.CaptureMessage("a constant")
	sentry.CaptureMessage(s)          // want: telemetry
	sentry.CaptureException(parse(s)) // want: error
	sentry.CaptureException(opened())
	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetExtra("a constant", n)
		scope.SetExtra("a value", s) // want: telemetry
	})
}

// kinds is a table of keploy's whose every value is a constant and which
// nothing writes: what an index of it reads is a constant.
var kinds = map[byte]string{1: "OK", 2: "ERR"}

// names is one too, but a line writes it.
var names = map[byte]string{1: "OK"}

// kindName is a text of kinds'.
func kindName(b byte) string {
	if k, ok := kinds[b]; ok {
		return k
	}
	return "UNKNOWN"
}

// Tables logs a table's values.
func Tables(logger *zap.Logger, b byte, s string) {
	logger.Info("a kind", zap.String("kind", kinds[b]), zap.String("name", kindName(b)))
	names[b] = s
	logger.Info("a name", zap.String("name", names[b])) // want: field
	for _, k := range kinds {
		_ = len(kinds) + len(k)
	}
}

// typed formats its value by its type alone, an error it logs whole: its
// callers give it what they like.
func typed(logger *zap.Logger, v any) {
	logger.Error("not TCP", zap.Error(fmt.Errorf("an accepted connection is not TCP (%T)", v)))
}

// Typed calls it with a value the application sent: no site.
func Typed(logger *zap.Logger, s string) { typed(logger, s) }

// banner is a text of keploy's in a variable nothing writes.
var banner = "keploy:"

// mood is one a line writes.
var mood = "calm"

// Vars logs package variables, and texts strings' and filepath's functions
// make of their arguments.
func Vars(logger *zap.Logger, s string) {
	logger.Info(banner + " a line")
	mood = s
	logger.Info(mood) // want: message
	logger.Info("trimmed", zap.String("t", strings.TrimSpace("x"+strconv.Itoa(len(s)))))
	logger.Info("trimmed", zap.String("t", strings.TrimSpace(s))) // want: field
	logger.Info("joined", zap.String("p", filepath.Join("a", banner)))
	logger.Info("cut", zap.Strings("p", strings.Split(s, ","))) // want: field
}

// said is a value as a line may say it: whole only where the switch's opt-in
// returns it.
func said(v string) string {
	if logcontent.On() {
		return strconv.Quote(v)
	}
	return strconv.Itoa(len(v)) + " bytes"
}

// unsaid returns the value whatever the switch says.
func unsaid(v string) string { return "value " + v }

// Said2 logs both.
func Said2(logger *zap.Logger, s string) {
	logger.Info("said", zap.String("v", said(s)))
	logger.Info("unsaid", zap.String("v", unsaid(s))) // want: field
}

// Hosts logs net's errors: a socket's names an address and an errno; a
// parse's quotes the text it was given.
func Hosts(logger *zap.Logger, s string) {
	if _, _, err := net.SplitHostPort(s); err != nil {
		logger.Error("not host:port", zap.Error(err)) // want: error
	}
	if _, err := net.Dial("tcp", "127.0.0.1:1"); err != nil {
		logger.Error("no dial", zap.Error(err))
	}
}
