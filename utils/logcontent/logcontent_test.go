package logcontent

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// canary is content no line may carry with the switch off. Distinctive, so a
// match is the canary and nothing else.
const canary = "kp-canary-4f1c9e-customer-row"

// carries reports whether line holds the canary in any form a zap field
// writes content in: as text, as zap.Binary's base64, or as the observer's
// map prints a []byte.
func carries(line string) bool {
	return strings.Contains(line, canary) ||
		strings.Contains(line, base64.StdEncoding.EncodeToString([]byte(canary))) ||
		strings.Contains(line, strings.Trim(fmt.Sprint([]byte(canary)), "[]"))
}

// row is the shape of a protocol struct or a mock that carries content in its
// fields, as an Any call site dumps one.
type row struct {
	Table string
	Value string
}

// mock is one value, so that the observer's map, which prints its address,
// reads the same for each run that logs it.
var mock = &row{Table: "users", Value: canary}

// rawBody is a named []byte, as json.RawMessage is.
type rawBody []byte

// constructors are the field constructors a call site uses: the helpers, or
// the zap ones they stand in for.
type constructors struct {
	bytes  func(string, []byte) zap.Field
	binary func(string, []byte) zap.Field
	str    func(string, string) zap.Field
	any    func(string, any) zap.Field
}

var (
	helpers = constructors{Bytes, Binary, String, Any}
	zaps    = constructors{zap.ByteString, zap.Binary, zap.String, zap.Any}
)

// logEach logs one Debug line per constructor, and per kind of value Any
// treats apart, through logger, the way call sites build them.
func logEach(logger *zap.Logger, c constructors) {
	logger.Debug("frame", c.bytes("payload", []byte(canary)))
	logger.Debug("buffer", c.binary("buffer", []byte(canary)))
	logger.Debug("header", c.str("value", canary))
	logger.Debug("mock", c.any("mock", mock))
	logger.Debug("raw", c.any("raw", []byte(canary)))
	logger.Debug("text", c.any("text", canary))
	logger.Debug("named", c.any("body", rawBody(canary)))
}

// observed logs through a Debug-level observer core and returns each entry
// as it reaches a reader: the message and every field, as the observer maps
// them and as a console encoder writes the line.
func observed(t *testing.T, logLines func(*zap.Logger)) []string {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	logLines(zap.New(core))
	cfg := zap.NewDevelopmentEncoderConfig()
	cfg.TimeKey = "" // so two runs' lines compare equal
	enc := zapcore.NewConsoleEncoder(cfg)
	var out []string
	for _, e := range logs.All() {
		buf, err := enc.EncodeEntry(e.Entry, e.Context)
		if err != nil {
			t.Fatalf("encode %q: %v", e.Message, err)
		}
		out = append(out, fmt.Sprintf("%s %v %s", e.Message, e.ContextMap(), buf.String()))
		buf.Free()
	}
	return out
}

