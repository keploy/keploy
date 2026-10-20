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
