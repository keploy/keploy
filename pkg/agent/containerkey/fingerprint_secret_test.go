package containerkey

import (
	"maps"
	"testing"
)

// A recording names a container by its fingerprint, which anyone who reads
// the recording holds; a fingerprint that a secret's value changes is an
// offline oracle for that value: hash a guess with the rest of the
// configuration, compare. So no secret's value counts: an environment
// variable, label or argument named for one, or a value that carries one (a
// URL's password, a password=... inside a JAAS configuration or a
// connection string). Two containers that differ only in a secret then share
// a fingerprint: ambiguous where both answer one scope, withheld, and a miss
// -- never the other's answers.
func TestNoSecretValueChangesTheFingerprint(t *testing.T) {
	base := Fingerprint(suitePostgres())
	for name, change := range map[string]func(*Config){
		"POSTGRES_PASSWORD":  func(c *Config) { setEnv(c, "POSTGRES_PASSWORD", "hunter2") },
		"PGPASSWORD":         func(c *Config) { setEnv(c, "PGPASSWORD", "hunter2") },
		"an API token":       func(c *Config) { setEnv(c, "STRIPE_API_TOKEN", "sk_live_1") },
		"a camelCase secret": func(c *Config) { setEnv(c, "dbPwd", "abc") },
		"a camelCase key":    func(c *Config) { setEnv(c, "signingKey", "abc") },
		"redis's arguments":  func(c *Config) { setEnv(c, "REDIS_ARGS", "--requirepass hunter2 --appendonly yes") },
		"a shell's command":  func(c *Config) { c.Cmd = []string{"sh", "-c", "redis-server --requirepass hunter2"} },
		"one argument":       func(c *Config) { c.Cmd = []string{"redis-server", "--requirepass hunter2"} },
		"an Erlang cookie":   func(c *Config) { setEnv(c, "RABBITMQ_ERLANG_COOKIE", "hunter2") },
		"an Erlang flag":     func(c *Config) { setEnv(c, "RABBITMQ_SERVER_ADDITIONAL_ERL_ARGS", "-setcookie hunter2") },
		"a Go MySQL DSN":     func(c *Config) { setEnv(c, "DB_CONN", "app:hunter2@tcp(mysql:3306)/orders") },
		"an Oracle login":    func(c *Config) { setEnv(c, "ORA_LOGIN", "app/hunter2@db:1521/XE") },
		"a JSON value": func(c *Config) {
			setEnv(c, "SPRING_APPLICATION_JSON", `{"spring":{"datasource":{"password":"hunter2"}}}`)
		},
		"a flag's value that looks like a flag": func(c *Config) { c.Cmd = []string{"server", "--password", "-hunter2"} },
		"the entrypoint's":                      func(c *Config) { c.Entrypoint = []string{"docker-entrypoint.sh", "--token=hunter2"} },
		"one argument with a later =":           func(c *Config) { c.Cmd = []string{"redis-server", "--requirepass hunter2 --appendonly=yes"} },
		"a DSN without a host":                  func(c *Config) { setEnv(c, "DB_CONN", "app:hunter2@/orders") },
		"Oracle's EZConnect":                    func(c *Config) { setEnv(c, "ORA_LOGIN", "app/hunter2@//db:1521/XE") },
		"a config in one string":                func(c *Config) { setEnv(c, "REDIS_EXTRA_CONF", "appendonly yes\nrequirepass hunter2") },
		"a config's argument":                   func(c *Config) { c.Cmd = []string{"keydb-server", "masterauth", "hunter2"} },
		"mysql's attached -p":                   func(c *Config) { c.Cmd = []string{"mysql", "-h", "db", "-uapp", "-phunter2"} },
		"mysql's -p in a shell":                 func(c *Config) { c.Cmd = []string{"sh", "-c", "mysqladmin -uroot -phunter2 ping"} },
		"redis-cli's -a":                        func(c *Config) { c.Cmd = []string{"redis-cli", "-h", "cache", "-a", "hunter2", "ping"} },
		"redis-cli's -a in a variable":          func(c *Config) { setEnv(c, "HEALTHCHECK", "/usr/local/bin/redis-cli -a hunter2 ping") },
		"mysql's -p with an =":                  func(c *Config) { c.Cmd = []string{"mysql", "-psecret-hunter2=x"} },
		"a flag the entrypoint ends with": func(c *Config) {
			c.Entrypoint, c.Cmd = []string{"redis-cli", "-a"}, []string{"hunter2", "ping"}
		},
		"over the image's own": func(c *Config) {
			c.Image.Env = append(c.Image.Env, "MYSQL_PASSWORD=image-default")
			setEnv(c, "MYSQL_PASSWORD", "hunter2")
		},
		"a URL's password": func(c *Config) { setEnv(c, "DATABASE_URL", "postgres://app:hunter2@db:5432/orders") },
		"a JAAS password": func(c *Config) {
			setEnv(c, "KAFKA_SASL_JAAS_CONFIG", `org.apache.kafka.common.security.plain.PlainLoginModule required username="app" password="hunter2";`)
		},
		"a connection string's": func(c *Config) { setEnv(c, "MSSQL_CONN", "Server=db;User Id=sa;Password=hunter2;") },
		"a JVM property's":      func(c *Config) { setEnv(c, "JAVA_OPTS", "-Xmx1g -Djavax.net.ssl.trustStorePassword=hunter2") },
		"a flag's value":        func(c *Config) { c.Cmd = []string{"redis-server", "--requirepass", "hunter2"} },
		"a flag=value":          func(c *Config) { c.Cmd = []string{"postgres", "--password=hunter2"} },
		"a label's": func(c *Config) {
			c.Labels = maps.Clone(c.Labels)
			c.Labels["traefik.http.middlewares.auth.basicauth.users"] = "app:$apr1$x$hunter2"
		},
	} {
		withA, withB := suitePostgres(), suitePostgres()
		change(&withA)
		change(&withB)
		swap(&withB, "hunter2", "correct-horse")
		swap(&withB, "sk_live_1", "sk_live_2")
		swap(&withB, "abc", "xyz")
		swap(&withB, "-hunter2", "-other")
		if a, b := Fingerprint(withA), Fingerprint(withB); a != b {
			t.Errorf("%s: the secret's value changes the fingerprint (%s, %s): a recording is an oracle for it", name, a, b)
		}
	}
	// What is no secret still tells containers apart, and so does a secret
	// variable's presence.
	other := suitePostgres()
	setEnv(&other, "POSTGRES_DB", "billing")
	if Fingerprint(other) == base {
		t.Error("POSTGRES_DB's value no longer tells two containers apart")
	}
	none := suitePostgres()
	none.Env = without(none.Env, "POSTGRES_PASSWORD")
	if Fingerprint(none) == base {
		t.Error("a container without POSTGRES_PASSWORD has the fingerprint of one with it")
	}
}