func TestWanted(t *testing.T) {
	for _, tc := range []struct {
		v    string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{" 1 ", true},
		{"\ttrue\n", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"yes", false},
		{"on", false},
		{"t", false},
		{"2", false},
		{"01", false},
		{"1 1", false},
		{"truee", false},
	} {
		if got := Wanted(tc.v); got != tc.want {
			t.Errorf("Wanted(%q) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

// The switch is read once, at start: setting the variable later changes
// nothing.
func TestOnIsReadOnceAtStart(t *testing.T) {
	start := On()
	if start {
		t.Setenv(EnvVar, "0")
	} else {
		t.Setenv(EnvVar, "1")
	}
	if On() != start {
		t.Fatalf("On() followed %s set after start", EnvVar)
	}
}

func TestSetRestores(t *testing.T) {
	start := On()
	restore := Set(!start)
	if On() == start {
		t.Fatalf("Set(%v) left On() = %v", !start, start)
	}
	restore()
	if On() != start {
		t.Fatalf("restore left On() = %v, want %v", !start, start)
	}

	// Nested, each restore puts back what its own Set found.
	outer := Set(true)
	inner := Set(false)
	inner()
	if !On() {
		t.Fatal("the inner restore did not put back the outer Set's true")
	}
	outer()
	if On() != start {
		t.Fatalf("the outer restore left On() = %v, want %v", On(), start)
	}
}

// A launcher passes the opt-in to the native agent it starts as AgentFlag,
// and nothing with the switch off, so the agent's argv is what it was; the
// agent that reads the flag opts in.
func TestAgentArgsCarryTheOptIn(t *testing.T) {
	t.Cleanup(Set(false))
	if got := AgentArgs(); got != nil {
		t.Fatalf("with the switch off, AgentArgs() = %q, want nothing", got)
	}
	OptIn()
	if !On() {
		t.Fatal("OptIn left the switch off")
	}
	if got := AgentArgs(); len(got) != 1 || got[0] != "--"+AgentFlag {
		t.Fatalf("with the switch on, AgentArgs() = %q, want [--%s]", got, AgentFlag)
	}
}

func TestNoHelperLogsContentWithTheSwitchOff(t *testing.T) {
	t.Cleanup(Set(false))
	lines := observed(t, func(l *zap.Logger) { logEach(l, helpers) })
	if len(lines) != 7 {
		t.Fatalf("logged %d lines, want 7", len(lines))
	}
	for _, line := range lines {
		if carries(line) {
			t.Errorf("a line carries the content with the switch off: %s", line)
		}
	}

	// What a line carries instead names the value.
	digest := fmt.Sprintf(`"len": %d, "fp": "%s"`, len(canary), Fingerprint([]byte(canary)))
	for i, want := range []string{
		`"payload": {` + digest + `}`,
		`"buffer": {` + digest + `}`,
		`"value": {` + digest + `}`,
		`"mock": {"type": "*logcontent.row"}`,
		`"raw": {"type": "[]uint8", ` + digest + `}`,
		`"text": {"type": "string", ` + digest + `}`,
		`"body": {"type": "logcontent.rawBody", ` + digest + `}`,
	} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d does not carry %s: %s", i, want, lines[i])
		}
	}
}

// With the switch on, each helper logs exactly what the zap constructor it
// stands in for does.
func TestEveryHelperLogsContentWithTheSwitchOn(t *testing.T) {
	t.Cleanup(Set(true))
	lines := observed(t, func(l *zap.Logger) { logEach(l, helpers) })
	want := observed(t, func(l *zap.Logger) { logEach(l, zaps) })
	if len(lines) != 7 || len(want) != 7 {
		t.Fatalf("logged %d and %d lines, want 7", len(lines), len(want))
	}
	for i, line := range lines {
		if !carries(line) {
			t.Errorf("a line does not carry the content with the switch on: %s", line)
		}
		if line != want[i] {
			t.Errorf("line %d is not what zap logs:\n got %s\nwant %s", i, line, want[i])
		}
	}
}

// A field whose key names a credential is logged by its length alone with
// the switch off, by every helper, since a fingerprint of a password, a PIN
// or a token from a small set gives it away to whoever hashes the guesses;
// with the switch on it is what zap logs, as for any other key.
func TestACredentialNamedFieldIsLoggedByItsLengthAlone(t *testing.T) {
	keys := []string{
		"password", "Password", "db_password", "normalised password", "user.password",
		"authorization", "Authorization", "Proxy-Authorization", "auth", "auth token", "authResponse",
		"auth_plugin_data", "AuthPluginData", "authplugindata", "authMechanismProperties",
		"token", "access_token", "idToken", "X-Auth-Token", "x-csrf-token",
		"secret", "client_secret", "SecretString", "secretAccessKey",
		"cookie", "Set-Cookie", "x-api-key", "apiKey", "api_key", "private_key",
		"pin", "pin_code", "otp", "OTPCode", "cvv", "ssn", "pwd", "passwd", "pass", "pw",
		"session_id", "sessionId", "key", "server key", "StoredKey", "hmac", "signature", "sig",
		"credentials", "dsn", "connection_string", "passphrase", "bearer", "jwt",
		"passcode", "totp", "totp_code", "hotp", "mfa_code", "MFACode", "2fa", "otpcode", "pincode",
		"cvv2", "cvc2", "pin1", "oauth2", "OAuth2Token", "privKey", "priv_key",
		// A key written in capitals alone has lost its words' boundaries.
		"DBPASS", "REDISPASS", "SMTPPASS", "MYSQLPWD", "APPPIN", "USEROTP", "DBPW",
		"DBPASSHASH", "ADMINPWDFILE", "USEROTPSEED",
		// A plural acronym, a code that starts with a digit, a word that ends in
		// a credential's, and a key whose last word is key.
		"OTPs", "PINs", "SSNs", "CVVs", "userPINs", "backupOTPs", "pins", "otps",
		"2FA", "X-2FA-Code", "2FACode",
		"dbpass", "userpass", "adminpwd", "mysqlpwd",
		"Ocp-Apim-Subscription-Key", "x-functions-key", "SubscriptionKey", "license_key", "cache key", "signing keys",
		// A last word of key with digits (a rotated key), and a session's
		// cookie or a CSRF token by the names frameworks give them.
		"key2", "keys2", "secondary_key2", "x-api-key2",
		"JSESSIONID", "PHPSESSID", "connect.sid", "sid", "session", "laravel_session", "ASP.NET_SessionId",
		"_csrf", "csrf", "xsrf", "XSRF-TOKEN", "csrftoken",
		// A card's or an account's number, and a code sent to verify one,
		// from sets small enough to hash.
		"card_number", "cardNumber", "CARDNO", "credit_card", "cc_number", "pan", "PAN", "iban",
		"verification_code", "verificationCode", "security_code",
		"accountNumber", "account_number", "acct_no", "routing_number", "card_num", "cardNum", "ccNum", "card_pan", "pan_number",
		// Their long forms and other spellings, and a key a credential's word
		// runs into in lower case.
		"social_security_number", "socialSecurityNumber", "card_verification_value", "ccv", "csc", "cvn",
		"authorisation", "Proxy-Authorisation", "authkey", "passkey", "sharedkey", "sslkey", "aeskey",
		"tlskey", "rsakeypem", "TLSKeyFile",
	}
	for _, key := range keys {
		logEachUnder := func(l *zap.Logger, c constructors) {
			l.Debug("frame", c.bytes(key, []byte(canary)))
			l.Debug("buffer", c.binary(key, []byte(canary)))
			l.Debug("header", c.str(key, canary))
			l.Debug("raw", c.any(key, []byte(canary)))
			l.Debug("text", c.any(key, canary))
		}
		t.Run(key, func(t *testing.T) {
			fp := Fingerprint([]byte(canary))
			restore := Set(false)
			hashes := countHashes(t)
			off := observed(t, func(l *zap.Logger) { logEachUnder(l, helpers) })
			restore()
			if len(off) != 5 {
				t.Fatalf("logged %d lines, want 5", len(off))
			}
			for i, line := range off {
				if carries(line) || strings.Contains(line, `"fp"`) || strings.Contains(line, fp) {
					t.Errorf("line %d names a credential by more than its length: %s", i, line)
				}
			}
			for i, want := range []string{
				`{"len": %d}`, `{"len": %d}`, `{"len": %d}`,
				`{"type": "[]uint8", "len": %d}`, `{"type": "string", "len": %d}`,
			} {
				if want = fmt.Sprintf(want, len(canary)); !strings.Contains(off[i], want) {
					t.Errorf("line %d does not carry %s: %s", i, want, off[i])
				}
			}
			if n := hashes.Load(); n != 0 {
				t.Errorf("a credential's lines hashed %d times, want 0", n)
			}

			t.Cleanup(Set(true))
			on := observed(t, func(l *zap.Logger) { logEachUnder(l, helpers) })
			want := observed(t, func(l *zap.Logger) { logEachUnder(l, zaps) })
			for i := range on {
				if on[i] != want[i] {
					t.Errorf("with the switch on, line %d is not what zap logs:\n got %s\nwant %s", i, on[i], want[i])
				}
			}
		})
	}
}

// The names enterprise's secret protection redacts a zap.String field under
// by default (pkg/secret: defaultSensitiveHeaders, defaultSensitiveBodyKeys,
// defaultSensitiveURLParams, logOnlyJSONKeys). Its RedactField reads only a
// zap.String field, which String's off field is not, so each of these must be
// a credential's name here, or a field it redacted before is fingerprinted
// now.
var enterpriseSensitiveNames = []string{
	// headers
	"authorization", "cookie", "set-cookie", "x-api-key", "x-auth-token", "x-csrf-token",
	"x-xsrf-token", "proxy-authorization", "www-authenticate", "x-forwarded-authorization",
	"x-amz-security-token",
	// body keys
	"password", "secret", "token", "access_token", "refresh_token", "api_key", "apikey",
	"private_key", "client_secret", "credentials", "secret_key", "session_token", "id_token",
	"auth_token", "pass", "pw", "passphrase", "connection_string", "dsn", "db_password",
	"secret_access_key", "master_key", "pin", "otp", "cvv", "ssn", "encryption_key",
	"signing_key", "hmac",
	// URL params
	"key", "auth", "signature", "sig", "session_id", "passwd", "pwd", "sslpassword",
	"authmechanismproperties", "tlscertificatekeyfilepassword", "proxypassword",
	// log-only JSON keys
	"secretstring", "authresponse", "auth_response", "authplugindata", "auth_plugin_data",
	"normalised password", "normalised_password", "normalisedpassword",
	"normalized password", "normalized_password", "normalizedpassword",
	"salted password", "salted_password", "saltedpassword",
	"server key", "server_key", "serverkey", "stored key", "stored_key", "storedkey",
	"client key", "client_key", "clientkey",
	"server signature", "server_signature", "serversignature",
	"auth message", "auth_message", "authmessage",
	"authentication message", "authentication_message", "authenticationmessage",
	"derived from auth message",
}

func TestEnterpriseSensitiveNamesAreCredentialNames(t *testing.T) {
	for _, name := range enterpriseSensitiveNames {
		for _, spelling := range []string{name, strings.ToUpper(name)} {
			if !credentialName(spelling) {
				t.Errorf("credentialName(%q) = false: secret protection redacted a field under it", spelling)
			}
		}
	}
}

// A key that holds a credential's word only inside another word, or that
// names something else, keeps its fingerprint: the length alone is for what
// a fingerprint would give away.
func TestAKeyThatNamesNoCredentialKeepsItsFingerprint(t *testing.T) {
	for _, key := range []string{
		"payload", "value", "body", "row", "query", "statement", "mock", "frame",
		"mapping", "pinned", "spinner", "shipping", "keyspace", "monkey", "hockey", "keyCount",
		"author", "authority", ":authority", "userIDs", "URLs",
		"classname", "passage", "signal", "design", "topic", "hotpath",
		"header names", "content-type", "x-request-id", "",
		// A trace's span and its id name no credential, nor does a code.
		"span", "span_id", "spanId", "status_code", "code", "company",
		// In capitals alone, a word holds a credential's only when that word
		// has three letters or more: UPWARD holds pw.
		"UPWARD", "REQUEST_ID",
	} {
		if credentialName(key) {
			t.Errorf("credentialName(%q) = true", key)
		}
	}
}

// Deciding whether a key names a credential costs an encoded line no
// allocation: it runs for every field the agent's debug file writes.
func TestCredentialNameAllocatesNothing(t *testing.T) {
	for _, key := range []string{"X-Amz-Security-Token", "partition key", "DBPASS", "OAuth2Token", "value", "X-Forwarded-Authorization-For-Upstream-Proxy-Hop"} {
		if n := testing.AllocsPerRun(100, func() { credentialName(key) }); n != 0 {
			t.Errorf("credentialName(%q) allocates %v times, want 0", key, n)
		}
	}
}

// NamedString is for a value whose name a sibling field holds, as a header's
// or a variable's does: logged under "value", it is logged by its length
// alone when that name, or its own key, names a credential.
func TestNamedStringIsLoggedByItsLengthAloneWhenItsNameIsACredential(t *testing.T) {
	logNamed := func(l *zap.Logger, named func(key, name, s string) zap.Field) {
		l.Debug("header", zap.String("name", "Authorization"), named("value", "Authorization", canary))
		l.Debug("variable", zap.String("variable", "resetToken"), named("value", "resetToken", canary))
		l.Debug("header", zap.String("name", "Content-Type"), named("value", "Content-Type", canary))
		l.Debug("header", zap.String("name", "Content-Type"), named("password", "Content-Type", canary))
		l.Debug("header", named("value", "", canary))
	}
	restore := Set(false)
	off := observed(t, func(l *zap.Logger) { logNamed(l, NamedString) })
	restore()
	digest := fmt.Sprintf(`"len": %d, "fp": "%s"`, len(canary), Fingerprint([]byte(canary)))
	lengthAlone := fmt.Sprintf(`{"len": %d}`, len(canary))
	if len(off) != 5 {
		t.Fatalf("logged %d lines, want 5", len(off))
	}
	for i, want := range []string{
		`"value": ` + lengthAlone,
		`"value": ` + lengthAlone,
		`"value": {` + digest + `}`,
		`"password": ` + lengthAlone,
		`"value": {` + digest + `}`,
	} {
		if carries(off[i]) || !strings.Contains(off[i], want) {
			t.Errorf("line %d does not carry %s alone: %s", i, want, off[i])
		}
	}

	t.Cleanup(Set(true))
	on := observed(t, func(l *zap.Logger) { logNamed(l, NamedString) })
	zapped := observed(t, func(l *zap.Logger) {
		logNamed(l, func(key, _, s string) zap.Field { return zap.String(key, s) })
	})
	for i := range on {
		if on[i] != zapped[i] {
			t.Errorf("with the switch on, line %d is not what zap logs:\n got %s\nwant %s", i, on[i], zapped[i])
		}
	}
}

// StringLen and BytesLen log a value by its length alone with the switch off,
// whatever its key, and hash nothing; with it on they are zap.String and
// zap.ByteString.
func TestTheLengthHelpersLogTheLengthAlone(t *testing.T) {
	logBoth := func(l *zap.Logger, str func(string, string) zap.Field, b func(string, []byte) zap.Field) {
		l.Debug("status", str("status", canary))
		l.Debug("code", b("code", []byte(canary)))
	}
	restore := Set(false)
	hashes := countHashes(t)
	off := observed(t, func(l *zap.Logger) { logBoth(l, StringLen, BytesLen) })
	restore()
	want := fmt.Sprintf(`{"len": %d}`, len(canary))
	if len(off) != 2 {
		t.Fatalf("logged %d lines, want 2", len(off))
	}
	for _, line := range off {
		if carries(line) || strings.Contains(line, `"fp"`) || !strings.Contains(line, want) {
			t.Errorf("with the switch off, a line is not the value's length alone, %s: %s", want, line)
		}
	}
	if n := hashes.Load(); n != 0 {
		t.Errorf("the length helpers hashed %d times, want 0", n)
	}

	// A value shorter than 256 bytes costs a disabled line nothing zap's
	// constructors do not.
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.InfoLevel))
	short := strings.Repeat("7", 255)
	if ours, zaps := testing.AllocsPerRun(100, func() { logger.Debug("status", StringLen("status", short)) }),
		testing.AllocsPerRun(100, func() { logger.Debug("status", zap.String("status", short)) }); ours != zaps {
		t.Errorf("a disabled line with StringLen of 255 bytes allocates %v times, with zap.String %v", ours, zaps)
	}
	// BytesLen costs one nothing for a value shorter than 256 bytes, as
	// zap.String costs it, and its length's box for a longer one, as
	// zap.ByteString costs the slice's box.
	b := bytes.Repeat([]byte("7"), 255)
	if ours, zaps := testing.AllocsPerRun(100, func() { logger.Debug("code", BytesLen("code", b)) }),
		testing.AllocsPerRun(100, func() { logger.Debug("code", zap.String("code", short)) }); ours != zaps {
		t.Errorf("a disabled line with BytesLen of 255 bytes allocates %v times, with zap.String %v", ours, zaps)
	}
	b = bytes.Repeat([]byte("7"), 4096)
	if ours, zaps := testing.AllocsPerRun(100, func() { logger.Debug("code", BytesLen("code", b)) }),
		testing.AllocsPerRun(100, func() { logger.Debug("code", zap.ByteString("code", b)) }); ours != zaps {
		t.Errorf("a disabled line with BytesLen of 4096 bytes allocates %v times, with zap.ByteString %v", ours, zaps)
	}

	t.Cleanup(Set(true))
	on := observed(t, func(l *zap.Logger) { logBoth(l, StringLen, BytesLen) })
	zapped := observed(t, func(l *zap.Logger) { logBoth(l, zap.String, zap.ByteString) })
	for i := range on {
		if on[i] != zapped[i] {
			t.Errorf("with the switch on, line %d is not what zap logs:\n got %s\nwant %s", i, on[i], zapped[i])
		}
	}
}

