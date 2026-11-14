package utils

import (
	"bufio"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"gopkg.in/yaml.v3"
)

// The errors keploy gets parsing what an application sent quote it; once
// withheld, none holds a byte of it, and each is still the error it was.
func TestWithholdInputLeavesNoneOfTheInput(t *testing.T) {
	const s = logleak.Canary
	read := func(raw string) error {
		_, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		return err
	}
	_, parseErr := strconv.ParseInt(s, 10, 64)
	// A malformed escape, built at run time: the URL is meant not to parse.
	badEscape := strings.Repeat("%z", 1) + "z"
	_, urlErr := url.Parse("http://h/" + badEscape + "?token=" + s)
	var into struct{ Token int }
	yamlErr := yaml.Unmarshal([]byte("token: "+s), &into)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a malformed request line", read("GET /?access_token=" + s + "\r\nHost: h\r\n\r\n")},
		{"a malformed header line", read("GET / HTTP/1.1\r\nHost: h\r\nAuthorization Bearer " + s + "\r\n\r\n")},
		{"strconv", parseErr},
		{"url.Parse", urlErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil || !strings.Contains(tc.err.Error(), s[:8]) {
				t.Fatalf("precondition: the error %v does not quote the input", tc.err)
			}
			got := WithholdInput(tc.err)
			if logleak.Holds(got.Error(), s) {
				t.Fatalf("WithholdInput left the input in %q", got.Error())
			}
			if !errors.Is(got, tc.err) {
				t.Fatal("WithholdInput hid the error from errors.Is")
			}
		})
	}
	// YAML quotes the head of the value it refused in backquotes: fewer
	// bytes than the windows above, all of a short secret.
	if !strings.Contains(yamlErr.Error(), "`"+s[:7]) {
		t.Fatalf("precondition: the YAML error %d bytes does not quote the value's head", len(yamlErr.Error()))
	}
	if strings.Contains(WithholdInput(yamlErr).Error(), s[:7]) || !errors.Is(WithholdInput(yamlErr), yamlErr) {
		t.Fatal("WithholdInput left the YAML error's quoted value, or hid the error")
	}
	plain := errors.New("connection reset by peer")
	if WithholdInput(plain) != plain || WithholdInput(nil) != nil {
		t.Fatal("an error that quotes nothing is not returned as it is")
	}
}

// A URL is named by its shape: its method, host, scheme, its path's
// segments counted and its query's names, never a value, a segment or
// userinfo.
func TestURLLogMetaNamesTheShapeOnly(t *testing.T) {
	const s = logleak.Canary
	meta := URLLogMeta("GET", "https://app:"+s+"@api.example.com/v1/reset/"+s+"?access_token="+s+"&lang=en")
	want := map[string]string{"method": "GET", "host": "api.example.com", "scheme": "https", "path": "/{}/{}/{}", "query keys": "access_token,lang"}
	for k, v := range want {
		if meta[k] != v {
			t.Errorf("%s = %q, want %q", k, meta[k], v)
		}
	}
	for k, v := range meta {
		if strings.Contains(v, s[:8]) {
			t.Errorf("%s holds part of the URL's secret", k)
		}
	}
	// A path that begins with "//" is a path, not a host and a path.
	if meta := URLLogMeta("GET", "//"+s+"/v1/me?code="+s); meta["host"] != "" || meta["path"] != "//{}/{}/{}" || meta["query keys"] != "code" {
		t.Errorf("a // path is named %v; want its shape, no host", meta)
	}
	if bad := URLLogMeta("GET", "http://h/%zz"+s); bad["url"] != "unparsable" || strings.Contains(bad["bytes"], s[:8]) {
		t.Errorf("an unparsable URL is named %v, want its size only", bad)
	}
}