func setEnv(c *Config, name, value string) {
	c.Env = append(without(c.Env, name), name+"="+value)
}

func without(env []string, name string) []string {
	var out []string
	for _, e := range env {
		if len(e) > len(name) && e[:len(name)+1] == name+"=" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// swap replaces old with new in every value of c, as another secret would be.
func swap(c *Config, old, new string) {
	r := func(s string) string {
		for i := 0; i+len(old) <= len(s); i++ {
			if s[i:i+len(old)] == old {
				return s[:i] + new + s[i+len(old):]
			}
		}
		return s
	}
	for i := range c.Env {
		c.Env[i] = r(c.Env[i])
	}
	for i := range c.Cmd {
		c.Cmd[i] = r(c.Cmd[i])
	}
	for i := range c.Entrypoint {
		c.Entrypoint[i] = r(c.Entrypoint[i])
	}
	for k, v := range c.Labels {
		c.Labels[k] = r(v)
	}
}

// The current form is pinned: a change of what is redacted or how the
// configuration is hashed moves every container's fingerprint, and every
// recording made with the form it replaces then misses. Such a change
// needs a form of its own (fingerprintForm) and its own legacy reading.
func TestTheFingerprintsFormIsPinned(t *testing.T) {
	if got := Fingerprint(suitePostgres()); got != pinnedSuite {
		t.Fatalf("Fingerprint(suite postgres) = %s, want %s: the form moved", got, pinnedSuite)
	}
}

// pinnedSuite is Fingerprint(suitePostgres()) in the current form.
const pinnedSuite = "s33277505a0ad"
