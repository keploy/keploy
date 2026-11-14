package utils

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// URLLogMeta is what a log line says about a request to rawURL: what
// GetReqMeta says of a request (its method, its host, its path's shape and
// the names of its query parameters), and its scheme. Never a path segment, a
// query value or userinfo: a token rides in any of them (?access_token=, an
// OAuth code=, a reset link's path, user:password@), and keploy's log lines
// reach log stores. It is read as keploy reads a recorded request's URL
// (models.ParseRequestURL): url.Parse would take the token of a path that
// begins with "//" (//<token>/v1/me) for a host and name it. A URL that does
// not parse is named by its size only: the parser's error quotes it.
func URLLogMeta(method, rawURL string) map[string]string {
	u, err := models.ParseRequestURL(rawURL)
	if err != nil {
		return map[string]string{"method": method, "url": "unparsable", "bytes": strconv.Itoa(len(rawURL))}
	}
	meta := GetReqMeta(&http.Request{Method: method, Host: u.Host, URL: u})
	if u.Scheme != "" {
		meta["scheme"] = u.Scheme
	}
	return meta
}

// quotedInput is a Go-quoted string (strconv.Quote, %q) in an error's text.
var quotedInput = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// backquotedInput is a backquoted string in an error's text: YAML's errors
// quote the value they refused so ("cannot unmarshal !!str `...`").
var backquotedInput = regexp.MustCompile("`[^`]*`")

// WithholdInput is err with the application's bytes it quotes withheld, for
// an error a log line holds or a caller logs. Parsing what the application
// sent fails with an error that quotes it: net/http's "malformed HTTP
// request" quotes the request line, query values and all; textproto's
// "malformed MIME header line: " ends with the raw line, a header's value
// with it; strconv's "parsing" errors quote the value they refused, YAML's
// in backquotes; a connection joined mid-stream hands the parser a body's
// line. Each quoted
// string is written as its size, and a textproto.ProtocolError's text after
// its kind is withheld. errors.Is and errors.As still see err; an err that
// quotes nothing is returned as it is.
func WithholdInput(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	var pe textproto.ProtocolError
	if errors.As(err, &pe) {
		if kind, _, ok := strings.Cut(string(pe), ": "); ok {
			text = strings.Replace(text, string(pe), kind+": (input withheld, "+strconv.Itoa(len(pe)-len(kind)-2)+" bytes)", 1)
		}
	}
	text = WithholdQuoted(text)
	if text == err.Error() {
		return err
	}
	return withheldInput{err: err, text: text}
}

// WithholdQuoted is text with every quoted part of it, Go-quoted or
// backquoted (a regexp's or YAML's error quotes so), written as its size:
// for keploy's own words about something the user wrote (keploy.yaml's
// worker-mock rules, whose ids and patterns can hold a path or a value) or
// the application sent. Backquotes are read in pairs: text after an odd
// one out is kept (a stated limit, pkg/models/miss_log.go).
func WithholdQuoted(text string) string {
	withhold := func(q string) string {
		return "(quoted input withheld, " + strconv.Itoa(len(q)) + " bytes)"
	}
	return backquotedInput.ReplaceAllStringFunc(quotedInput.ReplaceAllStringFunc(text, withhold), withhold)
}

// ShapeQuotedURLs is text with every Go-quoted URL in it (a client's error
// quotes the request's: Post "http://..."), and every quoted path, written
// by its shape (urlLogText: scheme, host, path's shape, query's names). The
// rest of what text quotes stays: keploy's own words ("Content-Encoding").
func ShapeQuotedURLs(text string) string {
	return quotedInput.ReplaceAllStringFunc(text, func(q string) string {
		u, err := strconv.Unquote(q)
		if err != nil {
			return q
		}
		switch {
		case strings.Contains(u, "://"):
			return strconv.Quote(urlLogText(u))
		case strings.HasPrefix(u, "/"):
			if p, err := models.ParseRequestURL(u); err == nil {
				shape := URLPathShape(p)
				if keys := queryKeys(p.RawQuery); len(keys) > 0 {
					shape += "?{" + strings.Join(keys, ",") + "}"
				}
				return strconv.Quote(shape)
			}
			return "(quoted path withheld, " + strconv.Itoa(len(q)) + " bytes)"
		}
		return q
	})
}