// A re-exec's argv: an argument that is a command line is redacted inside.
func TestRedactArgvRedactsInsideACommandArgument(t *testing.T) {
	const s = logleak.Canary
	got := RedactArgv([]string{"/usr/bin/keploy", "-c", "./app --api-key " + s, "--debug"})
	if strings.Join(got, " ") != "/usr/bin/keploy -c ./app --api-key <redacted> --debug" {
		t.Fatalf("RedactArgv = %q", got)
	}
}

// withheldHost is what urlLogText writes for a URL's host that is no list
// of host[:port].
func withheldHost(host string) string {
	return "(host withheld, " + strconv.Itoa(len(host)) + " bytes)"
}

// A command line is logged with every value that can be a credential
// redacted: an environment assignment's, a secret-named flag's, a URL's
// password; its program and its other flags stay.
func TestRedactCommandLineLeavesNoCredential(t *testing.T) {
	const s = logleak.Canary
	for _, tc := range []struct{ cmd, want string }{
		{"docker run -e API_TOKEN=" + s + " --name app app:latest", "docker run -e API_TOKEN=<redacted> --name app app:latest"},
		{"env DB_DSN=postgres://app:" + s + "@db:5432/shop ./server --port 8080", "env DB_DSN=<redacted> ./server --port 8080"},
		{"./server --db postgres://app:" + s + "@db/shop", "./server --db postgres://db/{}"},
		{"./server --password " + s + " --api-key=" + s, "./server --password <redacted> --api-key=<redacted>"},
		{"npm start", "npm start"},
		{"docker run --env=API_TOKEN=" + s + " app", "docker run --env=API_TOKEN=<redacted> app"},
		{"docker build --build-arg=NPM_TOKEN=" + s + " .", "docker build --build-arg=NPM_TOKEN=<redacted> ."},
		{"mysql -h db -uroot -p" + s + " shop", "mysql -h db -uroot -p<redacted> shop"},
		{`curl -H "Authorization: Bearer ` + s + `" -u app:` + s + ` https://api.example.com`, "curl -H 'Authorization: <redacted>' -u app:<redacted> https://api.example.com"},
		{`./app --password "two words ` + s + `" --port 80`, "./app --password <redacted> --port 80"},
		{`sh -c "./app --api-key ` + s + `"`, "sh -c './app --api-key <redacted>'"},
		{"redis-cli -h cache -a " + s + " ping", "redis-cli -h cache -a <redacted> ping"},
		{"sshpass -p " + s + " ssh app@host", "sshpass -p <redacted> ssh app@host"},
		{"mongosh -u app -p " + s + " shop", "mongosh -u app -p <redacted> shop"},
		{`curl -d '{"password":"` + s + `"}' https://api.example.com/hooks/` + s + `?token=` + s, "curl -d <redacted> https://api.example.com/{}/{}?{token}"},
		{"docker run --name mysql-db -p3306:3306 -e MYSQL_ROOT_PASSWORD=" + s + " mysql:8", "docker run --name mysql-db -p3306:3306 -e MYSQL_ROOT_PASSWORD=<redacted> mysql:8"},
		// Other programs' flags that curl's or a client's rules would read
		// as secrets are left alone.
		{"docker run -d -p 8080:8080 --name mongo -p 27017:27017 -u 1000:1000 app", "docker run -d -p 8080:8080 --name mongo -p 27017:27017 -u 1000:1000 app"},
		{"gunicorn -b 0.0.0.0:8000 app:wsgi", "gunicorn -b 0.0.0.0:8000 app:wsgi"},
		{"docker -H tcp://dockerd:2375 ps", "docker -H tcp://dockerd:2375 ps"},
		{"sudo env PGPASSWORD=" + s + " mongosh -p " + s + " shop", "sudo env PGPASSWORD=<redacted> mongosh -p <redacted> shop"},
		{"./app --passThroughPorts 5432 --bypass 1 --db-token " + s, "./app --passThroughPorts 5432 --bypass 1 --db-token <redacted>"},
		{`C:\app\srv.exe --apiKey ` + s, `C:\app\srv.exe --apiKey <redacted>`},
		{"java -Dpassword=" + s + " -Dspring.datasource.password=" + s + " -jar app.jar", "java -Dpassword=<redacted> -Dspring.datasource.password=<redacted> -jar app.jar"},
		{"./cli --bearer " + s + " --jwt=" + s, "./cli --bearer <redacted> --jwt=<redacted>"},
		{"./migrate app:" + s + "@tcp(db:3306)/shop", "./migrate app:<redacted>@tcp(db:3306)/shop"},
		{"./migrate root:p@ss" + s + "@tcp(db:3306)/shop --db=app:" + s + "@tcp(mysql:3306)/app", "./migrate root:<redacted>@tcp(db:3306)/shop --db=app:<redacted>@tcp(mysql:3306)/app"},
		{"./app --database-url=postgres://u:" + s + "@h/db?sslmode=disable", "./app --database-url=postgres://h/{}?{sslmode}"},
		{"mongod --auth --port 27017", "mongod --auth --port 27017"},
		{`sh -c "PORT=8080 npm start"`, "sh -c 'PORT=<redacted> npm start'"},
		{"docker exec db mysql -uroot -p" + s + " shop", "docker exec db mysql -uroot -p<redacted> shop"},
		{"kubectl exec -it cache -- redis-cli -a " + s + " ping", "kubectl exec -it cache -- redis-cli -a <redacted> ping"},
		{"timeout 10 mysql -p" + s, "timeout 10 mysql -p<redacted>"},
		{"./app --name\u00a0x --token " + s, "./app --name\u00a0x --token <redacted>"},
		// A value with white space is redacted whole by the rule that
		// names it, not split as a command line first.
		{`docker run -e "AUTH=Bearer ` + s + `" app`, "docker run -e AUTH=<redacted> app"},
		{`wget --header="Authorization: Bearer ` + s + `" https://api.example.com`, "wget '--header=Authorization: <redacted>' https://api.example.com"},
		{`./app --password="pass ` + s + `"`, "./app --password=<redacted>"},
		{`mysql "-pmy ` + s + `" shop`, "mysql -p<redacted> shop"},
		{`sh -c "TOKEN='a ` + s + `' ./app; echo ok"`, "sh -c 'TOKEN=<redacted> ./app; echo ok'"},
		{`sh -c "TOKEN=` + s + `; ./app && B=` + s + `&& c"`, "sh -c 'TOKEN=<redacted>; ./app && B=<redacted>&& c'"},
		// A JDBC URL's inner URL, as a flag's value, a property or a word.
		{"java -jar app.jar --spring.datasource.url=jdbc:mysql://db:3306/shop?user=root&password=" + s, "java -jar app.jar --spring.datasource.url=jdbc:mysql://db:3306/{}?{password,user}"},
		{"java -Dspring.datasource.url=jdbc:postgresql://db/shop?password=" + s + " -jar app.jar", "java -Dspring.datasource.url=jdbc:postgresql://db/{}?{password} -jar app.jar"},
		{"./app jdbc:oracle:thin:scott/" + s + "@db:1521/orcl", "./app jdbc:oracle:<redacted>"},
		// A host that is no list of host[:port] (url.Parse takes ; = ( ) ,
		// in one): SQL Server's documented form, Connector/J's key-value
		// and address-equals forms, as a property, a flag's value and a
		// word. A port before the ';' does not parse, and is withheld too.
		{"java -Dspring.datasource.url=jdbc:sqlserver://localhost;encrypt=true;user=sa;password=" + s + " -jar app.jar", "java '-Dspring.datasource.url=jdbc:sqlserver://" + withheldHost("localhost;encrypt=true;user=sa;password="+s) + "' -jar app.jar"},
		{"java -jar app.jar --spring.datasource.url=jdbc:sqlserver://db;databaseName=shop;user=sa;password=" + s + ";encrypt=true", "java -jar app.jar '--spring.datasource.url=jdbc:sqlserver://" + withheldHost("db;databaseName=shop;user=sa;password="+s+";encrypt=true") + "'"},
		{"./app jdbc:sqlserver://db;user=sa;password=" + s, "./app 'jdbc:sqlserver://" + withheldHost("db;user=sa;password="+s) + "'"},
		{"./app jdbc:sqlserver://db:1433;user=sa;password=" + s, "./app 'jdbc:(unparsable URL, 58 bytes)'"},
		{"java -jar app.jar --spring.datasource.url=jdbc:mysql://(host=db,port=3306,user=root,password=" + s + ")/shop", "java -jar app.jar '--spring.datasource.url=jdbc:mysql://" + withheldHost("(host=db,port=3306,user=root,password="+s+")") + "/{}'"},
		{"java -Dspring.datasource.url=jdbc:mysql://address=(host=db)(port=3306)(user=root)(password=" + s + ")/shop -jar app.jar", "java '-Dspring.datasource.url=jdbc:mysql://" + withheldHost("address=(host=db)(port=3306)(user=root)(password="+s+")") + "/{}' -jar app.jar"},
		{"./app jdbc:mysql://(host=db,password=" + s + ")/shop", "./app 'jdbc:mysql://" + withheldHost("(host=db,password="+s+")") + "/{}'"},
		// A list of host[:port], a bracketed IPv6 address, an empty name
		// with a port: kept.
		{"./app --db mongodb://m1:27017,m2.example.com:27018/shop --peer http://[::1]:8080/x --api http://:8080/", "./app --db mongodb://m1:27017,m2.example.com:27018/{} --peer http://[::1]:8080/{} --api http://:8080/"},
		// An assignment a flag carries is redacted whole before its value
		// is read as a command line: white space and all.
		{`docker run --env="AUTH=Bearer ` + s + `" app`, "docker run --env=AUTH=<redacted> app"},
		{`docker build --build-arg="X=a ` + s + `" .`, "docker build --build-arg=X=<redacted> ."},
		// Authorization as a flag's name; a JSON value; PowerShell's
		// environment assignment, attached and spaced.
		{`./app --authorization "Bearer ` + s + `" --authorization="Bearer ` + s + `"`, "./app --authorization <redacted> --authorization=<redacted>"},
		{`java -jar app.jar --spring.application.json='{"spring.datasource.password":"` + s + `"}'`, "java -jar app.jar --spring.application.json=<redacted>"},
		{`node app.js --config '{"apiKey": "` + s + `"}' --port 80`, "node app.js --config <redacted> --port 80"},
		{`powershell -Command "$env:API_TOKEN='` + s + `'; ./app.exe"`, "powershell -Command '$env:API_TOKEN=<redacted>; ./app.exe'"},
		{`powershell -Command "$Env:API_TOKEN = '` + s + `'; ./app.exe"`, "powershell -Command '$Env:API_TOKEN = <redacted>; ./app.exe'"},
		// Kept: an IPv6 host, a word that only looks like JSON.
		{"./app --bind=[::1]:8080 --list=[a,b]", "./app --bind=[::1]:8080 --list=[a,b]"},
		// A secret header, a secret flag's value that begins with '-', a
		// client's attached or AUTH-given password.
		{`./app -H "Authorization: Bearer ` + s + `" --header "X-Api-Key: ` + s + `"`, "./app -H 'Authorization: <redacted>' --header 'X-Api-Key: <redacted>'"},
		{"./app --password -" + s, "./app --password <redacted>"},
		{"curl -U proxy:" + s + " --data-ascii " + s + " -uapp:" + s + " https://h/", "curl -U proxy:<redacted> --data-ascii <redacted> -uapp:<redacted> https://h/"},
		{"mongosh -p" + s + " shop", "mongosh -p<redacted> shop"},
		{"docker login -u app -p " + s + " registry.example.com", "docker login -u app -p <redacted> registry.example.com"},
		{"redis-cli -h cache AUTH app " + s, "redis-cli -h cache AUTH <redacted> <redacted>"},
		{"./app a.root-password=" + s, "./app a.root-password=<redacted>"},
	} {
		got := RedactCommandLine(tc.cmd)
		if got != tc.want {
			t.Errorf("RedactCommandLine = %q, want %q", got, tc.want)
		}
		if logleak.Holds(got, s) {
			t.Errorf("RedactCommandLine left the secret in %q", got)
		}
	}
}

