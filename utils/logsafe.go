package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// URLLogMeta is what a log line says about a request to rawURL: what
// GetReqShape says of a request (its method, its host, its path's shape and
// the names of its query parameters, no path tag: keploy's CLI names a test
// case's URL so, and no console or report line prints a tag of it), and its
// scheme. Never a path segment, a
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
	meta := GetReqShape(&http.Request{Method: method, Host: u.Host, URL: u})
	if u.Scheme != "" {
		meta["scheme"] = u.Scheme
	}
	return meta
}

// PathTagField is the path_tag field of a log line that names an HTTP call
// by its shape: tag (models.PathTag), which the console and the report print
// beside the call whole. No field for "" (a call of another protocol).
func PathTagField(tag string) zap.Field {
	if tag == "" {
		return zap.Skip()
	}
	return zap.String("path_tag", tag)
}

// HostTagField is a host tag (models.HostTag) as a line of keploy's CLI
// names the tag the agent made for a destination it logs by its shape: a
// "host_tag" field, which the console, the report and the misses file print
// beside the destination whole. No field for "".
func HostTagField(tag string) zap.Field {
	if tag == "" {
		return zap.Skip()
	}
	return zap.String("host_tag", tag)
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
// No path tag: the report keeps the error as it came, with no tag beside its
// URL, so a tag here is one no reader could find; the line that logs the
// error names its test, which the report names it under.
func ShapeQuotedURLs(text string) string {
	return quotedInput.ReplaceAllStringFunc(text, func(q string) string {
		u, err := strconv.Unquote(q)
		if err != nil {
			return q
		}
		switch {
		case strings.Contains(u, "://"):
			return strconv.Quote(urlLogTextOf(u, true))
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
	return JoinCommandLine(RedactArgv(splitCommandLine(cmd)))
}

// RedactProcessArgv is the command line of a process keploy saw make a
// call (an application's process, not the command the user gave keploy),
// argv its words as the kernel keeps them (/proc/<pid>/cmdline,
// kern.procargs2), as a log line shows it: by its shape, but for its
// program, its flags' names and its numbers. It takes the words, never a
// line joined from them: a word may hold white space or a quote (a path
// "Bob's app/bin/python3", an argument "my file.txt", a process's own
// title), so a joined line splits back into other words. A process's words
// are the application's traffic, the hosts, paths, queries and users it
// calls with, and they come in more forms than any rule can tell from a
// file's name (a URL with no scheme, user:password@host, ssh's -L
// bind:port:host:port, bash's /dev/tcp/host/port, a container's command
// after its image). So no word is kept because no host was found in it:
// only the words below are.
//
// Its words are RedactArgv's, a secret's value RedactedValue, with no tag.
// Every other word, and every flag's value, is written by its shape (each
// run of letters and digits, and of bytes past ASCII, {}; every other
// character, and RedactedValue, kept), then the tag of the whole word under
// keys (LogKeySession.LogKey; the process's key for a nil keys), so the
// session's process lines about one word match up: "dep.example:18999/v1"
// is "{}.{}:{}/{} #3fa2c1d0e9b8", and an IP address is written so too. The
// session's other lines tag a host by its name (models.HostTag), so a tag
// here matches only another process line's. These carry no traffic, and
// are kept as they are:
//   - the program: argv[0]'s base name (python3, java), as the line's comm
//     names it, when argv[0] is a program's path, with no white space or
//     quote; an empty argv[0] is an empty word. Any other argv[0] (a
//     process's own title, "ssh: ...@db:22 [mux]", or a path a quote or a
//     space is in) is written by its shape and tag, as an argument is. A
//     process that names itself with one word (exec -a, setproctitle)
//     shows that word's base name: a stated limit;
//   - a flag's name (flagName): --name, a name up to and with its '='
//     (--name=, -name=), and the -X of any other word that begins with '-'.
//     What follows -X (-hdb, -uroot, -p8080:80) may be its value, and is
//     written by its shape and tag, as a value after '=' is. A program that
//     reads a host in a flag's name shows it there: Hadoop's
//     -Dfs.azure.account.key.<account>.blob.core.windows.net=<key> keeps
//     its account's host (its key is written by its shape);
//   - a number of one to five digits (a port, a count), as a log line
//     writes a port; a longer one may be an account's or a card's;
//   - a word with no letter or digit (-, &&), which its shape writes whole.
//
// So ["python3", "client.py", "http://dep.example:18999"] is
// "python3 '{}.{} #…' '{}://{}.{}:{} #…'". The whole command line goes
// only to keploy mock's local receipt (models.Origin.Cmdline). The command
// the user gave keploy is named at INFO by its program and how many words
// follow it (AppCommand), and whole but for its secrets
// (RedactCommandLine) at DEBUG alone (LogWholeCommand).
func RedactProcessArgv(keys *models.LogKeySession, argv []string) string {
	words := RedactArgv(argv)
	for i, w := range words {
		switch name := flagName.FindString(w); {
		case i == 0:
			words[i] = programWord(keys, argv[0], w)
		case name != "":
			words[i] = name + shapedWord(keys, w[len(name):])
		case !shortNumber.MatchString(w):
			words[i] = shapedWord(keys, w)
		}
	}
	return JoinCommandLine(words)
}

// programWord is argv[0] as a process's logged command line names its
// program, raw the process's word and w RedactArgv's: w's base name when
// raw is a program's path (programPath), else w by its shape and tag
// (shapedWord): a title a process gave itself, or a path whose base name a
// reader could not tell from the words after it. It reads raw, as
// RedactArgv reads a word with white space as a command line and writes it
// again: " db:5432" and "'db' " come out of it as db:5432 and db. An empty
// argv[0] is an empty word, which JoinCommandLine writes as an empty quoted
// one.
func programWord(keys *models.LogKeySession, raw, w string) string {
	if !programPath(raw) {
		return shapedWord(keys, w)
	}
	return w[strings.LastIndexAny(w, `/\`)+1:]
}

// programPath reports whether w can be a program's path: no white space
// (unicode.IsSpace: a no-break space, \v and U+3000 too) and no quote (',
// " or `).
func programPath(w string) bool {
	return !strings.ContainsFunc(w, unicode.IsSpace) && !strings.ContainsAny(w, "'\"`")
}

// programBaseName is a program's base name as AppCommand names one: letters,
// digits, '.', '_', '+' and '-', not first (python3, node, app.exe, c++,
// x86_64-linux-gnu-gcc).
var programBaseName = regexp.MustCompile(`^[A-Za-z0-9._+][A-Za-z0-9._+-]*$`)

// StartingApplication is what keploy logs at INFO as it starts the user's
// command (ExecuteCommand, a compose run in-process), beside its program
// (zap.Inline of an AppCommand); startingApplicationPTY under a terminal of
// its own, and startingApplicationWindows on Windows. CI lanes read these words as the
// mark that keploy started the app (grep -E 'Starting Application ?:'), so
// they stay as they are.
const (
	StartingApplication        = "Starting Application :"
	startingApplicationPTY     = "Starting Application:"
	startingApplicationWindows = "Starting Application (Windows):"
)

// WholeCommandMessage is the DEBUG line that names a command keploy runs
// whole but for its secrets (LogWholeCommand).
const WholeCommandMessage = "the command keploy runs, whole but for its secrets"

// LogWholeCmdArgs is LogWholeCommand of args, the argv keploy runs
// (exec.Cmd.Args: sh -c and the user's command), by RedactCmdArgs.
func LogWholeCmdArgs(logger *zap.Logger, key string, args []string) {
	if ce := logger.Check(zap.DebugLevel, WholeCommandMessage); ce != nil {
		ce.Write(zap.String(key, RedactCmdArgs(args)))
	}
}

var (
	// flagName is the name a flag's word begins with, the program's own
	// word for an option: --name (a long flag's value is never in its word
	// but after '='), a name up to and with its '=' (--name=, Go's
	// -name=, java's -Dname=, ssh's -oName=), or else the -X of a word
	// that begins with '-' and a letter or a digit: a short flag, which
	// its value may follow in the same word.
	flagName = regexp.MustCompile(`^(?:--?[A-Za-z0-9][A-Za-z0-9._-]*=|--[A-Za-z0-9][A-Za-z0-9._-]*$|-[A-Za-z0-9])`)
	// shortNumber is a number of one to five digits, a port's size.
	shortNumber = regexp.MustCompile(`^[0-9]{1,5}$`)
)

// shapedWord is w, a word or a flag's value, by its shape, each run of
// ASCII letters and digits, and of bytes past ASCII, {}, every other byte
// and each RedactedValue kept, then its tag under keys; w as it is when its
// shape is w (no letter or digit), as nothing is left out to tag.
func shapedWord(keys *models.LogKeySession, w string) string {
	var b strings.Builder
	for i, part := range strings.Split(w, RedactedValue) {
		if i > 0 {
			b.WriteString(RedactedValue)
		}
		run := false
		for j := 0; j < len(part); j++ {
			c := part[j]
			alnum := c >= 0x80 || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
			switch {
			case !alnum:
				b.WriteByte(c)
			case !run:
				b.WriteString("{}")
			}
			run = alnum
		}
	}
	if b.String() == w {
		return w
	}
	return b.String() + " " + keys.LogKey(w)
}

// JoinCommandLine is args as one command line, a word that holds white
// space (any unicode.IsSpace: \r, \v, a no-break space too) or a quote
// (', " or `), or none, single-quoted as a shell reads it, so each word
// reads as one: a path "/opt/Bob's app/bin/python3", an argument
// "my file.txt" ('my file.txt'), an empty argument.
func JoinCommandLine(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || !programPath(a) {
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
//     -u/--user/-U/--proxy-user (user:password; attached, -uuser:pw and
//     -Uuser:pw; or after an '=', --user=user:pw and --proxy-user=user:pw,
//     as curl 8.16 and later take them), a request body after
//     -d/--data.../--json/-F/--post-data, a cookie after -b;
//   - a client's password: a MySQL client's or a Mongo shell's attached
//     -p<password>, the word after redis-cli's -a, after sshpass's, a Mongo
//     shell's or docker login's -p, and every word after redis-cli's AUTH;
//     SQL Server's (sqlcmd, bcp, osql) -P <password> and -P<password>;
//     lftp's -u user,<password> (--user, attached too); Samba's (smbclient,
//     rpcclient, smbcacls, smbcquotas, smbget, smbtree) -U user%<password>
//     (--user, attached too) and the password smbclient takes after its
//     //server/share and its options (the first word that is no option and
//     no option's value); an Oracle client's (sqlplus, sqlldr, SQLcl's sql,
//     rman, expdp, impdp, exp, imp, dgmgrl) logon, user/<password>[@db],
//     with an '@' anywhere and without one right after the program, a flag
//     or rman's target, catalog or auxiliary (oracleClientLogonText), a
//     quote a shell kept because it was escaped around it (expdp
//     \"sys/pw@orcl as sysdba\"); git's -c name=value (a -c, --config or
//     --config= value that reads as a setting, gitSettingWord, after any
//     program: another program's of that shape is redacted too), its
//     value (a secret header's from the header's name on:
//     http.extraHeader=Authorization: <redacted>) and a userinfo in its
//     name; npm's
//     //registry/:_authToken=, and its value after it as a word of its
//     own; a Windows program's /Name:value whose name is a secret's
//     (/TargetPassword:, sqlpackage's /tp: and /sp:) and /p:Name=value
//     (msbuild's, sqlpackage's), as a flag's value otherwise. A
//     client's rules apply from its program's word on, a port mapping after
//     -p left alone; inside an Oracle client's or lftp's words, a word that
//     is a command line (sqlplus "sys/pw@db as sysdba", lftp -e 'open -u
//     user,pw host') is read by that client's rules too;
//   - a userinfo with no scheme, as a word or a flag's value
//     (user:password@host/path, curl's -x user:password@proxy:3128,
//     --db=app:password@db:5432/shop): its password (userinfoLogText);
//   - Oracle's logon, as a word or a flag's value, for any program, when
//     what follows its '@' names a database's address (oracleConnect:
//     //host..., host:port..., host/service..., a connect descriptor):
//     scott/<password>@db:1521/XE (oracleLogonLogText);
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
//     header, JSON or a command line, or the flag is one of the clients'
//     above: so do other programs' short password flags (ldapsearch's -w,
//     cqlsh's -p, mosquitto_pub's -P, zip's -P, openssl's -passin
//     pass:<pw>), and a password a program takes as a plain argument
//     (htpasswd -b);
//   - a shell variable other than an environment one keeps its value
//     (PowerShell's $token = '...');
//   - PowerShell environment forms other than $env:NAME='v' and
//     $env:NAME="v": uneven spacing around = ($env:NAME ='v'),
//     Set-Item -Path env:NAME -Value 'v' and
//     [Environment]::SetEnvironmentVariable('NAME','v');
//   - a JSON value followed by a shell separator in a sh -c string
//     (--spring.application.json='{...}'; true), and a YAML flow mapping or
//     JSON5 value (--config "{apiKey: 'v'}");
//   - Oracle's logon outside an Oracle client's own words when what follows
//     its '@' names no address: an alias (app scott/<pw>@orcl) or a host
//     with neither port nor service (scott/<pw>@db.example), which read as
//     a repository's owner/name@ref (actions/checkout@v4); and its password
//     when it holds a '/' unquoted.
//
// Forms no tool writes, e.g. curl's -x alice@corp:<pw>@proxy (curl refuses
// it: a proxy user's '@' is written %40), are not read; a real one is a bug
// to fix.
//
// It writes some words that hold no secret as if they did, so as to
// redact every one that does: a userinfo's or a logon's password runs to
// the word's last '@' (a host list's first hosts, --brokers=a:pw@k1,b:pw@k2,
// and a path after a port, host:8080/users/@me, are cut with it), and
// Oracle's logon form takes a ref or a path such as owner/repo@feature/x or
// assets/icon@3x/a.png; any program's -c or --config value that reads as a
// git setting is redacted as git's (python -c 'sys.argv=["app", "--port",
// "8080"]' is python -c sys.argv=<redacted>, spark-submit -c
// 'spark.driver.extraJavaOptions=-Dx=1 -Dy=2' loses -Dy=2), as git's word
// cannot be told in every form it takes. A name with white space is git's
// only in a section of git's that takes a subsection
// (gitSubsectionSections), so a command whose first word
// holds a dot (java.exe -jar app.jar --spring.profiles.active=dev) keeps its
// words.
func RedactArgv(args []string) []string {
	out := make([]string, len(args))
	var mysqlClient, redisClient, passOnP, httpClient, redisAuth bool
	var sqlServerClient, lftpClient, smbClient, smbclientArgs, oracleClient, sqlpackageClient bool
	// smbPassword: past smbclient's //server/share, its first word that is
	// no option and no option's value is its password; smbOptionValue: the
	// word is the value of the option before it.
	var smbPassword, smbOptionValue bool
	// client is the program whose rules a command line inside a word is
	// read by too (an Oracle client's, lftp's).
	client := ""
	for i, w := range args {
		prev := ""
		if i > 0 {
			prev = args[i-1]
		}
		// git's setting (git -c name=value, clone's -c and --config): a
		// -c or --config value that reads as one (gitSettingWord), after
		// any program's word. git's word takes too many forms to be told
		// (alpine/git:2.43.0, alpine/git@sha256:..., cd /src&&git, (git,
		// "$GIT", hub, a wrapper), and read as a shell's command, a
		// header's value is split at its space and its token kept. A
		// shell's own command is none (mysql -uroot -p..., anything with
		// white space before its first '='), so it is still read as one:
		// after git pull &&, after alpine/git sh, and after alpine/git
		// when its entrypoint is a shell. Another program's setting-shaped
		// value is redacted as git's is (python -c 'sys.argv=[...]'
		// becomes sys.argv=<redacted>): text a log line or a report loses,
		// never a secret it keeps.
		gitSetting := (prev == "-c" || prev == "--config") && gitSettingWord(w)
		// A client's rules apply from its program's word on (mysql -p...,
		// docker exec c mysql -p..., sh -c 'curl ...' is an argument of
		// its own, below): a word earlier, another program's flag, is not
		// read by them.
		switch program := programName(w); program {
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
		case "sqlcmd", "bcp", "osql":
			sqlServerClient = true
		case "lftp":
			lftpClient, client = true, program
		case "smbclient":
			smbClient, smbclientArgs = true, true
		case "rpcclient", "smbcacls", "smbcquotas", "smbget", "smbtree":
			smbClient = true
		case "sqlpackage":
			sqlpackageClient = true
		default:
			if oracleClients[program] {
				oracleClient, client = true, program
			}
		}
		oracleText, oracleLogon := "", false
		if oracleClient {
			oracleText, oracleLogon = oracleClientLogonText(prev, w)
		}
		smbclientPassword := false
		if smbPassword {
			switch {
			case smbOptionValue:
				smbOptionValue = false
			case strings.HasPrefix(w, "-"):
				smbOptionValue = smbclientOptionTakesValue(w)
			default:
				smbclientPassword, smbPassword = true, false
			}
		}
		if smbclientArgs && smbService(w) {
			smbPassword, smbOptionValue = true, false
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
		case sqlServerClient && prev == "-P":
			w = RedactedValue
		case lftpClient && (prev == "-u" || prev == "--user"):
			w = userPassLogText(w, ",")
		case smbClient && (prev == "-U" || prev == "--user"):
			w = userPassLogText(w, "%")
		case smbclientPassword:
			// smbclient //server/share [options] <password>.
			w = RedactedValue
		case npmScopedKey(prev) && !strings.HasPrefix(w, "-"):
			// npm config set //registry/:_authToken <token>.
			w = RedactedValue
		case secretFlagValue(prev, w):
			w = RedactedValue
		case oracleLogon:
			w = oracleText
		case secretHeader(w):
			w = headerLogText(w)
		case isJSONValue(w):
			w = RedactedValue
		case gitSetting:
			// git -c name=value: a setting git takes, not a shell's command.
			w = gitConfigLogText(w)
		case hasValue && name == "--config" && gitSettingWord(value):
			// git clone --config=name=value, after any program's word.
			w = name + "=" + gitConfigLogText(value)
		case shellCommandFlag(prev):
			// A shell's command: its own words, by these rules.
			w = redactCommandLineOf(client, w)
		// A short flag's attached password, before a '=' in it is read as
		// a flag's value (-pS3=CR3T).
		case httpClient && len(w) > 2 && strings.HasPrefix(w, "-u") && !strings.HasPrefix(w, "--") && strings.Contains(w, ":"):
			w = "-u" + userLogText(w[2:])
		case httpClient && len(w) > 2 && strings.HasPrefix(w, "-U") && !strings.HasPrefix(w, "--") && strings.Contains(w, ":"):
			// curl's proxy user, attached: -Uuser:pw.
			w = "-U" + userLogText(w[2:])
		case (mysqlClient || passOnP) && len(w) > 2 && strings.HasPrefix(w, "-p") && !strings.HasPrefix(w, "--") && !portMapping.MatchString(w[2:]):
			w = "-p" + RedactedValue
		case sqlServerClient && len(w) > 2 && strings.HasPrefix(w, "-P") && !strings.HasPrefix(w, "--"):
			w = "-P" + RedactedValue
		case lftpClient && len(w) > 2 && strings.HasPrefix(w, "-u") && !strings.HasPrefix(w, "--"):
			w = "-u" + userPassLogText(w[2:], ",")
		case smbClient && len(w) > 2 && strings.HasPrefix(w, "-U") && !strings.HasPrefix(w, "--"):
			w = "-U" + userPassLogText(w[2:], "%")
		case hasValue && lftpClient && name == "--user":
			w = name + "=" + userPassLogText(value, ",")
		case hasValue && smbClient && name == "--user":
			w = name + "=" + userPassLogText(value, "%")
		case hasValue && httpClient && (name == "--user" || name == "--proxy-user"):
			// curl's --user=user:pw and --proxy-user=user:pw (curl 8.16 and
			// later read a long option's value after its '=').
			w = name + "=" + userLogText(value)
		case windowsFlag.MatchString(w):
			// A Windows program's /Name:value (sqlpackage, msbuild).
			w = windowsFlagLogText(w, sqlpackageClient)
		case hasValue && npmScopedKey(name):
			// npm's //registry/:_authToken=<token>.
			w = name + "=" + RedactedValue
		case hasValue && assignedName.MatchString(name):
			w = name + "=" + RedactedValue + shellTail(value)
		case hasValue && strings.HasPrefix(name, "-") && (secretFlag(name) || httpClient && httpBodyFlags[name]):
			w = name + "=" + RedactedValue
		case hasValue && strings.HasPrefix(name, "-") && httpClient && name == "--header":
			w = name + "=" + headerLogText(value)
		case hasValue && strings.HasPrefix(name, "-"):
			w = name + "=" + flagValueLogText(value)
		case strings.ContainsAny(w, commandSpace):
			// A word that is itself a command line (a re-exec's
			// argument): its own words, by these rules.
			w = redactCommandLineOf(client, w)
		default:
			w = valueLogText(w)
		}
		out[i] = w
	}
	return out
}

// smbclientOptionTakesValue reports whether w, an option of smbclient's, is
// one whose value is the next word: -U user, --workgroup W. One that holds
// its value (-Ualice%pw, --user=alice) or takes none (-N, -g) is not.
func smbclientOptionTakesValue(w string) bool {
	switch w {
	case "-U", "-W", "-I", "-p", "-n", "-m", "-s", "-l", "-d", "-O", "-A", "-c", "-D", "-T", "-b", "-t", "-R", "-i", "-L", "-M", "-S",
		"--user", "--workgroup", "--ip-address", "--port", "--netbiosname", "--max-protocol", "--configfile", "--log-basename",
		"--debuglevel", "--socket-options", "--authentication-file", "--command", "--directory", "--tar", "--send-buffer",
		"--timeout", "--name-resolve", "--scope", "--list", "--message", "--signing", "--realm", "--option",
		"--client-protection", "--use-kerberos", "--use-krb5-ccache":
		return true
	}
	return false
}

// gitSettingName is a git setting's name as -c takes it (git splits -c's
// word at its first '='): a section of letters, digits and '-' before the
// name's first '.', a variable that begins with a letter after its last
// '.', and between them an optional subsection that may hold any byte but
// a newline (a URL, white space and all: http.https://host/my
// repo.extraHeader; a remote's name), as git takes it.
var gitSettingName = regexp.MustCompile(`^[A-Za-z0-9-]+\.(?:[^\n]*\.)?[A-Za-z][A-Za-z0-9-]*$`)

// gitSubsectionSections are git's sections whose variables take a
// subsection (section.<subsection>.key), the only ones whose name is read with
// white space in its subsection: git's own list, `git help --config` of git
// 2.55.0 (git-config(1), Documentation/config/*.adoc), whose variables of
// the form section.<name>.key or section.*.key (alias.*.command: an alias's
// name may hold white space, and a '!' alias is a shell command) are under
// these sections, and git-lfs's lfs (git-lfs-config(5), lfs.<url>.access).
// A refresh reads both placeholders, <name> and *. Section names are
// compared in lower case, as git compares them.
var gitSubsectionSections = map[string]bool{
	"alias": true, "bitmappseudomerge": true, "branch": true, "browser": true, "bundle": true, "credential": true, "diff": true,
	"difftool": true, "filter": true, "gc": true, "gpg": true, "guitool": true, "hook": true, "http": true,
	"includeif": true, "maintenance": true, "man": true, "merge": true, "mergetool": true, "notes": true,
	"protocol": true, "remote": true, "sendemail": true, "sideband": true, "submodule": true, "trailer": true,
	"uploadpackfilter": true, "url": true, "lfs": true,
}

// gitSettingWord reports whether w reads as git -c's name[=value]. A shell's
// command (mysql -uroot -p...) is none: its first word holds no '.' after
// a section's letters, or its last '.' is followed by more than a
// variable's name, and sh would run a program named for it. A name that
// holds white space is read so only with an '=' after it and in one of
// gitSubsectionSections: a shell's -c command and keploy's own -c command (java.exe
// -jar app.jar --spring.profiles.active=dev) reach the redactor in the form
// git's -c value does, one word whose white space its source quoted, so
// neither the quotes nor the word tell them apart; git's section does, as
// java, node, python3 and mvnw are none of git's sections.
func gitSettingWord(w string) bool {
	name, _, assigns := strings.Cut(w, "=")
	if !gitSettingName.MatchString(name) {
		return false
	}
	if strings.ContainsAny(name, commandSpace) {
		section, _, _ := strings.Cut(name, ".")
		return assigns && gitSubsectionSections[strings.ToLower(section)]
	}
	return true
}

// gitConfigLogText is git -c's name=value with its value redacted, as an
// assignment's is, a secret header's from the header's name on
// (http.extraHeader=Authorization: <redacted>), and its name as a URL is
// written (url.<base>.insteadOf names a URL, which may hold a userinfo). A
// name that holds white space (a subsection's URL with a space; a shell's
// command of that shape) is written word by word, as a shell's command
// line is (RedactCommandLine), so it keeps no more than that reading.
func gitConfigLogText(w string) string {
	name, value, ok := strings.Cut(w, "=")
	if !ok {
		return valueLogText(w)
	}
	nameText := valueLogText(name)
	if strings.ContainsAny(name, commandSpace) {
		nameText = RedactCommandLine(name)
	}
	if secretHeader(value) {
		return nameText + "=" + headerLogText(value)
	}
	return nameText + "=" + RedactedValue
}

// windowsFlag is a Windows program's flag that holds its value after a ':'
// (/Name:value): sqlpackage's /TargetPassword:<pw>, msbuild's and
// sqlpackage's /p:Name=value.
var windowsFlag = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9]*:`)

// windowsFlagLogText is w, a windowsFlag, as a log line shows it: the value
// of one named for a secret (/TargetPassword:<redacted>, sqlpackage's
// /tp: and /sp:) redacted, a property's (/p:Password=<redacted>) when its
// name is one, and any other value as a flag's value is written
// (flagValueLogText: /tcs:Server=<redacted>).
func windowsFlagLogText(w string, sqlpackage bool) string {
	i := strings.IndexByte(w, ':')
	name, value := w[1:i], w[i+1:]
	switch lower := strings.ToLower(name); {
	case secretFlag(name), sqlpackage && (lower == "tp" || lower == "sp"):
		return w[:i+1] + RedactedValue
	case lower == "p" || lower == "property":
		if prop, v, ok := strings.Cut(value, "="); ok {
			if secretFlag(prop) {
				return w[:i+1] + prop + "=" + RedactedValue
			}
			return w[:i+1] + prop + "=" + flagValueLogText(v)
		}
	}
	return w[:i+1] + flagValueLogText(value)
}

// npmScopedKey reports whether name is npm's key of a registry's secret,
// //registry.example/:_authToken (and :_auth, :_password), as npm config
// set and .npmrc take it.
func npmScopedKey(name string) bool {
	i := strings.LastIndexByte(name, ':')
	return strings.HasPrefix(name, "//") && i > 0 && secretFlag(name[i+1:])
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

// userLogText is curl's user:password with the password redacted; a user
// alone (curl prompts for the password) as it is.
func userLogText(w string) string {
	return userPassLogText(w, ":")
}

// userPassLogText is a user and its password, sep between them (curl's
// user:pw, lftp's user,pw, Samba's [domain\]user%pw), with the password
// redacted, from sep's first place on; a user alone (the client prompts for
// the password) as it is.
func userPassLogText(w, sep string) string {
	if u, _, ok := strings.Cut(w, sep); ok {
		return u + sep + RedactedValue
	}
	return w
}

// smbService reports whether w is a Samba service, the //server/share
// (\\server\share) smbclient takes its password after.
func smbService(w string) bool {
	return strings.HasPrefix(w, "//") || strings.HasPrefix(w, `\\`)
}

// redactCommandLineOf is cmd, a command line inside a word of client's
// words, as a log line shows it: by client's rules too (sqlplus
// "sys/pw@db as sysdba", lftp -e 'open -u user,pw host'), and of no client
// by RedactCommandLine's.
func redactCommandLineOf(client, cmd string) string {
	if client == "" {
		return RedactCommandLine(cmd)
	}
	return JoinCommandLine(RedactArgv(append([]string{client}, splitCommandLine(cmd)...))[1:])
}

// oracleClients are the programs that take Oracle's logon,
// user/password[@db], as an argument: SQL*Plus, SQL*Loader, SQLcl (sql),
// RMAN, Data Pump and its predecessors, Data Guard's broker.
var oracleClients = map[string]bool{
	"sqlplus": true, "sqlldr": true, "sql": true, "rman": true, "expdp": true, "impdp": true, "exp": true, "imp": true, "dgmgrl": true,
}

var (
	// oracleLogonWord is Oracle's logon, user/password[@db] (sqlplus,
	// python-oracledb's and godror's connect string): its user an Oracle
	// name (letters, digits, _, $, #: C##ADMIN) or a quoted one, a proxy
	// user's [client] after it; its password, quoted or holding no '/' or
	// '"', runs to the word's last '@'. A name holds no '.' and begins with
	// a letter, so no Go module's path (golang.org/x/net@latest), npm's
	// scope (@types/node@20) or registry's image (docker.io/a/b@sha256:...)
	// is one.
	oracleLogonWord = regexp.MustCompile(`^((?:[A-Za-z][A-Za-z0-9_$#]*|"[^"\s]+")(?:\[[A-Za-z0-9_$#]+\])?/)("[^"]*"|[^/\s"]+?)(@[^@\s]*)?$`)
	// oracleConnect is what follows a logon's '@' when it names a database's
	// address: EZConnect's //host[:port][/service], host:port[/service]
	// (an IPv6 address bracketed), host/service, or a connect descriptor
	// ((DESCRIPTION=...)); not a ref (v4, main, v2.1.0) or a version,
	// which follow a repository's owner/name@, and not an image's digest
	// (myorg/app@sha256:<hex>), whose hex is no port of 1 to 5 digits.
	oracleConnect = regexp.MustCompile(`^(?://\S+|(?:[A-Za-z0-9][A-Za-z0-9._-]*|\[[0-9A-Fa-f:.]+\]):[0-9]{1,5}(?:[/:]\S*)?|[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z]\S*|\(\S*)$`)
)

// oracleLogonLogText is w, Oracle's logon (oracleLogonWord) whose '@' is
// followed by a database's address (oracleConnect), with its password
// redacted: scott/<redacted>@db:1521/XE, as any program may take it (a
// python script's argument, --db=); ok is false for any other word.
func oracleLogonLogText(w string) (string, bool) {
	lead, m, tail := oracleLogonOf(w)
	if m == nil {
		return w, false
	}
	if !oracleConnect.MatchString(strings.TrimPrefix(m[3], "@")) {
		return w, false
	}
	return lead + m[1] + RedactedValue + m[3] + tail, true
}

// oracleLogonOf is w's match of oracleLogonWord, and the quotes around it: a
// quote a shell kept because it was escaped (expdp \"sys/pw@orcl as
// sysdba\" gives the words "sys/pw@orcl, as and sysdba"), at w's start or
// end, is no part of the logon. A quoted user ("C##ADMIN"/pw@db) is read
// as the logon's own first.
func oracleLogonOf(w string) (lead string, m []string, tail string) {
	if m = oracleLogonWord.FindStringSubmatch(w); m != nil {
		return "", m, ""
	}
	core := strings.TrimLeft(w, `"'`)
	lead = w[:len(w)-len(core)]
	trimmed := strings.TrimRight(core, `"'`)
	tail = core[len(trimmed):]
	if lead == "" && tail == "" {
		return "", nil, ""
	}
	if m = oracleLogonWord.FindStringSubmatch(trimmed); m == nil {
		return "", nil, ""
	}
	return lead, m, tail
}

// oracleAddressLogon reports whether w is Oracle's logon whose '@' is
// followed by a database's address (oracleLogonLogText).
func oracleAddressLogon(w string) bool {
	_, ok := oracleLogonLogText(w)
	return ok
}

// oracleClientLogonText is w, an Oracle client's word after prev, with its
// logon's password redacted when it is one: user/password@db anywhere
// (an alias, sqlplus scott/<pw>@orcl, too), and user/password with no '@'
// (a local database) right after the client's program, a flag (sqlplus
// -s) or rman's target, catalog or auxiliary, where it is the logon; ok is
// false for any other word.
func oracleClientLogonText(prev, w string) (string, bool) {
	lead, m, tail := oracleLogonOf(w)
	if m == nil {
		return w, false
	}
	if m[3] == "" && !oracleClients[programName(prev)] && !strings.HasPrefix(prev, "-") {
		switch strings.ToLower(prev) {
		case "target", "catalog", "auxiliary":
		default:
			return w, false
		}
	}
	return lead + m[1] + RedactedValue + m[3] + tail, true
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
// and all: --env="AUTH=Bearer <tok>"), a URL, a DSN, a userinfo with no
// scheme (--db=app:<password>@db:5432/shop) or Oracle's logon
// (--db=scott/<password>@db:1521/XE) by valueLogText, a command line by its
// words.
func flagValueLogText(value string) string {
	inner, rest, assigns := strings.Cut(value, "=")
	switch {
	case secretHeader(value):
		return headerLogText(value)
	case isJSONValue(value):
		return RedactedValue
	case assigns && assignedName.MatchString(inner):
		return inner + "=" + RedactedValue + shellTail(rest)
	case strings.Contains(value, "://") || goDSN.MatchString(value) || isJDBC(value) || userinfoWord.MatchString(value) || oracleAddressLogon(value):
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
// jdbc:oracle:thin:user/password@host) by its kind alone, a userinfo with
// no scheme with its password redacted (userinfoLogText), Oracle's logon
// with a database's address with its password redacted
// (oracleLogonLogText), a URL by urlLogText, any other word as it is.
func valueLogText(w string) string {
	if goDSN.MatchString(w) {
		return goDSN.ReplaceAllString(w, "${1}:"+RedactedValue+"@${2}")
	}
	if isJDBC(w) && !strings.Contains(w, "://") {
		kind := strings.SplitN(w, ":", 3)
		return "jdbc:" + kind[1] + ":" + RedactedValue
	}
	if text, ok := userinfoLogText(w); ok {
		return text
	}
	if text, ok := oracleLogonLogText(w); ok {
		return text
	}
	return urlLogTextOf(w, false)
}

// userinfoWord is a word that begins with a userinfo and no scheme,
// [user]:password@...: curl's URL (user:pw@host:port/path), its proxy
// (-x user:pw@proxy:3128, --proxy), a DSN (--db=app:pw@db:5432/shop), a
// Redis address (:pw@redis:6379). Its user holds no ':', '@', '/' or white
// space; its password runs to the word's last '@', as one may hold '@',
// ':' or '/'.
var userinfoWord = regexp.MustCompile(`^((?://)?[^:@/\s]*:)(\S+)@([^@\s]*)$`)

// userinfoLogText is w, a word that begins with a userinfo and no scheme
// (userinfoWord), with its password redacted: user:<redacted>@host:port/path,
// its host and path kept, as a URL's are in the user's own command line;
// ok is false for any other word. These look alike and hold no password:
// a URL's userinfo (scheme://user:pw@, which urlLogTextOf reads), an
// image's digest (nginx:1.25@sha256:<hex>), npm's alias of a version
// (npm:name@1.2.3), a mail address (mailto:a@b). A user with no password
// (app@db, git@host:org/repo) and a package's version (pkg@1.2.3) are no
// userinfoWord.
func userinfoLogText(w string) (string, bool) {
	m := userinfoWord.FindStringSubmatch(w)
	switch {
	case m == nil, strings.HasPrefix(m[2], "//"), strings.Contains(m[2], "://"):
		return w, false
	case imageDigest.MatchString(m[3]),
		strings.EqualFold(m[1], "npm:") && npmVersion.MatchString(m[3]),
		strings.EqualFold(m[1], "mailto:"):
		return w, false
	}
	return m[1] + RedactedValue + "@" + m[3], true
}

var (
	// imageDigest is an image's digest after its '@' (OCI's
	// algorithm:encoded): sha256:<64 hex>.
	imageDigest = regexp.MustCompile(`^[A-Za-z0-9]+(?:[.+_-][A-Za-z0-9]+)*:[0-9A-Fa-f]{32,}$`)
	// npmVersion is a version or a range of one npm's alias names after
	// its '@' (npm:name@1.2.3, @^1.2, @1.x): at most three parts, so no
	// IPv4 address.
	npmVersion = regexp.MustCompile(`^[~^]?v?[0-9]+(?:\.(?:[0-9]+|[xX*])){0,2}(?:[-+][0-9A-Za-z.-]+)?$`)
)

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

// urlLogTextOf is w, or, when w is a URL, its scheme, host and path's shape
// (URLPathShape) and its query's names: a URL in a command line carries a
// token in its userinfo, a path segment or a query value. A URL inside a
// scheme (jdbc:mysql://db/shop?password=...) is written by its inner URL's
// text; one with no host by its text without userinfo password, query
// values or fragment. A host is written only when it is a list of
// host[:port] (hostList); any other by its size: url.Parse takes ; = ( ) ,
// in a host, so SQL Server's jdbc:sqlserver://db;user=sa;password=... and
// Connector/J's jdbc:mysql://(host=db,password=...)/shop parse with the
// password in the host.
//
// trafficHost: w is a URL of the application's traffic (a client's error
// quotes the request's), whose host names are written by their labels'
// shape (models.HostShape), as a name the app chose; a URL of keploy's or
// the user's own command line keeps its hosts.
func urlLogTextOf(w string, trafficHost bool) string {
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
		return u.Scheme + ":" + urlLogTextOf(inner, trafficHost)
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
	switch {
	case !isHostList(host):
		host = "(host withheld, " + strconv.Itoa(len(host)) + " bytes)"
	case trafficHost:
		hosts := strings.Split(host, ",")
		for i, h := range hosts {
			hosts[i] = models.HostShape(h)
		}
		host = strings.Join(hosts, ",")
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

// flagWords are name's words in lower case: split at '-', '_', '.', ':' and
// a lower-to-upper case change (apiKey: api, key; msbuild's -p:Password:
// p, password).
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
		case c == '-' || c == '_' || c == '.' || c == ':':
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
	return JoinCommandLine(RedactArgv(args))
}

// SayWhole writes to console, beside a line of keploy's log that named a
// reason by logText (each host by its shape and tag, models.LossHosts.LogText,
// or by its size an older agent's text, models.UnlistedLogText, and a text
// whose hosts were listed at places it does not hold), the reason whole,
// each host beside its tag (models.LossHosts.ReportText), under what it is:
// where a reader finds the host a log line's tag names, or the text it
// withheld. It says which the log did, and why it withheld one. Nothing when the two are one (the
// reason named no host), or for a nil console (an embedder's, which may be a
// log).
func SayWhole(console io.Writer, what, logText, whole string) {
	if console == nil || whole == "" || logText == whole {
		return
	}
	tagged := strings.Contains(whole, "[host tag ")
	// A listed host with no tag is an IP address with a zone or a port that
	// is no number, which the log writes by its shape (10.0.0.1:{}).
	how := "by its hosts' shapes"
	if tagged {
		how = "by its host tags"
	}
	// A text the log withheld: the agent listed none of its hosts (an older
	// agent), or listed them at places the text does not hold.
	unlisted, misplaced := strings.Contains(logText, models.UnlistedLogNote), strings.Contains(logText, models.MisplacedLogNote)
	switch {
	case unlisted && misplaced && tagged:
		how = "by its host tags, and where the agent did not list them, or listed them at places its text does not hold, by its size"
	case unlisted && misplaced:
		how = "by its size, as the agent did not list its host names, or listed them at places the text does not hold"
	case unlisted && tagged:
		how = "by its host tags, and where the agent did not list them by its size"
	case unlisted:
		how = "by its size, as the agent did not list its host names"
	case misplaced && tagged:
		how = "by its host tags, and where the agent listed them at places its text does not hold, by its size"
	case misplaced:
		how = "by its size, as the agent listed its host names at places the text does not hold"
	}
	_, _ = fmt.Fprintf(console, "%s %s, which keploy's log names %s: %s\n", Emoji, what, how, whole)
}