type withheldInput struct {
	err  error
	text string
}

func (w withheldInput) Error() string { return w.text }
func (w withheldInput) Unwrap() error { return w.err }

// RedactedValue is what RedactCommandLine writes in place of a value.
const RedactedValue = "<redacted>"

// urlPassword is a URL's userinfo password: scheme://user:password@.
var urlPassword = regexp.MustCompile(`(://[^/\s:@]*:)[^@\s/]*@`)

// RedactCommandLine is an application's command line as a log line shows
// it: its program, flags and arguments, with every value that can be a
// credential written RedactedValue. The user writes credentials into the
// command keploy runs, and a log line reaches CI logs, keploy-logs.txt and
// support bundles. Words are split as a shell splits them (quotes group);
// RedactArgv says which values are redacted.
func RedactCommandLine(cmd string) string {
	return joinCommandLine(RedactArgv(splitCommandLine(cmd)))
}

// joinCommandLine is args as one command line, a word that holds white
// space or a quote single-quoted, so the line reads back as the same words.
func joinCommandLine(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

// RedactArgv is an argv as a log line shows it, a new slice, with redacted:
//   - the value of every flag named for a secret (--password x,
//     --api-key=x, --db-token: secretFlag), whole, white space or a leading
//     '-' and all;
//   - the value of every NAME=value word that is no flag (an environment
//     assignment: docker's -e TOKEN=..., env FOO=..., PowerShell's
//     $env:TOKEN='...' and $env:TOKEN = '...'), and of an assignment a flag
//     carries (--env=TOKEN=..., --build-arg=NPM_TOKEN=...), whole, white
//     space and all;
//   - a JSON object or array, as a word or a flag's value
//     (--spring.application.json='{"...password":"..."}', --config '{...}'),
//     whole;
//   - a header word that names a secret (Authorization: ..., Cookie: ...,
//     X-Api-Key: ...), for any program: its value;
//   - for curl or wget: a header's value after -H/--header, a password after
//     -u/--user/-U/--proxy-user (user:password, or attached: -uuser:pw), a
//     request body after -d/--data.../--json/-F/--post-data, a cookie after
//     -b;
//   - a client's password: a MySQL client's or a Mongo shell's attached
//     -p<password>, the word after redis-cli's -a, after sshpass's, a Mongo
//     shell's or docker login's -p, and every word after redis-cli's AUTH
//     (a client's rules apply from its program's word on, a port mapping
//     after -p left alone);
//   - a URL: written as its scheme, host and path's shape (userinfo, path
//     segments and query values left out); a JDBC URL by its inner URL's; a
//     host that is no list of host[:port] (SQL Server's
//     //db;user=sa;password=..., Connector/J's //(host=db,password=...)) by
//     its size;
//   - inside a word that is itself a command line (the argument of a
//     shell's -c or /C, or a word with white space that is none of the
//     above: a re-exec's argument), the same.
//
// Stated limits, forms no rule here reads:
//   - a path given without a scheme is kept whole (--callback=/reset/<tok>,
//     --config=/etc/app.yaml): a command line's paths are files, which a
//     user reads the line for;
//   - a flag whose name names no secret keeps its value
//     (--opaque-thing <tok>), unless the value is a URL, an assignment, a
//     header, JSON or a command line;
//   - a shell variable other than an environment one keeps its value
//     (PowerShell's $token = '...');
//   - PowerShell environment forms other than $env:NAME='v' and
//     $env:NAME="v": uneven spacing around = ($env:NAME ='v'),
//     Set-Item -Path env:NAME -Value 'v' and
//     [Environment]::SetEnvironmentVariable('NAME','v');
//   - a JSON value followed by a shell separator in a sh -c string
//     (--spring.application.json='{...}'; true), and a YAML flow mapping or
//     JSON5 value (--config "{apiKey: 'v'}").
func RedactArgv(args []string) []string {
	out := make([]string, len(args))
	var mysqlClient, redisClient, passOnP, httpClient, redisAuth bool
	for i, w := range args {
		prev := ""
		if i > 0 {
			prev = args[i-1]
		}
		// A client's rules apply from its program's word on (mysql -p...,
		// docker exec c mysql -p..., sh -c 'curl ...' is an argument of
		// its own, below): a word earlier, another program's flag, is not
		// read by them.
		switch programName(w) {
		case "mysql", "mysqldump", "mysqladmin", "mysqlimport", "mysqlshow", "mysqlcheck", "mysqlsh", "mariadb", "mariadb-dump", "mariadb-admin":
			mysqlClient = true
		case "redis-cli", "valkey-cli":
			redisClient = true
		case "sshpass", "mongo", "mongosh", "mongodump", "mongorestore", "mongoexport", "mongoimport":
			passOnP = true
		case "curl", "wget":
			httpClient = true
		case "login":
			// docker login -u u -p <password>, podman's, nerdctl's.
			switch programName(prev) {
			case "docker", "podman", "nerdctl":
				passOnP = true
			}
		}
		name, value, hasValue := strings.Cut(w, "=")
		switch {
		case prev == "=" && i > 1 && psEnvName.MatchString(args[i-2]):
			// PowerShell's $env:TOKEN = '...': the word after its "=".
			w = RedactedValue + shellTail(w)
		case redisAuth:
			// redis-cli ... AUTH [user] <password>: every word after it.
			w = RedactedValue
		case redisClient && strings.EqualFold(w, "AUTH"):
			redisAuth = true
		case httpClient && (prev == "-H" || prev == "--header"):
			w = headerLogText(w)
		case httpClient && (prev == "-u" || prev == "--user" || prev == "-U" || prev == "--proxy-user"):
			w = userLogText(w)
		case httpClient && httpBodyFlags[prev]:
			w = RedactedValue
		case ((redisClient && prev == "-a") || (passOnP && prev == "-p")) && !portMapping.MatchString(w):
			w = RedactedValue
		case secretFlagValue(prev, w):
			w = RedactedValue
		case secretHeader(w):
			w = headerLogText(w)
		case isJSONValue(w):
			w = RedactedValue
		case shellCommandFlag(prev):
			// A shell's command: its own words, by these rules.
			w = RedactCommandLine(w)
		case hasValue && assignedName.MatchString(name):
			w = name + "=" + RedactedValue + shellTail(value)
		case hasValue && strings.HasPrefix(name, "-") && (secretFlag(name) || httpClient && httpBodyFlags[name]):
			w = name + "=" + RedactedValue
		case hasValue && strings.HasPrefix(name, "-") && httpClient && name == "--header":
			w = name + "=" + headerLogText(value)
		case hasValue && strings.HasPrefix(name, "-"):
			w = name + "=" + flagValueLogText(value)
		case httpClient && len(w) > 2 && strings.HasPrefix(w, "-u") && !strings.HasPrefix(w, "--") && strings.Contains(w, ":"):
			w = "-u" + userLogText(w[2:])
		case (mysqlClient || passOnP) && len(w) > 2 && strings.HasPrefix(w, "-p") && !strings.HasPrefix(w, "--") && !portMapping.MatchString(w[2:]):
			w = "-p" + RedactedValue
		case strings.ContainsAny(w, commandSpace):
			// A word that is itself a command line (a re-exec's
			// argument): its own words, by these rules.
			w = RedactCommandLine(w)
		default:
			w = valueLogText(w)
		}
		out[i] = w
	}
	return out
}

// httpBodyFlags are curl's and wget's flags whose value is a request body or
// a cookie.
var httpBodyFlags = map[string]bool{
	"-d": true, "--data": true, "--data-raw": true, "--data-binary": true, "--data-urlencode": true, "--data-ascii": true,
	"--json": true, "-F": true, "--form": true, "--form-string": true, "-b": true, "--cookie": true,
	"--post-data": true, "--body-data": true,
}

// shellCommandFlag reports whether flag is the one after which a shell takes
// its command: sh -c, cmd /C or /K, PowerShell's -Command.
func shellCommandFlag(flag string) bool {
	switch flag {
	case "-c", "/C", "/c", "/K", "/k", "-Command", "-command":
		return true
	}
	return false
}

// secretFlagValue reports whether w is the value the flag before it takes,
// a flag named for a secret: whole, white space and all. A word that begins
// with '-' is a value only after a flag whose secret always takes one (a
// password, a token): after one that may take none (mongod --auth --port
// 27017), it is the next flag.
func secretFlagValue(flag, w string) bool {
	if !strings.HasPrefix(flag, "-") || strings.Contains(flag, "=") || !secretFlag(flag) {
		return false
	}
	if !strings.HasPrefix(w, "-") {
		return true
	}
	words := flagWords(strings.TrimLeft(flag, "-"))
	switch words[len(words)-1] {
	case "auth", "cookie":
		return false
	}
	return true
}

// headerWord is a header as a command line passes one: "Name: value".
var headerWord = regexp.MustCompile(`^\s*([A-Za-z0-9!#$%&'*+.^_|~-]+)\s*:`)

// secretHeader reports whether w is a header whose value is a secret:
// Authorization, Proxy-Authorization, Cookie, Set-Cookie, or a name
// secretFlag reads as one (X-Api-Key, X-Auth-Token). A URL is none.
func secretHeader(w string) bool {
	m := headerWord.FindStringSubmatch(w)
	if m == nil || strings.HasPrefix(w[len(m[0]):], "//") {
		return false
	}
	switch strings.ToLower(m[1]) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie":
		return true
	}
	return secretFlag(m[1])
}

