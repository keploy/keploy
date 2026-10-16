package containerkey

import (
	"maps"
	"regexp"
	"strings"
	"unicode"
)

// A secret's value never counts in a fingerprint (Fingerprint). What names a
// secret is keploy's sanitizer's rule for a field name (keploy's secret
// detector's context keywords and sensitive keys): its segments, split at _,
// -, . and the other non-letters and at camelCase, include a keyword
// (secretWords), or the name holds a word no other name holds (secretParts:
// PGPASSWORD, requirepass). And a value carries one if it holds a URL's
// password or a key=value pair a secret's name keys (a JAAS configuration, a
// connection string, a JVM property), or a word that names one followed by
// its value (requirepass hunter2). A short flag's secret is told apart only
// where the program is known to take one: a mysql client's attached
// -p<password>, a redis client's -a <password>; another program's (curl's
// -u user:pw) counts as given: pass it in a variable or a long flag named
// for it.

// secretWords are the keywords a name's segments are compared with.
var secretWords = map[string]bool{
	"secret": true, "secrets": true, "token": true, "tokens": true, "key": true, "keys": true,
	"password": true, "passwords": true, "pass": true, "passwd": true, "pwd": true, "pw": true,
	"passphrase": true, "auth": true, "authorization": true, "credential": true, "credentials": true,
	"cred": true, "creds": true, "hmac": true, "signature": true, "sig": true, "encrypt": true,
	"cipher": true, "hash": true, "oauth": true, "apikey": true, "dsn": true, "pin": true,
	"otp": true, "cvv": true, "ssn": true, "private": true, "jaas": true, "salt": true,
	"pat": true, "jwt": true, "bearer": true, "cookie": true, "cookies": true,
}

// secretParts are words a name is a secret's for holding anywhere in it,
// its separators left out (connection_string: connectionstring).
var secretParts = []string{"password", "passwd", "pass", "secret", "token", "credential", "apikey", "passphrase", "privatekey", "privkey", "accesskey", "auth", "cookie", "connectionstring", "sessionid"}

// secretName reports whether name names a secret.
func secretName(name string) bool {
	joined := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, name)
	for _, p := range secretParts {
		if strings.Contains(joined, p) {
			return true
		}
	}
	for _, seg := range nameSegments(name) {
		if secretWords[seg] {
			return true
		}
	}
	return false
}

// nameSegments splits name, lowercased, at every non-letter-or-digit and at
// each lower-to-upper change of case (clientSecret: client, secret).
func nameSegments(name string) []string {
	var segs []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	var prev rune
	for _, r := range name {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	return segs
}

var (
	// urlPassword is a URL whose user information has a password.
	urlPassword = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^/\s:@]*:[^/\s@]*@`)
	// userPassword is user information with a password and no scheme
	// before it: a Go MySQL DSN (app:pw@tcp(db:3306)/x, app:pw@/x),
	// user:pw@host, and Oracle's user/pw@host and user/pw@//host.
	userPassword = regexp.MustCompile(`[^\s/@:=]+[:/][^\s/@]+@[A-Za-z0-9(._/\-]`)
	// keyWord is a word that can name a setting: a config line's key
	// (requirepass hunter2), a flag's name without its dashes.
	keyWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\-]*$`)
	// keyedValue is a key=value or key: value pair in a value, its key
	// quoted or not (JAAS's password="...", a connection string's
	// Password=...;, a JVM property's -Dx.password=..., JSON's "password":).
	keyedValue = regexp.MustCompile(`["']?([A-Za-z0-9_.\-]+)["']?\s*[=:]`)
)

// carriesSecret reports whether value holds a secret: a URL's or a DSN's
// password, a pair a secret's name keys, or, among the words it holds (a
// command line in one string: REDIS_ARGS, sh -c), a flag named for one
// (--requirepass x, --password=x, -setcookie x).
func carriesSecret(value string) bool {
	if urlPassword.MatchString(value) || userPassword.MatchString(value) {
		return true
	}
	for _, m := range keyedValue.FindAllStringSubmatch(value, -1) {
		if secretName(m[1]) {
			return true
		}
	}
	words := strings.Fields(value)
	for i, w := range words {
		flag, _, _ := strings.Cut(w, "=")
		if strings.HasPrefix(flag, "-") && secretName(flag) && (strings.Contains(w, "=") || i+1 < len(words)) {
			return true
		}
	}
	for _, secret := range secretWordsOf(words) {
		if secret {
			return true
		}
	}
	return false
}