func TestAnyOfNilNamesNoType(t *testing.T) {
	t.Cleanup(Set(false))
	lines := observed(t, func(l *zap.Logger) { l.Debug("nil", Any("v", nil)) })
	if len(lines) != 1 || !strings.Contains(lines[0], `"v": {"type": "<nil>"}`) {
		t.Fatalf("Any(nil) logged %q", lines)
	}
}

// parseErr is a parser's error that quotes what it could not read, as
// net/http's does a header line, wrapped as a parser wraps it.
var parseErr = fmt.Errorf("parse request: %w", textproto.ProtocolError("malformed MIME header: missing colon: "+strconv.Quote("X-Token "+canary)))

// An error is logged by its kind and its text's length with the switch off,
// with no fingerprint: the text quotes the value among words anyone can read,
// so a short value is found by hashing the texts it would make. With the
// switch on, Error and NamedError are zap's, and a nil error logs nothing
// either way.
func TestErrorIsLoggedByItsKindAndLength(t *testing.T) {
	logBoth := func(l *zap.Logger, e func(error) zap.Field, ne func(string, error) zap.Field) {
		l.Debug("parse", e(parseErr))
		l.Debug("named", ne("cause", parseErr))
		l.Debug("nil", e(nil))
	}
	t.Run("off", func(t *testing.T) {
		t.Cleanup(Set(false))
		fp := Fingerprint([]byte(parseErr.Error()))
		hashes := countHashes(t)
		lines := observed(t, func(l *zap.Logger) { logBoth(l, Error, NamedError) })
		for _, line := range lines {
			if carries(line) || strings.Contains(line, fp) {
				t.Errorf("a line carries the error's text or its fingerprint: %s", line)
			}
		}
		for i, want := range []string{
			fmt.Sprintf(`"error": {"kind": "textproto.ProtocolError", "len": %d}`, len(parseErr.Error())),
			fmt.Sprintf(`"cause": {"kind": "textproto.ProtocolError", "len": %d}`, len(parseErr.Error())),
		} {
			if !strings.Contains(lines[i], want) {
				t.Errorf("line %d does not carry %s: %s", i, want, lines[i])
			}
		}
		if strings.Contains(lines[2], "error") {
			t.Errorf("a nil error was logged: %s", lines[2])
		}
		if n := hashes.Load(); n != 0 {
			t.Errorf("an error's line hashed %d times, want 0", n)
		}
	})
	t.Run("on", func(t *testing.T) {
		t.Cleanup(Set(true))
		on := observed(t, func(l *zap.Logger) { logBoth(l, Error, NamedError) })
		zapped := observed(t, func(l *zap.Logger) { logBoth(l, zap.Error, zap.NamedError) })
		for i := range on {
			if on[i] != zapped[i] {
				t.Errorf("with the switch on, line %d is not what zap logs:\n got %s\nwant %s", i, on[i], zapped[i])
			}
		}
		if !carries(on[0]) {
			t.Errorf("with the switch on, the error's text is not logged: %s", on[0])
		}
	})
}