// headerLogText is a header word with its value redacted: "Name: <redacted>",
// or <redacted> for a word that is not "Name: value".
func headerLogText(w string) string {
	if m := headerWord.FindStringSubmatch(w); m != nil {
		return m[1] + ": " + RedactedValue
	}
	return RedactedValue
}

// userLogText is user:password with the password redacted; a user alone
// (curl prompts for the password) as it is.
func userLogText(w string) string {
	if u, _, ok := strings.Cut(w, ":"); ok {
		return u + ":" + RedactedValue
	}
	return w
}

// shellTail is the shell's separators a word ends with (TOKEN=x; ./app,
// A=b&& c), kept after its redacted value.
func shellTail(value string) string {
	return value[len(strings.TrimRight(value, ";&|")):]
}

// commandSpace are the bytes splitCommandLine splits words at: a word that
// holds one is a command line of its own.
const commandSpace = " \t\n\r"

// flagValueLogText is a --flag=value's value as a log line shows it: a
// secret header's value redacted, JSON redacted, an assignment
// (--env=TOKEN=..., --build-arg=) with its value redacted whole (white space
// and all: --env="AUTH=Bearer <tok>"), a URL or a DSN by valueLogText, a
// command line by its words.
func flagValueLogText(value string) string {
	inner, rest, assigns := strings.Cut(value, "=")
	switch {
	case secretHeader(value):
		return headerLogText(value)
	case isJSONValue(value):
		return RedactedValue
	case assigns && assignedName.MatchString(inner):
		return inner + "=" + RedactedValue + shellTail(rest)
	case strings.Contains(value, "://") || goDSN.MatchString(value) || isJDBC(value):
		return valueLogText(value)
	case strings.ContainsAny(value, commandSpace):
		return RedactCommandLine(value)
	case assigns && inner != "":
		return inner + "=" + RedactedValue + shellTail(rest)
	}
	return value
}

