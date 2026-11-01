package containerkey

import (
	"maps"
	"regexp"
	"strings"
	"unicode"

	"go.keploy.io/server/v3/pkg/secretname"
)

// A secret's value never counts in a fingerprint (Fingerprint). What names a
// secret is keploy's sanitizer's rule for a field name (keploy's secret
// detector's context keywords and sensitive keys, secretname.Name): its
// segments, split at _, -, . and the other non-letters and at camelCase,
// include a keyword, or the name holds a word no other name holds
// (PGPASSWORD, requirepass). And a value carries one if it holds a URL's
// password or a key=value pair a secret's name keys (a JAAS configuration, a
// connection string, a JVM property), or a word that names one followed by
// its value (requirepass hunter2). A short flag's secret is told apart only
// where the program is known to take one: a mysql client's attached
// -p<password>, a redis client's -a <password>; another program's (curl's
// -u user:pw) counts as given: pass it in a variable or a long flag named
// for it.

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
		if secretname.Name(m[1]) {
			return true
		}
	}
	words := strings.Fields(value)
	for i, w := range words {
		flag, _, _ := strings.Cut(w, "=")
		if strings.HasPrefix(flag, "-") && secretname.Name(flag) && (strings.Contains(w, "=") || i+1 < len(words)) {
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
		case keyWord.MatchString(w) && secretname.Name(w) && i+1 < len(words):
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
		if secretname.Name(name) || carriesSecret(value) {
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
		if secretname.Name(k) || carriesSecret(v) {
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

// RedactArgs is args as keploy may log or store them: every secret they hold
// (a flag's value named for a secret, a word marked a secret's value, an
// argument that carries one) is RedactedText, as a container's command line
// is redacted before keploy fingerprints it. The record log's command line of
// a process that made a recorded call, and its origin's exe, read it
// (starts.Registry.StampOrigin).
func RedactArgs(args []string) []string {
	out := redactArgs(args)
	for i, a := range out {
		out[i] = strings.ReplaceAll(a, redacted, RedactedText)
	}
	return out
}

// RedactedText is what RedactArgs puts in place of a secret's value.
const RedactedText = "<redacted>"

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
		if flag, _, ok := strings.Cut(a, "="); ok && strings.HasPrefix(flag, "-") && strings.IndexFunc(flag, unicode.IsSpace) < 0 && secretname.Name(flag) {
			out[i] = flag + "=" + redacted
			continue
		}
		if carriesSecret(a) {
			out[i] = redacted
			continue
		}
		// The word after a flag named for a secret is its value, whatever
		// it looks like (--password -x).
		if i > 0 && strings.HasPrefix(args[i-1], "-") && !strings.Contains(args[i-1], "=") && secretname.Name(args[i-1]) {
			out[i] = redacted
		}
	}
	return out
}