// Error's off field reads err when the entry is encoded: a disabled line
// neither names its kind nor formats its text.
func TestADisabledErrorLineFormatsNothing(t *testing.T) {
	t.Cleanup(Set(false))
	var formatted atomic.Int64
	err := countingError{&formatted}
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.InfoLevel))
	for range 100 {
		logger.Debug("parse", Error(err))
	}
	if n := formatted.Load(); n != 0 {
		t.Fatalf("100 disabled lines formatted the error %d times, want 0", n)
	}
	logger.Info("parse", Error(err))
	if n := formatted.Load(); n != 1 {
		t.Fatalf("an encoded line formatted the error %d times, want 1", n)
	}
}

// countingError counts the times its text is formatted.
type countingError struct{ n *atomic.Int64 }

func (e countingError) Error() string { e.n.Add(1); return canary }

// An error is named by its cause however it was wrapped, past the text the
// errors and fmt packages carry, an errno by what it says; one of text alone
// by the sentinel it ends in, and failing that by its own type.
func TestErrorKind(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{parseErr, "textproto.ProtocolError"},
		{fmt.Errorf("invalid content-length: %w", &strconv.NumError{Func: "Atoi", Num: canary, Err: strconv.ErrSyntax}), "*strconv.NumError"},
		{&url.Error{Op: "Get", URL: "http://app/", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}, "syscall.Errno(" + syscall.ECONNREFUSED.Error() + ")"},
		{&net.OpError{Op: "read", Net: "tcp", Err: errors.New("i/o timeout")}, "*net.OpError"},
		{fmt.Errorf("%w: %s", io.ErrUnexpectedEOF, "the COM_QUERY response"), "io.ErrUnexpectedEOF"},
		{fmt.Errorf("read the request: %w", fmt.Errorf("after %q: %w", canary, io.EOF)), "io.EOF"},
		{errors.Join(parseErr, io.EOF), "*errors.joinError"},
		{errors.New("unexpected " + canary), "*errors.errorString"},
		{io.EOF, "io.EOF"},
		{fmt.Errorf("read the mocks of %s: %w", canary, os.ErrNotExist), "os.ErrNotExist"},
		{fmt.Errorf("scan the frame: %w", bufio.ErrTooLong), "bufio.ErrTooLong"},
		{nil, "<nil>"},
	} {
		if got := ErrorKind(c.err); got != c.want {
			t.Errorf("ErrorKind(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestFingerprint(t *testing.T) {
	b := []byte(canary)
	sum := sha256.Sum256(b)
	if got, want := Fingerprint(b), fmt.Sprintf("%x", sum[:6]); got != want {
		t.Fatalf("Fingerprint = %q, want %q", got, want)
	}
	if got := Fingerprint([]byte("hello")); got != "2cf24dba5fb0" {
		t.Fatalf(`Fingerprint("hello") = %q, want the first 12 hex digits of its sha256`, got)
	}
}

// The fingerprint is what the package doc says it is: no redaction. A PIN
// logged by String is found from its line by hashing the ten thousand PINs,
// and it is named alike on every line, as on a second run's; StringLen leaves
// nothing to hash.
func TestAFingerprintGivesAwayAValueFromASmallSet(t *testing.T) {
	const pin = "4821"
	t.Cleanup(Set(false))
	lines := observed(t, func(l *zap.Logger) {
		l.Debug("verify", String("value", pin))
		l.Debug("verify", String("value", pin))
		l.Debug("verify", StringLen("value", pin))
	})
	if len(lines) != 3 {
		t.Fatalf("logged %d lines, want 3", len(lines))
	}
	fp := regexp.MustCompile(`"fp": "([0-9a-f]{12})"`)
	m := fp.FindStringSubmatch(lines[0])
	if m == nil {
		t.Fatalf("String's line has no fingerprint: %s", lines[0])
	}
	var found []string
	for i := range 10000 {
		if guess := fmt.Sprintf("%04d", i); Fingerprint([]byte(guess)) == m[1] {
			found = append(found, guess)
		}
	}
	if len(found) != 1 || found[0] != pin {
		t.Fatalf("hashing every PIN found %q from the line, want [%s]", found, pin)
	}
	if again := fp.FindStringSubmatch(lines[1]); again == nil || again[1] != m[1] {
		t.Fatalf("the same PIN is named apart on two lines: %s, %s", lines[0], lines[1])
	}
	if strings.Contains(lines[2], `"fp"`) || !strings.Contains(lines[2], `"value": {"len": 4}`) {
		t.Fatalf("StringLen's line is not the PIN's length alone: %s", lines[2])
	}
}

// countHashes makes every fingerprint count itself until the test ends.
func countHashes(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := sum256
	sum256 = func(b []byte) [sha256.Size]byte {
		n.Add(1)
		return orig(b)
	}
	t.Cleanup(func() { sum256 = orig })
	return &n
}

// With the switch off, a field costs no hash until a core encodes its entry:
// a Debug line whose level is off, on the console as in the debug file once
// its cap has disabled it, hashes nothing.
func TestADisabledLineHashesNothing(t *testing.T) {
	t.Cleanup(Set(false))
	hashes := countHashes(t)
	payload := bytes.Repeat([]byte(canary), 64)
	var out bytes.Buffer
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&out), zapcore.InfoLevel)
	logger := zap.New(core)

	for range 100 {
		logger.Debug("frame", Bytes("payload", payload), String("value", string(payload)), Any("raw", payload), Any("mock", &row{Value: canary}))
	}
	if n := hashes.Load(); n != 0 {
		t.Fatalf("100 disabled lines hashed %d times, want 0", n)
	}
	if out.Len() != 0 {
		t.Fatalf("a disabled line was written: %s", out.String())
	}

	// The same fields on a line that is written are hashed then, once each.
	logger.Info("frame", Bytes("payload", payload), String("value", string(payload)), Any("raw", payload), Any("mock", &row{Value: canary}))
	if n := hashes.Load(); n != 3 {
		t.Fatalf("an encoded line hashed %d times, want 3 (Bytes, String and Any of a []byte)", n)
	}
	if !strings.Contains(out.String(), Fingerprint(payload)) {
		t.Fatalf("the written line does not name the payload: %s", out.String())
	}
}