// isJSONValue reports whether w is a JSON object or array: a document a
// flag passes whole (Spring's --spring.application.json, a --config),
// whose values are the app's settings, a password among them. A bracketed
// address ([::1]:8080) or a list ([a,b]) is not JSON.
func isJSONValue(w string) bool {
	t := strings.TrimSpace(w)
	if len(t) < 2 {
		return false
	}
	if first, last := t[0], t[len(t)-1]; (first != '{' || last != '}') && (first != '[' || last != ']') {
		return false
	}
	return json.Valid([]byte(t))
}

// valueLogText is a word as a log line shows it: a Go MySQL DSN with its
// password redacted, a JDBC URL without a URL inside (Oracle's
// jdbc:oracle:thin:user/password@host) by its kind alone, a URL by
// urlLogText, any other word as it is.
func valueLogText(w string) string {
	if goDSN.MatchString(w) {
		return goDSN.ReplaceAllString(w, "${1}:"+RedactedValue+"@${2}")
	}
	if isJDBC(w) && !strings.Contains(w, "://") {
		kind := strings.SplitN(w, ":", 3)
		return "jdbc:" + kind[1] + ":" + RedactedValue
	}
	return urlLogText(w)
}

// isJDBC reports whether w is a JDBC URL: jdbc:<subprotocol>:....
func isJDBC(w string) bool {
	return len(w) > 5 && strings.EqualFold(w[:5], "jdbc:") && strings.Count(w, ":") >= 2
}