// A quoted URL or path is written by its shape; keploy's own quoted words
// stay.
func TestShapeQuotedURLsKeepsKeploysWords(t *testing.T) {
	const s = logleak.Canary
	got := ShapeQuotedURLs(`Post "http://app:8080/v1/reset/` + s + `?token=` + s + `": EOF; header "Content-Encoding" is "gzip"; path "/v1/me/` + s + `"`)
	if logleak.Holds(got, s) {
		t.Fatalf("ShapeQuotedURLs left the secret in %d bytes", len(got))
	}
	for _, want := range []string{`"http://app:8080/{}/{}/{}?{token}"`, `"Content-Encoding"`, `"gzip"`, `"/{}/{}/{}"`} {
		if !strings.Contains(got, want) {
			t.Errorf("ShapeQuotedURLs = %q, want it to hold %s", got, want)
		}
	}
}

// The argv of a command keploy runs through a shell is redacted inside the
// shell's -c argument, by that command's own program's rules.
func TestRedactCmdArgsReadsTheShellsCommand(t *testing.T) {
	const s = logleak.Canary
	got := RedactCmdArgs([]string{"/bin/sh", "-c", `curl -H "Authorization: Bearer ` + s + `" -u me:` + s + ` https://api.example.com && mysql -p` + s})
	if logleak.Holds(got, s) {
		t.Fatalf("RedactCmdArgs left the secret in %d bytes", len(got))
	}
	if want := `/bin/sh -c 'curl -H '\''Authorization: <redacted>'\'' -u me:<redacted> https://api.example.com && mysql -p<redacted>'`; got != want {
		t.Errorf("RedactCmdArgs = %q, want %q", got, want)
	}
}