// Bytes with the switch off costs a disabled line no allocation zap.ByteString
// does not, so a hot path that switches to it pays no allocation for the
// switch.
func TestBytesCostsADisabledLineWhatByteStringDoes(t *testing.T) {
	t.Cleanup(Set(false))
	payload := bytes.Repeat([]byte(canary), 64)
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.InfoLevel))
	ours := testing.AllocsPerRun(1000, func() { logger.Debug("frame", Bytes("payload", payload)) })
	zaps := testing.AllocsPerRun(1000, func() { logger.Debug("frame", zap.ByteString("payload", payload)) })
	if ours != zaps {
		t.Fatalf("a disabled line with Bytes allocates %v times, with zap.ByteString %v", ours, zaps)
	}
}

// Bytes at a disabled level, against zap.ByteString and against hashing the
// payload when the field is made, which is what the lazy digest saves.
func BenchmarkBytesAtADisabledLevel(b *testing.B) {
	payload := bytes.Repeat([]byte(canary), 2048) // ~60 KiB, a large frame
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.InfoLevel))
	b.Run("off", func(b *testing.B) {
		b.Cleanup(Set(false))
		b.ReportAllocs()
		for b.Loop() {
			logger.Debug("frame", Bytes("payload", payload))
		}
	})
	b.Run("on", func(b *testing.B) {
		b.Cleanup(Set(true))
		b.ReportAllocs()
		for b.Loop() {
			logger.Debug("frame", Bytes("payload", payload))
		}
	})
	b.Run("zap.ByteString", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			logger.Debug("frame", zap.ByteString("payload", payload))
		}
	})
	b.Run("eager-hash", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			logger.Debug("frame", zap.String("payload", Fingerprint(payload)))
		}
	})
}