// goDSN is the userinfo of a Go MySQL driver's DSN: user:password@ before a
// protocol and its address (tcp(...), unix(...)) or the database (/db); the
// password may hold '@' (the last one before the address ends it).
var goDSN = regexp.MustCompile(`^([^:@\s/]+):\S*@((?:tcp|unix|udp)\(|\(|/)`)

// assignedName is the name of a NAME=value word: an environment variable's
// (PowerShell's $env:NAME too) or a setting's (a.b, a.root-password), not a
// URL's or a flag's.
var assignedName = regexp.MustCompile(`^(?i:\$env:)?[A-Za-z_][A-Za-z0-9_.-]*$`)

// psEnvName is a PowerShell environment variable's name, $env:NAME, a word
// of its own before a spaced " = ".
var psEnvName = regexp.MustCompile(`^(?i:\$env:)[A-Za-z_][A-Za-z0-9_.-]*$`)

// portMapping is what follows a -p that publishes ports (docker run
// -p3306:3306): digits and colons.
var portMapping = regexp.MustCompile(`^[0-9]+(:[0-9]+)*(/(tcp|udp))?$`)

// programName is w's base name in lower case, without a Windows .exe: the
// program a word names, if it names one.
func programName(w string) string {
	b := strings.ToLower(w[strings.LastIndexAny(w, `/\`)+1:])
	return strings.TrimSuffix(b, ".exe")
}

// urlLogText is w, or, when w is a URL, its scheme, host and path's shape
// (URLPathShape) and its query's names: a URL in a command line carries a
// token in its userinfo, a path segment or a query value. A URL inside a
// scheme (jdbc:mysql://db/shop?password=...) is written by its inner URL's
// text; one with no host by its text without userinfo password, query
// values or fragment. A host is written only when it is a list of
// host[:port] (hostList); any other by its size: url.Parse takes ; = ( ) ,
// in a host, so SQL Server's jdbc:sqlserver://db;user=sa;password=... and
// Connector/J's jdbc:mysql://(host=db,password=...)/shop parse with the
// password in the host.
func urlLogText(w string) string {
	if !strings.Contains(w, "://") {
		return w
	}
	u, err := url.Parse(w)
	if err != nil {
		// Its parser's error would quote it: named by its size alone.
		return "(unparsable URL, " + strconv.Itoa(len(w)) + " bytes)"
	}
	if u.Host == "" && u.Opaque != "" && strings.Contains(u.Opaque, "://") {
		inner := u.Opaque
		if u.RawQuery != "" {
			inner += "?" + u.RawQuery
		}
		return u.Scheme + ":" + urlLogText(inner)
	}
	if u.Host == "" {
		bare, _, _ := strings.Cut(w, "#")
		bare, _, _ = strings.Cut(bare, "?")
		out := urlPassword.ReplaceAllString(bare, "${1}"+RedactedValue+"@")
		if keys := queryKeys(u.RawQuery); len(keys) > 0 {
			out += "?{" + strings.Join(keys, ",") + "}"
		}
		return out
	}
	host := u.Host
	if !isHostList(host) {
		host = "(host withheld, " + strconv.Itoa(len(host)) + " bytes)"
	}
	out := u.Scheme + "://" + host + URLPathShape(u)
	if keys := queryKeys(u.RawQuery); len(keys) > 0 {
		out += "?{" + strings.Join(keys, ",") + "}"
	}
	return out
}

// hostEntry is one host[:port] of a URL's host: a DNS name or an IPv4
// address (letters, digits, '.', '-', '_'), or a bracketed IPv6 address
// with its zone, each with an optional port; a name may be empty before a
// port (http://:8080).
var hostEntry = regexp.MustCompile(`^(?:[A-Za-z0-9._-]*|\[[0-9A-Fa-f:.]+(?:%[A-Za-z0-9._~-]+)?\])(?::[0-9]*)?$`)

// isHostList reports whether host is a list of host[:port] entries,
// comma-separated (mongodb://m1:27017,m2:27017), each a hostEntry.
func isHostList(host string) bool {
	for _, h := range strings.Split(host, ",") {
		if !hostEntry.MatchString(h) {
			return false
		}
	}
	return true
}

// secretFlag reports whether the flag name names a secret: its last word
// is one (--password, --db-pass, --auth-token, --secret, --cookie), or a key
// of a kind that is one (--api-key, --access-key, --secret-key). A flag a
// secret's word only begins (--passThroughPorts) or holds inside a word
// (--bypass) names none.
func secretFlag(name string) bool {
	name = strings.TrimLeft(name, "-")
	if strings.HasPrefix(name, "D") && len(name) > 1 {
		// A Java system property: -Dspring.datasource.password=, -Dpassword=.
		name = name[1:]
	}
	words := flagWords(name)
	if len(words) == 0 {
		return false
	}
	last := words[len(words)-1]
	switch last {
	case "password", "passwd", "pass", "pwd", "secret", "token", "apikey", "credential", "credentials", "auth",
		"authorization", "cookie", "passphrase", "dsn", "accesskey", "secretkey", "privatekey", "bearer", "jwt", "sessionid":
		return true
	case "key":
		if len(words) > 1 {
			switch words[len(words)-2] {
			case "api", "access", "secret", "private", "auth", "client", "master", "encryption", "signing":
				return true
			}
		}
	}
	return false
}

// flagWords are name's words in lower case: split at '-', '_', '.' and a
// lower-to-upper case change (apiKey: api, key).
func flagWords(name string) []string {
	var words []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			words = append(words, strings.ToLower(b.String()))
			b.Reset()
		}
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '-' || c == '_' || c == '.':
			flush()
		case c >= 'A' && c <= 'Z' && i > 0 && name[i-1] >= 'a' && name[i-1] <= 'z':
			flush()
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return words
}

// splitCommandLine is cmd's words as a shell splits them: white space
// separates them, single and double quotes group (and are dropped), and a
// backslash outside single quotes before a quote, a backslash or a space
// takes that character as it is.
func splitCommandLine(cmd string) []string {
	var words []string
	var b strings.Builder
	in, quote, escaped := false, byte(0), false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case escaped:
			b.WriteByte(c)
			escaped, in = false, true
		case c == '\\' && quote != '\'' && i+1 < len(cmd) && strings.IndexByte("\"'\\ \t", cmd[i+1]) >= 0:
			// An escape only before a quote, a backslash or a space: a
			// Windows path's backslashes (C:\app\srv.exe) stay.
			escaped, in = true, true
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				b.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if in {
				words = append(words, b.String())
				b.Reset()
				in = false
			}
		default:
			b.WriteByte(c)
			in = true
		}
	}
	if in {
		words = append(words, b.String())
	}
	return words
}

// RedactCmdArgs is the argv of a command keploy runs (exec.Cmd.Args) as one
// log line: RedactArgv's words, quoted where they hold white space. A shell's
// -c argument is redacted as the command line it is.
func RedactCmdArgs(args []string) string {
	return joinCommandLine(RedactArgv(args))
}