// mysqlClients are the programs whose -p takes its password attached
// (-phunter2), and redisClients those whose -a takes it as the next word.
var (
	mysqlClients = map[string]bool{"mysql": true, "mariadb": true, "mysqladmin": true, "mysqldump": true, "mysqlsh": true, "mysqlimport": true, "mysqlcheck": true, "mariadb-dump": true, "mariadb-admin": true}
	redisClients = map[string]bool{"redis-cli": true, "valkey-cli": true, "keydb-cli": true}
)

// secretWordsOf marks the words of a command line, or of a config in one
// string, that are a secret's value: the word after a word that names one
// (requirepass hunter2, the value of a dashless key), a mysql client's
// attached -p<password>, and a redis client's -a <password>. A secret given
// to another program's short flag is not told from another value.
func secretWordsOf(words []string) []bool {
	marks := make([]bool, len(words))
	program := ""
	for i, w := range words {
		if base := w[strings.LastIndexByte(w, '/')+1:]; mysqlClients[base] || redisClients[base] {
			program = base
			continue
		}
		switch {
		case mysqlClients[program] && strings.HasPrefix(w, "-p") && !strings.HasPrefix(w, "--") && len(w) > 2:
			marks[i] = true
		case redisClients[program] && w == "-a" && i+1 < len(words):
			marks[i+1] = true
		case keyWord.MatchString(w) && secretName(w) && i+1 < len(words):
			marks[i+1] = true
		}
	}
	return marks
}

// redacted is what a secret's value is in the configuration a fingerprint
// hashes: the same for every value, so only the secret's presence counts.
const redacted = "\x00secret"

// redactSecrets is c with every secret's value put as redacted: an
// environment variable or label named for one, or whose value carries one;
// a command or entrypoint argument that carries one, the value of a
// --name=value argument named for one, and the argument after a --name
// argument named for one. The image's own values are redacted the same way,
// so a value the image gives is still the image's.
func redactSecrets(c Config) Config {
	out := c
	out.Env = redactEnv(c.Env)
	out.Labels = redactLabels(c.Labels)
	out.Entrypoint, out.Cmd = redactArgv(c.Entrypoint, c.Cmd)
	out.Image.Env = redactEnv(c.Image.Env)
	out.Image.Labels = redactLabels(c.Image.Labels)
	out.Image.Entrypoint, out.Image.Cmd = redactArgv(c.Image.Entrypoint, c.Image.Cmd)
	return out
}

func redactEnv(env []string) []string {
	if env == nil {
		return nil
	}
	out := make([]string, len(env))
	for i, e := range env {
		name, value, _ := strings.Cut(e, "=")
		if secretName(name) || carriesSecret(value) {
			e = name + "=" + redacted
		}
		out[i] = e
	}
	return out
}

func redactLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	out := maps.Clone(labels)
	for k, v := range out {
		if secretName(k) || carriesSecret(v) {
			out[k] = redacted
		}
	}
	return out
}

// redactArgv redacts a container's entrypoint and command as the one argv
// Docker runs them as, so a flag or a client the entrypoint ends with and
// the value the command begins with are read together (redis-cli -a, then
// the password). Each keeps its own nil-ness.
func redactArgv(entrypoint, cmd []string) (outEntrypoint, outCmd []string) {
	if entrypoint == nil && cmd == nil {
		return nil, nil
	}
	all := redactArgs(append(append([]string{}, entrypoint...), cmd...))
	if entrypoint != nil {
		outEntrypoint = all[:len(entrypoint):len(entrypoint)]
	}
	if cmd != nil {
		outCmd = all[len(entrypoint):]
	}
	return outEntrypoint, outCmd
}

func redactArgs(args []string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	marks := secretWordsOf(args)
	for i, a := range args {
		out[i] = a
		// A word marked a secret's value (a mysql client's -p<password>)
		// is redacted whole, whatever it holds.
		if marks[i] {
			out[i] = redacted
			continue
		}
		// --name=value, named for a secret: its value. Only where the name
		// is the whole of what comes before the "=": an argument holding a
		// command line ("--requirepass x --appendonly=yes") is one value,
		// below.
		if flag, _, ok := strings.Cut(a, "="); ok && strings.HasPrefix(flag, "-") && strings.IndexFunc(flag, unicode.IsSpace) < 0 && secretName(flag) {
			out[i] = flag + "=" + redacted
			continue
		}
		if carriesSecret(a) {
			out[i] = redacted
			continue
		}
		// The word after a flag named for a secret is its value, whatever
		// it looks like (--password -x).
		if i > 0 && strings.HasPrefix(args[i-1], "-") && !strings.Contains(args[i-1], "=") && secretName(args[i-1]) {
			out[i] = redacted
		}
	}
	return out
}