// runtimePanic is the value the Go runtime panics with for an index out of
// range, as a parser's recover() gets it.
func runtimePanic() (v any) {
	defer func() { v = recover() }()
	var s []int
	i := len(canary)
	_ = s[i]
	return nil
}

// fakeRuntimeError has a RuntimeError method, as any package's type may: it
// is no panic the runtime raised.
type fakeRuntimeError struct{ s string }

func (e fakeRuntimeError) Error() string { return e.s }

func (fakeRuntimeError) RuntimeError() {}

func TestAPanicIsLoggedByItsKindUnlessTheRuntimeRaisedIt(t *testing.T) {
	rt := runtimePanic()
	if rt == nil {
		t.Fatal("no runtime panic")
	}
	values := []any{
		"bad frame " + canary,
		[]byte(canary),
		fmt.Errorf("decode: %w", &strconv.NumError{Func: "Atoi", Num: canary, Err: strconv.ErrSyntax}),
		fakeRuntimeError{"runtime error: " + canary},
		mock,
		rt,
		nil,
	}
	logAll := func(l *zap.Logger, field func(string, any) zap.Field) {
		for _, v := range values {
			l.Error("Recovered from panic in parser", field("panic", v))
		}
	}
	t.Run("off", func(t *testing.T) {
		t.Cleanup(Set(false))
		lines := observed(t, func(l *zap.Logger) { logAll(l, Panic) })
		for i, line := range lines[:len(values)-2] {
			if carries(line) {
				t.Errorf("line %d carries the panic's value: %s", i, line)
			}
			if strings.Contains(line, Fingerprint([]byte(canary))) || strings.Contains(line, Fingerprint([]byte("bad frame "+canary))) {
				t.Errorf("line %d carries the panic's fingerprint: %s", i, line)
			}
		}
		for i, want := range []string{
			fmt.Sprintf(`"panic": {"type": "string", "len": %d}`, len("bad frame "+canary)),
			fmt.Sprintf(`"panic": {"type": "[]uint8", "len": %d}`, len(canary)),
			`"panic": {"kind": "*strconv.NumError", "len": `,
			`"panic": {"kind": "logcontent.fakeRuntimeError", "len": `,
			`"panic": {"type": "*logcontent.row"}`,
			fmt.Sprintf(`"panic": "runtime error: index out of range [%d] with length 0"`, len(canary)),
			`"panic": {"type": "<nil>"}`,
		} {
			if !strings.Contains(lines[i], want) {
				t.Errorf("line %d does not carry %s: %s", i, want, lines[i])
			}
		}
		for _, line := range lines {
			if !strings.Contains(line, `"panic"`) {
				t.Errorf("a line names no panic: %s", line)
			}
		}
	})
	t.Run("on", func(t *testing.T) {
		t.Cleanup(Set(true))
		on := observed(t, func(l *zap.Logger) { logAll(l, Panic) })
		zapped := observed(t, func(l *zap.Logger) { logAll(l, zap.Any) })
		for i := range on {
			if on[i] != zapped[i] {
				t.Errorf("with the switch on, line %d is not what zap.Any logs:\n got %s\nwant %s", i, on[i], zapped[i])
			}
		}
	})
}