// A quoted URL that does not parse is named by its size.
func TestShapeQuotedURLsWithholdsAnUnparsableURL(t *testing.T) {
	const s = logleak.Canary
	bad := strings.Repeat("%z", 1) + "z"
	got := ShapeQuotedURLs(`parse "http://h/reset/` + s + bad + `?token=` + s + `": invalid URL escape`)
	if logleak.Holds(got, s) || !strings.Contains(got, "unparsable URL") {
		t.Fatalf("ShapeQuotedURLs = %d bytes, want the URL named by its size", len(got))
	}
}

// An argv keploy logs (a re-exec's, a signal handler's) withholds a JDBC
// URL's host that is no list of host[:port], as a command line does; a
// quoted one in an error is shaped the same way.
func TestRedactArgvWithholdsAHostThatIsNoHostList(t *testing.T) {
	const s = logleak.Canary
	for _, url := range []string{
		"jdbc:sqlserver://db;user=sa;password=" + s,
		"jdbc:sqlserver://localhost;encrypt=true;user=sa;password=" + s,
		"jdbc:mysql://(host=db,port=3306,user=root,password=" + s + ")/shop",
		"jdbc:mysql://address=(host=db)(port=3306)(user=root)(password=" + s + ")/shop",
	} {
		for _, argv := range [][]string{
			{"java", "-jar", "app.jar", "--spring.datasource.url=" + url},
			{"java", "-Dspring.datasource.url=" + url, "-jar", "app.jar"},
			{"java", "-jar", "app.jar", url},
		} {
			if got := RedactCmdArgs(argv); logleak.Holds(got, s) || !strings.Contains(got, "(host withheld, ") {
				t.Errorf("RedactCmdArgs = %q, want the host withheld by its size", got)
			}
		}
		if got := ShapeQuotedURLs(`dial "` + url + `": refused`); logleak.Holds(got, s) {
			t.Errorf("ShapeQuotedURLs = %q, want the host withheld", got)
		}
	}
}