// A crash report leaves the machine: PanicText carries no content with the
// switch on or off, and keeps the runtime's own words.
func TestPanicTextCarriesNoContentWhateverTheSwitch(t *testing.T) {
	for _, sw := range []bool{false, true} {
		restore := Set(sw)
		for v, want := range map[any]string{
			"bad frame " + canary:    fmt.Sprintf("a panic of string (%d bytes)", len("bad frame "+canary)),
			fakeRuntimeError{canary}: fmt.Sprintf("a panic of logcontent.fakeRuntimeError (%d bytes)", len(canary)),
			mock:                     "a panic of *logcontent.row",
		} {
			if got := PanicText(v); got != want {
				t.Errorf("switch %v: PanicText(%T) = %q, want %q", sw, v, got, want)
			}
		}
		if got, want := PanicText(runtimePanic()), fmt.Sprintf("runtime error: index out of range [%d] with length 0", len(canary)); got != want {
			t.Errorf("switch %v: PanicText(a runtime panic) = %q, want %q", sw, got, want)
		}
		restore()
	}
}

// A crash report keeps keploy's own words of a panic it raised, so Sentry
// groups and reads it: a text keploy's code makes whole, and a format's
// words with {} for each value, the values' content withheld; the runtime's
// error whole, as before. An error's chain is read as Error's shape reads
// it.
func TestPanicTextKeepsKeploysOwnWords(t *testing.T) {
	for _, sw := range []bool{false, true} {
		restore := Set(sw)
		const text = "BindEvents map is nil"
		for v, want := range map[any]string{
			text:             "a panic of string: " + text,
			errors.New(text): "a panic of *errors.errorString: " + text,
			fmt.Sprintf("invalid port in URL: %s", canary):                   "a panic of string: invalid port in URL: {}",
			fmt.Errorf("failed to read %s of %s: %w", "hdr", canary, io.EOF): "a panic of io.EOF: failed to read {} of {}: EOF",
		} {
			if got := PanicText(v); got != want {
				t.Errorf("switch %v: PanicText(%q) = %q, want %q", sw, fmt.Sprint(v), got, want)
			}
		}
		restore()
	}
}

// Withheld names the fields the switch withheld a value from, which a logger
// adds its Hint to a line for, and none other.
func TestWithheldNamesTheFieldsThatWithholdAValue(t *testing.T) {
	t.Cleanup(Set(false))
	for _, f := range []zap.Field{
		Bytes("b", []byte(canary)), Binary("b", []byte(canary)), String("s", canary), NamedString("v", "n", canary),
		StringLen("s", canary), BytesLen("b", []byte(canary)), Any("a", mock), Error(parseErr), NamedError("e", parseErr),
		Panic("p", canary),
	} {
		if !Withheld(f) {
			t.Errorf("Withheld(%s) = false, want true", f.Key)
		}
	}
	for _, f := range []zap.Field{
		zap.String("s", canary), zap.Error(parseErr), Error(nil), Panic("p", runtimePanic()), zap.Object("o", zapcore.ObjectMarshalerFunc(func(zapcore.ObjectEncoder) error { return nil })),
	} {
		if Withheld(f) {
			t.Errorf("Withheld(%s) = true, want false", f.Key)
		}
	}
	restore := Set(true)
	defer restore()
	if Withheld(String("s", canary)) {
		t.Error("with the switch on, String's field is withheld")
	}
}
