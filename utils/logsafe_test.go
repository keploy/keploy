package utils

import (
	"bufio"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
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

// A URL is named by its shape: its method, its host's labels' shape (a name
// is the app's), its scheme, its path's segments counted and its query's
// names, never a value, a segment, a name or userinfo.
func TestURLLogMetaNamesTheShapeOnly(t *testing.T) {
	const s = logleak.Canary
	meta := URLLogMeta("GET", "https://app:"+s+"@"+s+".example.com/v1/reset/"+s+"?access_token="+s+"&lang=en")
	want := map[string]string{"method": "GET", "host": "{}.{}.{}", "scheme": "https", "path": "/{}/{}/{}", "query keys": "access_token,lang"}
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
	// No path tag, of a method or of none (a test's target as keploy
	// resolves it): keploy's CLI names a test case's URL so, and no console
	// or report line prints a tag of it. GetReqMeta, for the agent's lines,
	// names the tag the console prints beside the call.
	for _, method := range []string{"GET", ""} {
		if meta := URLLogMeta(method, "http://app:8080/v1/reset/"+s); meta["path"] != "/{}/{}/{}" {
			t.Errorf("a URL of method %q is named %v, want its shape", method, meta)
		} else if tag, ok := meta["path_tag"]; ok {
			t.Errorf("a URL of method %q has path tag %q, want none", method, tag)
		}
	}
	req, _ := http.NewRequest("GET", "http://app:8080/v1/reset/"+s, nil)
	if shape, meta := GetReqShape(req), GetReqMeta(context.Background(), req); shape["path_tag"] != "" || meta["path_tag"] != models.PathTag(context.Background(), "GET", "/v1/reset/"+s) {
		t.Errorf("GetReqShape = %v, GetReqMeta = %v; want the tag in GetReqMeta's alone", shape, meta)
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
		{"./app --name\u00a0x --token " + s, "./app '--name\u00a0x' --token <redacted>"},
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

// A quoted URL or path is written by its shape, its host's name by its
// labels' shape (an address whole); keploy's own quoted words stay.
func TestShapeQuotedURLsKeepsKeploysWords(t *testing.T) {
	const s = logleak.Canary
	got := ShapeQuotedURLs(`Post "http://app:8080/v1/reset/` + s + `?token=` + s + `": EOF; header "Content-Encoding" is "gzip"; path "/v1/me/` + s + `"; dial "https://` + s + `.example.com:443/x": refused; "http://127.0.0.1:9/p"`)
	if logleak.Holds(got, s) {
		t.Fatalf("ShapeQuotedURLs left the secret in %d bytes", len(got))
	}
	for _, want := range []string{`"http://{}:8080/{}/{}/{}?{token}"`, `"Content-Encoding"`, `"gzip"`, `"/{}/{}/{}"`, `"https://{}.{}.{}:443/{}"`, `"http://127.0.0.1:9/{}"`} {
		if !strings.Contains(got, want) {
			t.Errorf("ShapeQuotedURLs = %q, want it to hold %s", got, want)
		}
	}
	// No path tag: the report keeps a client's error with no tag beside its
	// URL, so no reader could find one.
	if strings.Contains(got, "path tag") {
		t.Errorf("ShapeQuotedURLs = %q, want no path tag", got)
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

// SayWhole names a reason whole on a console only where its log line named
// it otherwise (a host by its shape and tag): never twice, never to a nil
// console.
func TestSayWholeNamesWhatTheLogShapedOnly(t *testing.T) {
	var console strings.Builder
	SayWhole(&console, "test set s", "to {}:443 #3fa2c1d0e9b8", "to orders-svc:443 [host tag #3fa2c1d0e9b8]")
	if want := "test set s, which keploy's log names by its host tags: to orders-svc:443 [host tag #3fa2c1d0e9b8]\n"; !strings.HasSuffix(console.String(), want) {
		t.Fatalf("console = %q, want it to end %q", console.String(), want)
	}
	// An older agent's text, which the log names by its size, is said so.
	console.Reset()
	SayWhole(&console, "test set s", "(withheld, 17 bytes: "+models.UnlistedLogNote+")", "to orders-svc:443")
	if want := "test set s, which keploy's log names by its size, as the agent did not list its host names: to orders-svc:443\n"; !strings.HasSuffix(console.String(), want) {
		t.Fatalf("console = %q, want it to end %q", console.String(), want)
	}
	console.Reset()
	SayWhole(&console, "test set s", "to {}:443 #3fa2c1d0e9b8; (withheld, 17 bytes: "+models.UnlistedLogNote+")", "to orders-svc:443 [host tag #3fa2c1d0e9b8]; to pay-svc:443")
	if want := "test set s, which keploy's log names by its host tags, and where the agent did not list them by its size: "; !strings.Contains(console.String(), want) {
		t.Fatalf("console = %q, want it to say %q", console.String(), want)
	}
	// A text whose hosts the agent listed, at places it does not hold, is
	// said so: the agent did list them.
	console.Reset()
	SayWhole(&console, "test set s", "(withheld, 17 bytes: "+models.MisplacedLogNote+")", "to orders-svc:443")
	if want := "test set s, which keploy's log names by its size, as the agent listed its host names at places the text does not hold: to orders-svc:443\n"; !strings.HasSuffix(console.String(), want) {
		t.Fatalf("console = %q, want it to end %q", console.String(), want)
	}
	console.Reset()
	SayWhole(&console, "test set s", "to {}:443 #3fa2c1d0e9b8; (withheld, 17 bytes: "+models.MisplacedLogNote+")", "to orders-svc:443 [host tag #3fa2c1d0e9b8]; to pay-svc:443")
	if want := "test set s, which keploy's log names by its host tags, and where the agent listed them at places its text does not hold, by its size: "; !strings.Contains(console.String(), want) {
		t.Fatalf("console = %q, want it to say %q", console.String(), want)
	}
	console.Reset()
	SayWhole(&console, "test set s", "to {}:443 #3fa2c1d0e9b8; (withheld, 17 bytes: "+models.UnlistedLogNote+"); (withheld, 9 bytes: "+models.MisplacedLogNote+")", "to orders-svc:443 [host tag #3fa2c1d0e9b8]; to pay-svc:443; to db")
	if want := "test set s, which keploy's log names by its host tags, and where the agent did not list them, or listed them at places its text does not hold, by its size: "; !strings.Contains(console.String(), want) {
		t.Fatalf("console = %q, want it to say %q", console.String(), want)
	}
	// An IP address whose port is no number has no tag: the log names it
	// by its shape.
	console.Reset()
	SayWhole(&console, "test set s", "a call to 10.0.0.1:{}", "a call to 10.0.0.1:s3cr3t")
	if want := "test set s, which keploy's log names by its hosts' shapes: a call to 10.0.0.1:s3cr3t\n"; !strings.HasSuffix(console.String(), want) {
		t.Fatalf("console = %q, want it to end %q", console.String(), want)
	}
	console.Reset()
	SayWhole(&console, "test set s", "to 10.0.0.1:443", "to 10.0.0.1:443")
	SayWhole(&console, "test set s", "", "")
	if console.Len() != 0 {
		t.Fatalf("a reason its log named whole was said again: %q", console.String())
	}
	SayWhole(nil, "test set s", "a", "b") // no console: nothing, and no panic
}

// The command line of a process keploy saw make a call is the application's
// traffic, and is logged by its shape: each word, but for its program, its
// flags' names and its short numbers, by its shape and its tag under the
// session, its secrets redacted with no tag. So no form names a host, a
// path, a user or a secret, whether or not a rule could read it as one. It
// is read from the process's words, never a line joined from them: a quote
// or a space in the program's path, an empty argv[0], a process's own title
// and an argument with a space in it each stay one word, and argv[0] is
// kept by its base name only when it is a program's path.
func TestRedactProcessArgvWritesItsWordsByShapeAndTag(t *testing.T) {
	const s = logleak.Canary
	keys := models.NewLogKeySession()
	tag := keys.LogKey
	const url, title = "http://dep.keploy.test:18999", "ssh: /home/u/.ssh/cm-app@db.keploy.test:22 [mux]"
	for _, tc := range []struct {
		name, cmd string
		argv      []string // the words, when cmd does not split into them
		want      string
	}{
		{"the program and a URL", "/usr/bin/python3 client.py http://dep.keploy.test:18999", nil,
			"python3 '{}.{} " + tag("client.py") + "' '{}://{}.{}.{}:{} " + tag("http://dep.keploy.test:18999") + "'"},
		{"a flag's value after '='", "./client --target=dep.keploy.test:18999/v1?token=" + s + " --retries 3 -v", nil,
			"client '--target={}.{}.{}:{}/{}?{}=<redacted> " + tag("dep.keploy.test:18999/v1?token=<redacted>") + "' --retries 3 -v"},
		{"a short flag's value in its word", "mysql -hdb.keploy.test -uroot -p" + s + " shop", nil,
			"mysql '-h{}.{}.{} " + tag("db.keploy.test") + "' '-u{} " + tag("root") + "' -p<redacted> '{} " + tag("shop") + "'"},
		{"a secret beside a host", "./worker --db-host db.keploy.test --db-password " + s, nil,
			"worker --db-host '{}.{}.{} " + tag("db.keploy.test") + "' --db-password <redacted>"},
		{"an environment's assignment", "env DB_URL=postgres://app:" + s + "@db.keploy.test/shop ./app", nil,
			"env '{}_{}=<redacted> " + tag("DB_URL=<redacted>") + "' './{} " + tag("./app") + "'"},
		{"numbers", "./client 5432 18999 123456 4111111111111111", nil,
			"client 5432 18999 '{} " + tag("123456") + "' '{} " + tag("4111111111111111") + "'"},
		{"single-dash names", "consul agent -server -bootstrap-expect 1", nil,
			"consul '{} " + tag("agent") + "' '-s{} " + tag("erver") + "' '-b{}-{} " + tag("ootstrap-expect") + "' 1"},
		{"a property's value", "java -Dsvc.url=dep.keploy.test:18999/sekritpath -jar app.jar", nil,
			"java '-Dsvc.url={}.{}.{}:{}/{} " + tag("dep.keploy.test:18999/sekritpath") + "' '-j{} " + tag("ar") + "' '{}.{} " + tag("app.jar") + "'"},
		{"a tunnel's bind and target", "ssh -L localhost:5432:db.keploy.test:5432 app@bastion.keploy.test", nil,
			"ssh -L '{}:{}:{}.{}.{}:{} " + tag("localhost:5432:db.keploy.test:5432") + "' '{}@{}.{}.{} " + tag("app@bastion.keploy.test") + "'"},
		{"a shell's command", "bash -c 'until </dev/tcp/db.keploy.test/5432; do sleep 1; done'", nil,
			"bash -c '{} </{}/{}/{}.{}.{}/{}; {} {} {}; {} " + tag("until </dev/tcp/db.keploy.test/5432; do sleep 1; done") + "'"},
		{"an IP address", "./client 10.0.0.7:8080", nil,
			"client '{}.{}.{}.{}:{} " + tag("10.0.0.7:8080") + "'"},
		{"a word past ASCII", "./client zürich.keploy.test", nil,
			"client '{}.{}.{} " + tag("zürich.keploy.test") + "'"},
		{"words with no letter or digit", "./client - -- &&", nil, "client - -- &&"},
		// RedactArgv reads a word with a space as a command line, so the
		// apostrophe of "Bob's" is read as a quote there; the word is
		// written by its shape all the same.
		{"an apostrophe in the program's path", "", []string{"/home/dev/Bob's app/bin/python3", "client.py", url},
			`''\''/{}/{}/{} {}/{}/{}'\'' ` + tag("'/home/dev/Bobs app/bin/python3'") + "' '{}.{} " + tag("client.py") + "' '{}://{}.{}.{}:{} " + tag(url) + "'"},
		{"an apostrophe in the program's name", "", []string{"/opt/bin/o'app", "--host", "db.keploy.test"},
			`'/{}/{}/{}'\''{} ` + tag("/opt/bin/o'app") + "' --host '{}.{}.{} " + tag("db.keploy.test") + "'"},
		{"a double quote in the program's path", "", []string{`/opt/a"b/run`, "--host", "db.keploy.test"},
			`'/{}/{}"{}/{} ` + tag(`/opt/a"b/run`) + "' --host '{}.{}.{} " + tag("db.keploy.test") + "'"},
		{"a backtick in the program's path", "", []string{"/opt/a`b/run", "--host", "db.keploy.test"},
			"'/{}/{}`{}/{} " + tag("/opt/a`b/run") + "' --host '{}.{}.{} " + tag("db.keploy.test") + "'"},
		{"a no-break space in the program's path", "", []string{"/opt/my\u00a0app/bin/python3", "--host", "db.keploy.test"},
			"'/{}/{}/{}/{} " + tag("/opt/my\u00a0app/bin/python3") + "' --host '{}.{}.{} " + tag("db.keploy.test") + "'"},
		{"an ideographic space in the program's path", "", []string{"/opt/my\u3000app/bin/python3", "--host", "db.keploy.test"},
			"'/{}/{}/{}/{} " + tag("/opt/my\u3000app/bin/python3") + "' --host '{}.{}.{} " + tag("db.keploy.test") + "'"},
		{"an empty argv[0]", "", []string{"", "dep.keploy.test:18999"},
			"'' '{}.{}.{}:{} " + tag("dep.keploy.test:18999") + "'"},
		{"a process's own title", "", []string{title},
			"'{}: /{}/{}/.{}/{}-{}@{}.{}.{}:{} [{}] " + tag(title) + "'"},
		{"a space before the program's word", "", []string{" dep.keploy.test:18999", "x"},
			"'{}.{}.{}:{} " + tag("dep.keploy.test:18999") + "' '{} " + tag("x") + "'"},
		{"a space after the program's word", "", []string{"dep.keploy.test:18999 "},
			"'{}.{}.{}:{} " + tag("dep.keploy.test:18999") + "'"},
		{"quotes and a space around the program's word", "", []string{"'dep.keploy.test' ", "x"},
			"'{}.{}.{} " + tag("dep.keploy.test") + "' '{} " + tag("x") + "'"},
		{"an argument with a space", "", []string{"python3", "client.py", "my file.txt"},
			"python3 '{}.{} " + tag("client.py") + "' '{} {}.{} " + tag("my file.txt") + "'"},
	} {
		argv := tc.argv
		if argv == nil {
			argv = splitCommandLine(tc.cmd)
		}
		got := RedactProcessArgv(keys, argv)
		if got != tc.want {
			t.Errorf("%s: RedactProcessArgv(%q)\n got %q\nwant %q", tc.name, argv, got, tc.want)
		}
		if strings.Contains(got, "keploy.test") || logleak.Holds(got, s) {
			t.Errorf("%s: %q names a host or a secret whole", tc.name, got)
		}
	}
	// The user's own command line is logged whole but for its secrets: the
	// user wrote it.
	if got := RedactCommandLine("python3 client.py http://dep.keploy.test:18999"); got != "python3 client.py http://dep.keploy.test:18999" {
		t.Errorf("RedactCommandLine = %q, want the user's own command line kept", got)
	}
	// Another session tags a word otherwise; one session, alike each time.
	argv := []string{"./client", "db.keploy.test:5432"}
	if a, b := RedactProcessArgv(keys, argv), RedactProcessArgv(models.NewLogKeySession(), argv); a == b {
		t.Errorf("two sessions wrote %q alike", a)
	}
	if a, b := RedactProcessArgv(keys, argv), RedactProcessArgv(keys, argv); a != b {
		t.Errorf("one session wrote one command line two ways: %q, %q", a, b)
	}
}

// processLineKept is what a process's logged command line may keep of a
// word, by RedactProcessArgv's rules, stated again here: a flag's name
// (--name, a name up to its '=', the -X of any other '-' word), and a
// number of one to five digits.
var (
	processLineFlag   = regexp.MustCompile(`^(?:--?[A-Za-z0-9][A-Za-z0-9._-]*=|--[A-Za-z0-9][A-Za-z0-9._-]*$|-[A-Za-z0-9])`)
	processLineNumber = regexp.MustCompile(`^[0-9]{1,5}$`)
	processLineTag    = regexp.MustCompile(` #[0-9a-f]{12}$`)
)

// In every form a process's command line names its traffic in (a URL with or
// without a scheme, a host:port, a host list, a DSN, a remote, a tunnel, a
// container's command, a shell's), the logged line names none of it: no
// host, path, query's value, user or password planted in it is left, and no
// letter or digit is, but in its program, its flags' names and its short
// numbers, which stay readable.
func TestRedactProcessArgvLeavesNoTrafficInAnyForm(t *testing.T) {
	planted := []string{"keploy.test", "kdbhost", "kproxy", "kbroker1", "kbroker2", "kgitea", "sekritpath", "s3cr3t", "qv4lue"}
	alnum := func(r rune) bool {
		return r >= 0x80 || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
	}
	keys := models.NewLogKeySession()
	for _, tc := range []struct{ id, cmd string }{
		{"L01", "curl dep.keploy.test:18999/v1/sekritpath?token=QV4LUE"},
		{"L02", "./client --url=dep.keploy.test:18999/v1/sekritpath?token=QV4LUE"},
		{"L03", "./client --url=dep.keploy.test:18999?token=QV4LUE"},
		{"L04", "java -Dsvc.url=dep.keploy.test:18999/sekritpath?x=QV4LUE -jar app.jar"},
		{"L05", "./client --target=kdbhost:8080/sekritpath?a=b"},
		{"L06", "curl user:S3CR3T@dep.keploy.test:18999/sekritpath"},
		{"L07", "curl app@dep.keploy.test:18999/sekritpath"},
		{"L08", "java -jar app.jar --spring.datasource.url=jdbc:mysql://db.keploy.test:3306/shop?password=S3CR3T"},
		{"L09", "mongosh mongodb+srv://app:S3CR3T@cluster0.keploy.test/shop?retryWrites=true"},
		{"L10", "./worker amqp://app@rabbit.keploy.test"},
		{"L11", "./worker amqp://app:S3CR3T@rabbit.keploy.test:5672/vh"},
		{"L12", "redis-cli -h kdbhost -p 6379"},
		{"L13", "redis-cli -h redis.keploy.test -p 6379 -a S3CR3T"},
		{"L14", `psql "host=db.keploy.test port=5432 password=S3CR3T"`},
		{"L15", "psql host=db.keploy.test port=5432"},
		{"L16", "kafka-console-producer --bootstrap-server kbroker1:9092,kbroker2:9093 --topic t"},
		{"L17", "kafka-console-producer --broker-list kbroker1.keploy.test:9092,kbroker2.keploy.test:9093"},
		{"L18", "curl -x kproxy:3128 http://10.0.0.1/"},
		{"L19", "curl --proxy proxy.keploy.test:3128 http://10.0.0.1/"},
		{"L20", "curl -x proxy.keploy.test http://10.0.0.1/"},
		{"L21", "curl http://[fe80::1%25eth0]:8080/v1/sekritpath"},
		{"L22", "./client [fe80::1%eth0]:8080"},
		{"L23", "./client --host=db.keploy.test"},
		{"L24", "./client --host db.keploy.test"},
		{"L25", "./client --host=kdbhost"},
		{"L26", "./client --host kdbhost"},
		{"L27", "curl --resolve dep.keploy.test:443:10.0.0.7 https://10.0.0.7/"},
		{"L28", "curl --connect-to dep.keploy.test:443:db.keploy.test:8443 https://10.0.0.1/"},
		{"L29", "ssh -L 5432:db.keploy.test:5432 -N app@bastion.keploy.test"},
		{"L30", "ssh -L localhost:5432:db.keploy.test:5432 app@bastion.keploy.test"},
		{"L31", "docker run --add-host dep.keploy.test:10.0.0.7 img"},
		{"L32", "docker run --add-host=dep.keploy.test:host-gateway img"},
		{"L33", "docker run --add-host dep.keploy.test=10.0.0.7 img"},
		{"L34", "wp db check --dbhost=db.keploy.test:3306"},
		{"L35", "./app --redishost redis.keploy.test"},
		{"L36", "ssh app@db.keploy.test"},
		{"L37", "scp f app@db.keploy.test:/tmp/x"},
		{"L38", "rsync -av ./d/ db.keploy.test:/srv/"},
		{"L39", "rsync -av ./d/ db.keploy.test::module/"},
		{"L40", "git clone git@git.keploy.test:org/repo.git"},
		{"L41", "git clone git@kgitea:org/repo.git"},
		{"L42", "mysql -hdb.keploy.test -uroot"},
		{"L43", "mysql -hkdbhost -uroot"},
		{"L44", "./migrate root@tcp(db.keploy.test:3306)/shop"},
		{"L45", "./migrate tcp(db.keploy.test:3306)/shop"},
		{"L46", "./migrate app:S3CR3T@tcp(db.keploy.test:3306)/shop"},
		{"L47", "./app --database=root@tcp(db.keploy.test:3306)/shop?parseTime=true"},
		{"L48", "./app --dsn=root@tcp(db.keploy.test:3306)/shop"},
		{"L49", "./migrate root@tcp(kdbhost:3306)/shop"},
		{"L50", "./migrate root@(db.keploy.test:3306)/shop"},
		{"L51", "./migrate root@tcp([fe80::1%eth0]:3306)/shop"},
		{"L52", `sh -c "wait-for-it db.keploy.test:5432; ./app"`},
		{"L53", `sh -c "curl -sf dep.keploy.test:18999/health&&./app"`},
		{"L54", `sh -c "curl -sf dep.keploy.test:18999||exit 1"`},
		{"L55", "sqlcmd -S db.keploy.test,1433 -U sa"},
		{"L56", "openssl s_client -connect db.keploy.test:443 -servername db.keploy.test"},
		{"L57", "mongo --host rs0/db1.keploy.test:27017,db2.keploy.test:27017"},
		{"L58", "mongo db.keploy.test:27017/shop"},
		{"L59", "mongodump -h db.keploy.test:27017 -d shop"},
		{"L60", "mongodump --uri=mongodb://app:S3CR3T@db.keploy.test/shop"},
		{"L61", "./client -Ddb.host=db.keploy.test"},
		{"L62", "./client -Dhttp.proxyHost=proxy.keploy.test"},
		{"L63", "./client --spring.redis.host=redis.keploy.test"},
		{"L64", "./client --zookeeper zk.keploy.test:2181/kafka"},
		{"L65", "etcdctl --endpoints=etcd1.keploy.test:2379,etcd2.keploy.test:2379 get k"},
		{"L66", "socat - TCP:db.keploy.test:5432"},
		{"L67", "grpcurl -plaintext dep.keploy.test:50051 list"},
		{"L68", "http dep.keploy.test:18999/v1/sekritpath token==QV4LUE"},
		{"L69", "wget -qO- dep.keploy.test:18999/sekritpath"},
		{"L70", "docker -H db.keploy.test:2375 ps"},
		{"L71", "docker run --rm img curl dep.keploy.test:18999/sekritpath"},
		{"L72", "docker run -p8080:80 img dep.keploy.test:5432"},
		{"L73", "docker run --use-api-socket img dep.keploy.test:5432"},
		{"L74", "kubectl run x --image=registry.keploy.test:5000/app:1"},
		{"L75", "docker pull registry.keploy.test/app:1"},
		{"L76", "docker build -t registry.keploy.test:5000/app:1 ."},
		{"L77", `curl -H "Host: api.keploy.test" http://10.0.0.1/`},
		{"L78", "curl --noproxy db.keploy.test http://10.0.0.1/"},
		{"L79", "ssh -J app@bastion.keploy.test app@db.keploy.test"},
		{"L80", `ssh -o ProxyCommand="ssh -W %h:%p app@bastion.keploy.test" app@db.keploy.test`},
		{"L81", "nc -zv db.keploy.test 5432"},
		{"L82", "./client DB.KEPLOY.TEST:5432"},
		{"L83", "./client db.keploy.test.:5432"},
		{"L84", "./client --hosts=a.keploy.test,b.keploy.test"},
		{"L85", "./client --peer=kbroker1:9092,kbroker2"},
		{"L86", "./client kbroker1,kbroker2:9092"},
		{"L87", "./client --url dep.keploy.test:18999/v1/sekritpath#frag"},
		{"L88", "./client dep.keploy.test:18999#frag"},
		{"L89", "curl 'dep.keploy.test:18999/v1/x?a=1&b=QV4LUE'"},
		{"L90", "./client --target=[::1]:8080/sekritpath?x=QV4LUE"},
		{"L91", "curl 10.0.0.1:8080/sekritpath"},
		{"L92", "./client --endpoint=10.0.0.1:8080/sekritpath?x=1"},
		{"L93", "curl --url dep.keploy.test:18999/sekritpath"},
		{"L94", "./client --webhook=hooks.keploy.test:443/notify/sekritpath?token=QV4LUE"},
		{"L95", "env URL=dep.keploy.test:18999 ./app"},
		{"L96", "docker run -e DB_HOST=db.keploy.test img"},
		{"L97", "python app.py --db db.keploy.test:5432/shop"},
		{"L98", "./app --db=db.keploy.test:5432/shop"},
		{"L99", "./app --db=db.keploy.test:5432/shop?sslmode=disable"},
		{"L100", "./app -Ddb=db.keploy.test:5432/shop?sslmode=disable"},
		{"L101", "./client user@dep.keploy.test:18999"},
		{"L102", "nats-server --routes=nats://kbroker1:6222"},
		{"L103", "./tool -connect=db.keploy.test:443"},
		{"L104", "./tool --server=db.keploy.test:8200/v1?x=QV4LUE"},
		{"L105", "ssh -p 22 app@kdbhost"},
		{"L106", "scp kdbhost:/tmp/x ."},
		{"L107", "./client db.keploy.test:5432,"},
		{"L108", `sh -c "(wait-for db.keploy.test:5432)"`},
		{"L109", `sh -c "dockerize -wait tcp://db.keploy.test:5432 -timeout 60s"`},
		{"L110", "./client --dbhost=db.keploy.test:3306"},
		{"L111", "./client --url=dep.keploy.test:18999/v1/sekritpath"},
		{"L112", "./client --callback=dep.keploy.test:18999/sekritpath?a=1"},
		{"L113", "curl -sS dep.keploy.test:18999/v1/sekritpath -o out.json"},
		{"L114", "./client --url=user:S3CR3T@dep.keploy.test:18999/sekritpath"},
		{"L115", "./client --origin=kdbhost:8080?x=QV4LUE"},
		{"L116", "./client -Dservice.endpoint.url=dep.keploy.test:443/sekritpath?k=v"},
		{"L117", "./client --upstream=dep.keploy.test:18999/a=b/sekritpath"},
		{"L118", "redis-cli -u redis://default:S3CR3T@redis.keploy.test:6379/0"},
		{"L119", "./client dep.keploy.test:18999/v1/sekritpath;x=1"},
		{"L120", "./client --target=dep.keploy.test:18999/v1/sekritpath;x=1"},
		{"M01", "npm install lodash@4.17.21"},
		{"M02", "npm install @types/node@20.1.0"},
		{"M03", "git clone git@github.com:org/repo.git"},
		{"M04", "docker run node:20-alpine"},
		{"M05", "docker run node:20"},
		{"M06", "./tool node:20"},
		{"M07", "docker build -t app:7 ."},
		{"M08", "./dev localhost:3000"},
		{"M09", `cmd /C C:\app\srv.exe --config C:\cfg\a.yaml`},
		{"M10", `./tool C:\path\file.txt`},
		{"M11", `scp C:\file.txt app@db.keploy.test:/tmp/`},
		{"M12", "chown app:staff /srv"},
		{"M13", "chown www-data:www-data /srv"},
		{"M14", "./tool --at 10:30"},
		{"M15", "./tool T10:30"},
		{"M16", "./tool KEY:value"},
		{"M17", "./tool --format=json"},
		{"M18", "kubectl -n ns get svc ns:service"},
		{"M19", "redis-cli GET user:1000"},
		{"M20", "uvicorn app.main:app --reload"},
		{"M21", "gunicorn -b unix:/tmp/app.sock app:app"},
		{"M22", "java -cp lib/*:app.jar com.example.Main"},
		{"M23", "java -server com.example.Main"},
		{"M24", "go test -run TestX ./pkg/..."},
		{"M25", "go install golang.org/x/tools/gopls@latest"},
		{"M26", "pytest tests/test_a.py::TestX::test_y"},
		{"M27", "python -W ignore::DeprecationWarning app.py"},
		{"M28", "socat TCP-LISTEN:8080,fork TCP:db.keploy.test:5432"},
		{"M29", "git show HEAD:src/main.go"},
		{"M30", "git log -L 10,20:file.go"},
		{"M31", "ffmpeg -ss 00:01:30 -i in.mp4 out.mp4"},
		{"M32", "docker run -p 127.0.0.1:8080:80 img"},
		{"M33", "./tool step:2 phase:3"},
		{"M34", `echo "listening on port:8080"`},
		{"M35", `ssh app@db.keploy.test "tail -f /var/log/app.log"`},
		{"M36", "rsync --chown=www-data:www-data -av src/ dst/"},
		{"M37", "docker run --user=app:1000 img"},
		{"M38", "mongo --host rs0/db1.keploy.test:27017"},
		{"M39", "./tool --known-hosts=known_hosts"},
		{"M40", "kind load docker-image redis:7"},
		{"M41", "git config user.email alice@example.com"},
		{"M42", "./tool mailto:alice@example.com"},
		{"M43", "./tool --localhost --port 3000"},
		{"M44", "./tool sha256:12345"},
		{"M45", "vite --host --port 3000"},
		{"M46", "java -server -Xmx1g -jar app.jar"},
		{"M47", "./tool main.go:12"},
		{"M48", "ssh fe80::1%eth0"},
		{"M49", "docker compose run app"},
		{"M50", "./tool --labels app,tier:1"},
		{"M51", "./tool a:b c:d e@f"},
		{"M52", "npm i pkg@npm:other@1.2.3"},
		{"M53", "./tool --listen=:8080"},
		{"M54", "./tool -http=localhost:6060"},
		{"M55", "python manage.py runserver 0.0.0.0:8000"},
		{"M56", "./tool --timeout=30s --retries=3"},
		{"M57", "docker run -v data:/data:ro img"},
		{"M58", "./tool user@laptop"},
		{"M59", "git push origin main:main"},
		{"M60", "kubectl port-forward svc/db 5432:5432 --address 0.0.0.0"},
	} {
		argv := splitCommandLine(tc.cmd)
		got := RedactProcessArgv(keys, argv)
		for _, p := range planted {
			if strings.Contains(strings.ToLower(got), p) {
				t.Errorf("%s: %q leaves %q", tc.id, got, p)
			}
		}
		in, out := RedactArgv(argv), splitCommandLine(got)
		if len(out) != len(in) {
			t.Errorf("%s: %q has %d words; want %d", tc.id, got, len(out), len(in))
			continue
		}
		// The program: argv[0]'s base name, when argv[0] is a program's
		// path (no white space or quote); else its shape.
		if program := in[0][strings.LastIndexAny(in[0], `/\`)+1:]; !strings.ContainsFunc(argv[0], unicode.IsSpace) && !strings.ContainsAny(argv[0], "'\"`") {
			if out[0] != program {
				t.Errorf("%s: %q names its program %q; want %q", tc.id, got, out[0], program)
			}
		} else if strings.ContainsFunc(processLineTag.ReplaceAllString(out[0], ""), alnum) {
			t.Errorf("%s: %q leaves its title's text: %q", tc.id, got, out[0])
		}
		for i := 1; i < len(out); i++ {
			left := strings.ReplaceAll(processLineTag.ReplaceAllString(out[i], ""), RedactedValue, "")
			switch name := processLineFlag.FindString(in[i]); {
			case name != "":
				if !strings.HasPrefix(out[i], name) {
					t.Errorf("%s: %q writes the flag %q as %q", tc.id, got, name, out[i])
				}
				left = strings.TrimPrefix(left, name)
			case processLineNumber.MatchString(in[i]):
				if out[i] != in[i] {
					t.Errorf("%s: %q writes the number %q as %q", tc.id, got, in[i], out[i])
				}
				left = ""
			}
			if strings.ContainsFunc(left, alnum) {
				t.Errorf("%s: %q leaves word %d's text: %q", tc.id, got, i, out[i])
			}
		}
	}
}

// The command the user gave keploy is logged whole but for its secrets
// ("Starting Application", the config's command): a userinfo's password is
// one with no scheme too, as curl, a proxy flag, a DSN or a Redis address
// takes it (user:password@host, -x user:password@proxy:3128,
// --db=app:password@db/shop, :password@redis), in an argument or a flag's
// value, inside a shell's command too. Its host and path stay, as the
// user wrote them. A user with no password, a package's version, an
// image's digest, npm's alias of a version and a mail address hold none,
// and stay whole.
func TestRedactCommandLineRedactsAPasswordWithNoScheme(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct{ cmd, want string }{
		{"curl user:S3CR3T@dep.keploy.test:18999/sekritpath", "curl user:<redacted>@dep.keploy.test:18999/sekritpath"},
		{"./client --url=user:S3CR3T@dep.keploy.test:18999/sekritpath", "./client --url=user:<redacted>@dep.keploy.test:18999/sekritpath"},
		{"curl -x user:S3CR3T@proxy.keploy.test:3128 10.0.0.1", "curl -x user:<redacted>@proxy.keploy.test:3128 10.0.0.1"},
		{"curl -xuser:S3CR3T@proxy.keploy.test:3128 10.0.0.1", "curl -xuser:<redacted>@proxy.keploy.test:3128 10.0.0.1"},
		{"curl --proxy user:S3CR3T@proxy.keploy.test:3128 10.0.0.1", "curl --proxy user:<redacted>@proxy.keploy.test:3128 10.0.0.1"},
		{"curl --proxy=user:S3CR3T@proxy.keploy.test:3128 10.0.0.1", "curl --proxy=user:<redacted>@proxy.keploy.test:3128 10.0.0.1"},
		{"./app --db=app:S3CR3T@db.keploy.test:5432/shop", "./app --db=app:<redacted>@db.keploy.test:5432/shop"},
		{"./app --db=app:S3CR3T@db.keploy.test:5432/shop?sslmode=disable", "./app --db=app:<redacted>@db.keploy.test:5432/shop?sslmode=disable"},
		{"./app app:S3CR3T@db.keploy.test:5432/shop", "./app app:<redacted>@db.keploy.test:5432/shop"},
		{"./app :S3CR3T@redis.keploy.test:6379", "./app :<redacted>@redis.keploy.test:6379"},
		{"./app app:S3:CR@3T@db.keploy.test/shop", "./app app:<redacted>@db.keploy.test/shop"},
		{"./app app:S3/CR3T@db.keploy.test/shop", "./app app:<redacted>@db.keploy.test/shop"},
		{"./app //app:S3CR3T@db.keploy.test/shop", "./app //app:<redacted>@db.keploy.test/shop"},
		{"./app --brokers=a:S3CR3T@k1.keploy.test:9092,b:S3CR3T@k2.keploy.test:9092", "./app --brokers=a:<redacted>@k2.keploy.test:9092"},
		{"java -Dsvc.url=user:S3CR3T@dep.keploy.test/v1 -jar app.jar", "java -Dsvc.url=user:<redacted>@dep.keploy.test/v1 -jar app.jar"},
		{"./app npm:S3CR3T@registry.keploy.test", "./app npm:<redacted>@registry.keploy.test"},
		{"./app npm:S3CR3T@10.0.0.5", "./app npm:<redacted>@10.0.0.5"},
		{`sh -c "curl -sf user:S3CR3T@dep.keploy.test:18999/health && ./app"`, `sh -c 'curl -sf user:<redacted>@dep.keploy.test:18999/health && ./app'`},
		// With a scheme: a URL, read as before (userinfo left out).
		{"curl http://user:S3CR3T@dep.keploy.test:18999/sekritpath", "curl http://dep.keploy.test:18999/{}"},
		{"./app --db=postgres://app:S3CR3T@db.keploy.test:5432/shop", "./app --db=postgres://db.keploy.test:5432/{}"},
		{"./app jdbc:mysql://app:S3CR3T@db.keploy.test:3306/shop", "./app jdbc:mysql://db.keploy.test:3306/{}"},
		{"./app redis://:S3CR3T@redis.keploy.test:6379", "./app redis://redis.keploy.test:6379"},
		// No password: whole.
		{"ssh app@db.keploy.test", "ssh app@db.keploy.test"},
		{"git clone git@github.com:org/repo.git", "git clone git@github.com:org/repo.git"},
		{"scp f app@db.keploy.test:/tmp/x", "scp f app@db.keploy.test:/tmp/x"},
		{"npm install lodash@4.17.21 @types/node@20.1.0", "npm install lodash@4.17.21 @types/node@20.1.0"},
		{"npm i pkg@npm:other@1.2.3", "npm i pkg@npm:other@1.2.3"},
		{"npx npm:other@^1.2.3", "npx npm:other@^1.2.3"},
		{"docker run nginx:1.25@" + digest, "docker run nginx:1.25@" + digest},
		{"go install golang.org/x/tools/gopls@latest", "go install golang.org/x/tools/gopls@latest"},
		{"./tool mailto:alice@example.com", "./tool mailto:alice@example.com"},
	} {
		if got := RedactCommandLine(tc.cmd); got != tc.want {
			t.Errorf("RedactCommandLine(%q)\n got %q\nwant %q", tc.cmd, got, tc.want)
		}
	}
	// "Starting Application" logs the command keploy runs (RedactCmdArgs):
	// the user's command, a shell's argument.
	argv := []string{"/bin/sh", "-c", "curl user:S3CR3T@dep.keploy.test:18999/sekritpath"}
	if got, want := RedactCmdArgs(argv), "/bin/sh -c 'curl user:<redacted>@dep.keploy.test:18999/sekritpath'"; got != want {
		t.Errorf("RedactCmdArgs(%q)\n got %q\nwant %q", argv, got, want)
	}
}

// Oracle's logon, user/password@db, keeps no password: as an Oracle client's
// argument (sqlplus, rman, dgmgrl), with an alias or with no '@' at the
// logon's place, and as any program's argument or flag's value when what
// follows its '@' is a database's address (//host, host:port, host/service,
// a connect descriptor). Words of that shape that are no logon stay whole: a
// Go module's path, a repository's owner/name@ref, an image's digest, npm's
// scope, a file's name. So do SQL Server's (-P), lftp's (-u user,pw) and
// Samba's (-U user%pw) passwords, and a client's attached password that
// holds '='.
func TestRedactCommandLineRedactsOraclesLogonAndClientsPasswords(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct{ cmd, want string }{
		// Oracle's logon with a database's address, for any program.
		{"./app --db=scott/S3CR3T@db.keploy.test:1521/XEPDB1", "./app --db=scott/<redacted>@db.keploy.test:1521/XEPDB1"},
		{"python app.py scott/S3CR3T@db.keploy.test/orclpdb", "python app.py scott/<redacted>@db.keploy.test/orclpdb"},
		{"python app.py scott/S3CR3T@//db.keploy.test:1521/ORCL", "python app.py scott/<redacted>@//db.keploy.test:1521/ORCL"},
		{"python app.py scott/S3CR3T@db.keploy.test:1521", "python app.py scott/<redacted>@db.keploy.test:1521"},
		{"./app C##ADMIN/S3CR3T@[::1]:1521/cdb", "./app C##ADMIN/<redacted>@[::1]:1521/cdb"},
		{"./app scott/S3:CR@3T@db.keploy.test:1521/XE", "./app scott/<redacted>@db.keploy.test:1521/XE"},
		{`./app 'scott/"S3/CR3T"@db.keploy.test:1521/XE'`, "./app scott/<redacted>@db.keploy.test:1521/XE"},
		{"./app 'app[hr]/S3CR3T@db.keploy.test:1521/XE'", "./app app[hr]/<redacted>@db.keploy.test:1521/XE"},
		{"./app scott/S3CR3T@(DESCRIPTION=(ADDRESS=(HOST=db.keploy.test)(PORT=1521)))", "./app scott/<redacted>@(DESCRIPTION=(ADDRESS=(HOST=db.keploy.test)(PORT=1521)))"},
		{"java -Ddb.logon=scott/S3CR3T@db.keploy.test:1521/XE -jar app.jar", "java -Ddb.logon=scott/<redacted>@db.keploy.test:1521/XE -jar app.jar"},
		{`sh -c "python app.py scott/S3CR3T@db.keploy.test/orclpdb && ./next"`, `sh -c 'python app.py scott/<redacted>@db.keploy.test/orclpdb && ./next'`},
		// An Oracle client's logon: an alias, no '@', or a spaced word.
		{"sqlplus scott/S3CR3T@//db.keploy.test:1521/ORCL", "sqlplus scott/<redacted>@//db.keploy.test:1521/ORCL"},
		{"sqlplus scott/S3CR3T@orcl", "sqlplus scott/<redacted>@orcl"},
		{"sqlplus -s scott/S3CR3T @report.sql", "sqlplus -s scott/<redacted> @report.sql"},
		{"/opt/oracle/bin/sqlplus scott/S3CR3T", "/opt/oracle/bin/sqlplus scott/<redacted>"},
		{`sqlplus "sys/S3CR3T@orcl as sysdba"`, "sqlplus 'sys/<redacted>@orcl as sysdba'"},
		{"rman target sys/S3CR3T catalog rman/S3CR3T@rcat", "rman target sys/<redacted> catalog rman/<redacted>@rcat"},
		{"dgmgrl sys/S3CR3T@db.keploy.test", "dgmgrl sys/<redacted>@db.keploy.test"},
		{"docker exec db sqlplus -L scott/S3CR3T@XEPDB1 @/tmp/x.sql", "docker exec db sqlplus -L scott/<redacted>@XEPDB1 @/tmp/x.sql"},
		{`sh -c "sqlplus -s scott/S3CR3T@orcl <<< 'select 1 from dual;'"`, `sh -c 'sqlplus -s scott/<redacted>@orcl <<< '\''select 1 from dual;'\'''`},
		// SQL Server's -P.
		{"sqlcmd -S db.keploy.test -U sa -P S3CR3T -Q 'SELECT 1'", "sqlcmd -S db.keploy.test -U sa -P <redacted> -Q 'SELECT 1'"},
		{"/opt/mssql-tools18/bin/sqlcmd -Usa -PS3CR3T", "/opt/mssql-tools18/bin/sqlcmd -Usa -P<redacted>"},
		{"sqlcmd -U sa -PS3=CR3T", "sqlcmd -U sa -P<redacted>"},
		{"bcp shop.dbo.orders out o.dat -S db.keploy.test -U sa -P S3CR3T -c", "bcp shop.dbo.orders out o.dat -S db.keploy.test -U sa -P <redacted> -c"},
		// lftp's -u user,password.
		{"lftp -u alice,S3CR3T sftp://files.keploy.test", "lftp -u alice,<redacted> sftp://files.keploy.test"},
		{"lftp --user alice,S3CR3T files.keploy.test", "lftp --user alice,<redacted> files.keploy.test"},
		{"lftp --user=alice,S3CR3T files.keploy.test", "lftp --user=alice,<redacted> files.keploy.test"},
		{"lftp -ualice,S3CR3T files.keploy.test", "lftp -ualice,<redacted> files.keploy.test"},
		{"lftp -u alice,S3,CR3T files.keploy.test", "lftp -u alice,<redacted> files.keploy.test"},
		{"lftp -e 'open -u alice,S3CR3T files.keploy.test; mirror'", "lftp -e 'open -u alice,<redacted> files.keploy.test; mirror'"},
		{`lftp -c "open -u alice,S3CR3T files.keploy.test; get a"`, "lftp -c 'open -u alice,<redacted> files.keploy.test; get a'"},
		// Samba's -U user%password, and smbclient's password after its share.
		{"smbclient //fs.keploy.test/share -U alice%S3CR3T -c ls", "smbclient //fs.keploy.test/share -U alice%<redacted> -c ls"},
		{`smbclient -U 'CORP\alice%S3CR3T' //fs.keploy.test/share`, `smbclient -U CORP\alice%<redacted> //fs.keploy.test/share`},
		{"smbclient --user=alice%S3CR3T -L fs.keploy.test", "smbclient --user=alice%<redacted> -L fs.keploy.test"},
		{"smbclient --user alice%S3CR3T -L fs.keploy.test", "smbclient --user alice%<redacted> -L fs.keploy.test"},
		{"smbclient -Ualice%S3CR3T //fs.keploy.test/share", "smbclient -Ualice%<redacted> //fs.keploy.test/share"},
		{"smbclient //fs.keploy.test/share S3CR3T -U alice", "smbclient //fs.keploy.test/share <redacted> -U alice"},
		{"rpcclient -U alice%S3CR3T fs.keploy.test", "rpcclient -U alice%<redacted> fs.keploy.test"},
		{"smbget -U alice%S3%CR3T smb://fs.keploy.test/share/a", "smbget -U alice%<redacted> smb://fs.keploy.test/{}/{}"},
		// A client's attached password that holds '='.
		{"mysql -uroot -pS3=CR3T shop", "mysql -uroot -p<redacted> shop"},
		{"curl -ualice:S3=CR3T https://api.keploy.test", "curl -ualice:<redacted> https://api.keploy.test"},
		// Oracle's logon in a quote a shell kept because it was escaped.
		{`expdp \"sys/S3CR3T@orcl as sysdba\" directory=d dumpfile=f.dmp`, `expdp '"sys/<redacted>@orcl' as 'sysdba"' directory=<redacted> dumpfile=<redacted>`},
		{`impdp \"sys/S3CR3T@orcl\" directory=d`, `impdp '"sys/<redacted>@orcl"' directory=<redacted>`},
		{`python app.py \"scott/S3CR3T@db.keploy.test:1521/XE\"`, `python app.py '"scott/<redacted>@db.keploy.test:1521/XE"'`},
		// smbclient's password after its share and its options.
		{"smbclient //fs.keploy.test/share -U alice S3CR3T", "smbclient //fs.keploy.test/share -U alice <redacted>"},
		{"smbclient //fs.keploy.test/share -W CORP --port 445 -m SMB3 S3CR3T", "smbclient //fs.keploy.test/share -W CORP --port 445 -m SMB3 <redacted>"},
		// git's -c setting, a header in it: its token too.
		{`git -c http.extraHeader="Authorization: Bearer S3CR3T" clone https://github.com/o/r`, "git -c 'http.extraHeader=Authorization: <redacted>' clone https://github.com/{}/{}"},
		{`sh -c 'git -c http.extraHeader="Authorization: Bearer S3CR3T" fetch'`, `sh -c 'git -c '\''http.extraHeader=Authorization: <redacted>'\'' fetch'`},
		{"git -c url.https://x-access-token:S3CR3T@github.com/.insteadOf=https://github.com/ fetch", "git -c url.https://github.com/{}=<redacted> fetch"},
		{`git -C /src --no-pager -c http.extraHeader="Authorization: Bearer S3CR3T" fetch`, "git -C /src --no-pager -c 'http.extraHeader=Authorization: <redacted>' fetch"},
		{`git clone -c http.extraHeader="Authorization: Bearer S3CR3T" https://github.com/o/r`, "git clone -c 'http.extraHeader=Authorization: <redacted>' https://github.com/{}/{}"},
		{`git clone --config http.extraHeader="Authorization: Bearer S3CR3T" https://github.com/o/r`, "git clone --config 'http.extraHeader=Authorization: <redacted>' https://github.com/{}/{}"},
		{`git clone --config=http.extraHeader="Authorization: Bearer S3CR3T" https://github.com/o/r`, "git clone '--config=http.extraHeader=Authorization: <redacted>' https://github.com/{}/{}"},
		{`docker run --rm alpine/git -c http.extraHeader="Authorization: Bearer S3CR3T" clone https://github.com/o/r`, "docker run --rm alpine/git -c 'http.extraHeader=Authorization: <redacted>' clone https://github.com/{}/{}"},
		{`docker exec app git -c http.extraHeader="Authorization: Bearer S3CR3T" pull`, "docker exec app git -c 'http.extraHeader=Authorization: <redacted>' pull"},
		// A shell's -c after a word named git is the shell's: past git's
		// command, and where the word after -c is no git setting.
		{`docker run --rm alpine/git sh -c 'mysql -uroot -pS3CR3T -e "select 1"'`, `docker run --rm alpine/git sh -c 'mysql -uroot -p<redacted> -e '\''select 1'\'''`},
		{`docker run --rm --entrypoint sh alpine/git -c 'mysql -uroot -pS3CR3T -e "select 1"'`, `docker run --rm --entrypoint sh alpine/git -c 'mysql -uroot -p<redacted> -e '\''select 1'\'''`},
		{`git pull && sh -c 'curl -u admin:S3CR3T -H "Authorization: Bearer S3CR3T" http://dep/x'`, `git pull && sh -c 'curl -u admin:<redacted> -H '\''Authorization: <redacted>'\'' http://dep/{}'`},
		{"git pull; sh -c 'mysql -uroot -pS3CR3T'", "git pull; sh -c 'mysql -uroot -p<redacted>'"},
		{"git -c core.autocrlf=false fetch && sh -c 'mysql -uroot -pS3CR3T'", "git -c core.autocrlf=<redacted> fetch && sh -c 'mysql -uroot -p<redacted>'"},
		// A Windows program's /Name:value and /p:Name=value.
		{"sqlpackage /Action:Publish /TargetPassword:S3CR3T /tp:S3CR3T /p:Password=S3CR3T", "sqlpackage /Action:Publish /TargetPassword:<redacted> /tp:<redacted> /p:Password=<redacted>"},
		{"msbuild app.sln /p:Configuration=Release /p:Password=S3CR3T -p:DbPassword=S3CR3T /m", "msbuild app.sln /p:Configuration=Release /p:Password=<redacted> -p:DbPassword=<redacted> /m"},
		{"dotnet build -p:Password=S3CR3T", "dotnet build -p:Password=<redacted>"},
		{"sqlpackage /Action:Publish /tcs:Server=db.keploy.test;Password=S3CR3T", "sqlpackage /Action:Publish /tcs:Server=<redacted>"},
		// npm's token of a registry.
		{"npm config set //registry.npmjs.org/:_authToken=S3CR3T", "npm config set //registry.npmjs.org/:_authToken=<redacted>"},
		{"npm config set //registry.npmjs.org/:_authToken S3CR3T", "npm config set //registry.npmjs.org/:_authToken <redacted>"},
		// No password: whole.
		{"go install golang.org/x/net@latest example.com/m/v2@v2.1.0 github.com/org/tool@v1.2.3", "go install golang.org/x/net@latest example.com/m/v2@v2.1.0 github.com/org/tool@v1.2.3"},
		{"./ci actions/checkout@v4 actions/setup-go@v5.0.1 owner/repo@main owner/repo@release/1.2", "./ci actions/checkout@v4 actions/setup-go@v5.0.1 owner/repo@main owner/repo@release/1.2"},
		{"docker pull myorg/app@" + digest + " library/nginx:1.25@" + digest, "docker pull myorg/app@" + digest + " library/nginx:1.25@" + digest},
		{"npm i @types/node@20 @scope/pkg@1.2.3 lodash@4.17.21", "npm i @types/node@20 @scope/pkg@1.2.3 lodash@4.17.21"},
		{"./app images/logo@2x.png team/alice@example.com", "./app images/logo@2x.png team/alice@example.com"},
		{"./app api.keploy.test/v1@db.keploy.test:1521/XE", "./app api.keploy.test/v1@db.keploy.test:1521/XE"},
		{"sqlplus /nolog @setup.sql", "sqlplus /nolog @setup.sql"},
		{"sqlplus / as sysdba", "sqlplus / as sysdba"},
		{"sqlplus scott@orcl", "sqlplus scott@orcl"},
		{"sqlplus -s scott/S3CR3T@orcl @export.sql out/data.csv", "sqlplus -s scott/<redacted>@orcl @export.sql out/data.csv"},
		{"mysql -h db.keploy.test -P 3306 -uroot shop", "mysql -h db.keploy.test -P 3306 -uroot shop"},
		{"lftp -u alice sftp://files.keploy.test", "lftp -u alice sftp://files.keploy.test"},
		{"ps -u alice,bob -U root", "ps -u alice,bob -U root"},
		{"smbcacls //fs.keploy.test/share report.txt -U alice%S3CR3T", "smbcacls //fs.keploy.test/share report.txt -U alice%<redacted>"},
		{"smbclient -U alice //fs.keploy.test/share -N", "smbclient -U alice //fs.keploy.test/share -N"},
		{"smbclient //fs.keploy.test/share -U alice -c ls", "smbclient //fs.keploy.test/share -U alice -c ls"},
		{"msbuild app.sln /p:Configuration=Release /m:4", "msbuild app.sln /p:Configuration=Release /m:4"},
	} {
		if got := RedactCommandLine(tc.cmd); got != tc.want {
			t.Errorf("RedactCommandLine(%q)\n got %q\nwant %q", tc.cmd, got, tc.want)
		}
	}
}

// curl's user and proxy user, user:password, keep no password in any form
// curl takes: apart (--user, -U), attached (-u, -U) and after an '=' (curl
// 8.16 and later: --user=, --proxy-user=), in a shell's command too. A user
// alone, and another program's --user=, are kept.
func TestCurlsUserAndProxyUserAreRedactedInEveryFormCurlTakes(t *testing.T) {
	for _, tc := range []struct{ cmd, want string }{
		{"curl --user=admin:S3CR3T http://dep.keploy.test/x", "curl --user=admin:<redacted> http://dep.keploy.test/{}"},
		{"curl --user=admin:S3=CR3T http://dep.keploy.test/x", "curl --user=admin:<redacted> http://dep.keploy.test/{}"},
		{"curl --proxy-user=admin:S3CR3T -x http://proxy.keploy.test:3128 http://dep.keploy.test/x", "curl --proxy-user=admin:<redacted> -x http://proxy.keploy.test:3128 http://dep.keploy.test/{}"},
		{"curl -Uadmin:S3CR3T -x http://proxy.keploy.test:3128 http://dep.keploy.test/x", "curl -Uadmin:<redacted> -x http://proxy.keploy.test:3128 http://dep.keploy.test/{}"},
		{"curl -Uadmin:S3=CR3T -x http://proxy.keploy.test:3128 http://dep.keploy.test/x", "curl -Uadmin:<redacted> -x http://proxy.keploy.test:3128 http://dep.keploy.test/{}"},
		{"curl -U admin:S3CR3T --user admin:S3CR3T http://dep.keploy.test/x", "curl -U admin:<redacted> --user admin:<redacted> http://dep.keploy.test/{}"},
		{"sh -c 'curl --user=admin:S3CR3T http://dep.keploy.test/x'", "sh -c 'curl --user=admin:<redacted> http://dep.keploy.test/{}'"},
		{"curl --user=admin http://dep.keploy.test/x", "curl --user=admin http://dep.keploy.test/{}"},
		{"wget --user=admin --password=S3CR3T http://dep.keploy.test/x", "wget --user=admin --password=<redacted> http://dep.keploy.test/{}"},
		{"docker run --user=1000:1000 img", "docker run --user=1000:1000 img"},
	} {
		if got := RedactCommandLine(tc.cmd); got != tc.want {
			t.Errorf("RedactCommandLine(%q)\n got %q\nwant %q", tc.cmd, got, tc.want)
		}
	}
}

// A git alias whose name holds white space (alias.<name>.command, git
// 2.55) is git's setting: its value, a '!' shell command or git's
// arguments, is redacted whole, in keploy's own -c command too, as an
// alias with no white space in its name is.
func TestASpacedGitAliasKeepsNoPassword(t *testing.T) {
	for _, tc := range []struct{ cmd, want string }{
		{`git -c "alias.my deploy.command=!curl -u admin:S3CR3T http://dep.keploy.test/x" "my deploy"`, "git -c 'alias.my deploy.command=<redacted>' 'my deploy'"},
		{`git -c "alias.my deploy.command=!sshpass -p S3CR3T ssh dep.keploy.test" "my deploy"`, "git -c 'alias.my deploy.command=<redacted>' 'my deploy'"},
		{`git -c "alias.my sync.command=!mysql -uroot -pS3CR3T shop" "my sync"`, "git -c 'alias.my sync.command=<redacted>' 'my sync'"},
		{`git -c "alias.my x.command=hunter two S3CR3T" "my x"`, "git -c 'alias.my x.command=<redacted>' 'my x'"},
		{`git -c "alias.deploy=!curl -u admin:S3CR3T http://dep.keploy.test/x" deploy`, "git -c alias.deploy=<redacted> deploy"},
	} {
		if got := RedactCommandLine(tc.cmd); got != tc.want {
			t.Errorf("RedactCommandLine(%q)\n got %q\nwant %q", tc.cmd, got, tc.want)
		}
	}
	keploy := []string{"keploy", "test", "-c", `git -c 'alias.my deploy.command=!curl -u admin:S3CR3T http://dep.keploy.test/x' 'my deploy'`, "--delay", "10"}
	if got, want := RedactArgv(keploy)[3], "git -c 'alias.my deploy.command=<redacted>' 'my deploy'"; got != want {
		t.Errorf("RedactArgv(keploy's arguments)[3]\n got %q\nwant %q", got, want)
	}
}

// git's setting is read after any program's word: git's word takes more
// forms than a list can tell (an image with a tag or a digest, a word a
// separator or a parenthesis is glued to, a variable, another name, a
// wrapper), and read as a shell's command, a header's token is kept. A
// shell's own -c command is still read as one. Another program's -c or
// --config value of a setting's shape is redacted as git's: what a line
// loses, never what it keeps.
func TestAGitSettingIsRedactedAfterAnyProgramsWord(t *testing.T) {
	const h = `http.extraHeader="Authorization: Bearer S3CR3T"`
	const hr = `'http.extraHeader=Authorization: <redacted>'`
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct{ cmd, want string }{
		// git in an image with a tag or a digest, as CI pins it.
		{"docker run --rm alpine/git:2.43.0 -c " + h + " clone https://github.com/o/r", "docker run --rm alpine/git:2.43.0 -c " + hr + " clone https://github.com/{}/{}"},
		{"docker run --rm alpine/git@" + digest + " -c " + h + " clone https://github.com/o/r", "docker run --rm alpine/git@" + digest + " -c " + hr + " clone https://github.com/{}/{}"},
		{`docker run --rm docker.io/alpine/git:v2.47.2 -c http.extraHeader="PRIVATE-TOKEN: S3CR3T" clone https://gitlab.com/o/r`, "docker run --rm docker.io/alpine/git:v2.47.2 -c 'http.extraHeader=PRIVATE-TOKEN: <redacted>' clone https://gitlab.com/{}/{}"},
		// git's word with a separator or a parenthesis glued to it.
		{"cd /src&&git -c " + h + " fetch", "cd /src&&git -c " + hr + " fetch"},
		{"cd /src;git -c " + h + " fetch", "cd /src;git -c " + hr + " fetch"},
		{"(git -c " + h + " fetch)", "(git -c " + hr + " fetch)"},
		{"echo $(git -c " + h + " fetch)", "echo $(git -c " + hr + " fetch)"},
		// A variable, another name, a wrapper.
		{`"$GIT" -c ` + h + " fetch", "$GIT -c " + hr + " fetch"},
		{"hub -c " + h + " fetch", "hub -c " + hr + " fetch"},
		{"sudo git -c " + h + " fetch", "sudo git -c " + hr + " fetch"},
		{"sudo -E git -c " + h + " fetch", "sudo -E git -c " + hr + " fetch"},
		{"env GIT_TRACE=1 git -c " + h + " fetch", "env GIT_TRACE=<redacted> git -c " + hr + " fetch"},
		{"xargs -n1 git -c " + h + " fetch", "xargs -n1 git -c " + hr + " fetch"},
		{"docker exec app git -C /r -c " + h + " pull", "docker exec app git -C /r -c " + hr + " pull"},
		// An insteadOf URL's userinfo, and --config in both forms, after a
		// tagged image.
		{"docker run --rm alpine/git:2.43.0 -c url.https://x-access-token:S3CR3T@github.com/.insteadOf=https://github.com/ fetch", "docker run --rm alpine/git:2.43.0 -c url.https://github.com/{}=<redacted> fetch"},
		{"docker run --rm alpine/git:2.43.0 clone --config=" + h + " https://github.com/o/r", "docker run --rm alpine/git:2.43.0 clone '--config=http.extraHeader=Authorization: <redacted>' https://github.com/{}/{}"},
		{"docker run --rm alpine/git:2.43.0 clone --config " + h + " https://github.com/o/r", "docker run --rm alpine/git:2.43.0 clone --config " + hr + " https://github.com/{}/{}"},
		// A subsection holding white space, dots and slashes, quoted whole,
		// in single quotes, or quoted alone, as git takes any subsection;
		// and a shell's command of that shape, read word by word.
		{"git -c \"http.https://host/my repo.extraHeader=Authorization: Bearer S3CR3T\" fetch", "git -c 'http.https://host/{} repo.extraHeader=Authorization: <redacted>' fetch"},
		{"git -c 'http.https://host/my repo.extraHeader=Authorization: Bearer S3CR3T' fetch", "git -c 'http.https://host/{} repo.extraHeader=Authorization: <redacted>' fetch"},
		{"git -c http.\"https://host/my repo\".extraHeader=\"Authorization: Bearer S3CR3T\" fetch", "git -c 'http.https://host/{} repo.extraHeader=Authorization: <redacted>' fetch"},
		{"docker run --rm alpine/git:2.43.0 -c \"http.https://git.example/team a/repo.v2.git.extraHeader=PRIVATE-TOKEN: S3CR3T\" fetch", "docker run --rm alpine/git:2.43.0 -c 'http.https://git.example/{} a/repo.v2.git.extraHeader=PRIVATE-TOKEN: <redacted>' fetch"},
		{"git -c \"url.https://oauth2:S3CR3T@git.example/my group/.insteadOf=https://git.example/my group/\" fetch", "git -c 'url.https://git.example/{} group/.insteadOf=<redacted>' fetch"},
		{"git -c \"remote.my origin.url=https://u:S3CR3T@git.example/r\" fetch", "git -c 'remote.my origin.url=<redacted>' fetch"},
		{"git -c \"http.https://host/my repo.sslVerify\" fetch", "git -c 'http.https://host/{} repo.sslVerify' fetch"},
		{"sh -c 'mysql.exe -uroot -pS3CR3T db.x=1'", "sh -c 'mysql.exe -uroot -p<redacted> db.x=<redacted>'"},
		{"sh -c 'mysql.exe -uroot -pS3CR3T db.x'", "sh -c 'mysql.exe -uroot -p<redacted> db.x'"},
		{"sh -c 'python3.11 -m pytest tests/test_a.py'", "sh -c 'python3.11 -m pytest tests/test_a.py'"},
		{"sh -c 'node.exe a.js x.y=1 --token S3CR3T'", "sh -c 'node.exe a.js x.y=<redacted> --token <redacted>'"},
		// A tab, an escaped quote, a quote inside the quotes, escaped
		// spaces, an upper-case section, --config-env, the credential and
		// lfs sections; and a shell's command whose first word holds a dot,
		// which no URL section names: whole but for its setting-shaped values.
		{"git -c \"http.https://h/a\tb.extraHeader=Authorization: Bearer S3CR3T\" fetch", "git -c 'http.https://h/{} b.extraHeader=Authorization: <redacted>' fetch"},
		{"git -c \"http.https://h/a b.extraHeader=Authorization:\tBearer S3CR3T\" fetch", "git -c 'http.https://h/{} b.extraHeader=Authorization: <redacted>' fetch"},
		{"git -c http.\"https://host/a\\\"b c\".extraHeader=\"Authorization: Bearer S3CR3T\" fetch", "git -c ''\\''http.https://host/{} c.extraHeader'\\''=Authorization: <redacted>' fetch"},
		{"git -c \"http.'https://host/a b'.extraHeader=Authorization: Bearer S3CR3T\" fetch", "git -c ''\\''http.https://host/{} b.extraHeader'\\''=Authorization: <redacted>' fetch"},
		{"git -c http.https://host/my\\ repo.extraHeader=Authorization:\\ Bearer\\ S3CR3T fetch", "git -c 'http.https://host/{} repo.extraHeader=Authorization: <redacted>' fetch"},
		{"git -c \"HTTP.https://host/my repo.extraHeader=Authorization: Bearer S3CR3T\" fetch", "git -c 'http.https://host/{} repo.extraHeader=Authorization: <redacted>' fetch"},
		{"GIT_AUTH=\"Authorization: Bearer S3CR3T\" git --config-env=http.extraHeader=GIT_AUTH fetch", "GIT_AUTH=<redacted> git --config-env=http.extraHeader=<redacted> fetch"},
		{"git -c \"credential.https://host/my team.username=alice\" -c \"lfs.https://host/my repo.access=basic\" fetch", "git -c 'credential.https://host/{} team.username=<redacted>' -c 'lfs.https://host/{} repo.access=<redacted>' fetch"},
		{"sh -c 'java.exe -jar app.jar --spring.profiles.active=dev --server.port=8080'", "sh -c 'java.exe -jar app.jar --spring.profiles.active=dev --server.port=8080'"},
		{"sh -c 'python3 -m a.b --x.y=1 z'", "sh -c 'python3 -m a.b --x.y=1 z'"},
		// Another program's setting-shaped value: redacted as git's.
		{"./tool -c 'x.y=Authorization: Bearer S3CR3T'", "./tool -c 'x.y=Authorization: <redacted>'"},
		{`python -c 'sys.argv=["app", "--port", "8080"]; import app'`, "python -c sys.argv=<redacted>"},
		{"spark-submit -c 'spark.driver.extraJavaOptions=-Dx=1 -Dy=2' app.py", "spark-submit -c spark.driver.extraJavaOptions=<redacted> app.py"},
		{"spark-submit -c spark.driver.memory=4g app.py", "spark-submit -c spark.driver.memory=<redacted> app.py"},
		// A file is no setting: whole.
		{"gunicorn -c gunicorn.conf.py app:app", "gunicorn -c gunicorn.conf.py app:app"},
		{"java -jar app.jar --config=application.yml", "java -jar app.jar --config=application.yml"},
		// A shell's own command after a word named git: the shell's.
		{`docker run --rm alpine/git sh -c 'mysql -uroot -pS3CR3T -e "select 1"'`, `docker run --rm alpine/git sh -c 'mysql -uroot -p<redacted> -e '\''select 1'\'''`},
		{`git pull && sh -c 'curl -u admin:S3CR3T -H "Authorization: Bearer S3CR3T" http://dep/x'`, `git pull && sh -c 'curl -u admin:<redacted> -H '\''Authorization: <redacted>'\'' http://dep/{}'`},
	} {
		if got := RedactCommandLine(tc.cmd); got != tc.want {
			t.Errorf("RedactCommandLine(%q)\n got %q\nwant %q", tc.cmd, got, tc.want)
		}
	}
}

// JoinCommandLine quotes a word that holds any white space (\r, \v, a
// no-break space, U+3000 too) or any quote (', ", `), so each word reads as
// one word.
func TestJoinCommandLineQuotesEveryWordAShellWouldSplit(t *testing.T) {
	got := JoinCommandLine([]string{"./app", "a\rb", "a\vb", "a b", "a　b", "a`b", "a'b", "", "plain:1"})
	if want := "./app 'a\rb' 'a\vb' 'a b' 'a　b' 'a`b' 'a'\\''b' '' plain:1"; got != want {
		t.Errorf("JoinCommandLine\n got %q\nwant %q", got, want)
	}
}

// keploy says it starts the app in these words, which CI lanes read as the
// mark that the app was started (keploy's and enterprise's: grep -E
// 'Starting Application ?:'), so they stay as they are.
func TestStartingApplicationsWordsStay(t *testing.T) {
	for got, want := range map[string]string{
		StartingApplication:        "Starting Application :",
		startingApplicationPTY:     "Starting Application:",
		startingApplicationWindows: "Starting Application (Windows):",
	} {
		if got != want {
			t.Errorf("the line is %q, want %q", got, want)
		}
	}
}

// No line at INFO or above, and no error, names a command whole: a call that
// logs at INFO, WARN or ERROR, or makes an error or a message, never takes
// the redactor's text of a command (RedactCommandLine, RedactCmdArgs,
// RedactArgv, an AppCommand's WholeRedacted) in its arguments. Only a DEBUG
// line does (Debug, LogWholeCommand), beside one that names the command by
// its program (AppCommand). Read from every Go file of the module but its
// tests. The flow test (internal/cmdflow) follows the command itself.
func TestNoLineAtInfoOrAboveNamesACommandWhole(t *testing.T) {
	redactors := map[string]bool{"RedactCommandLine": true, "RedactCmdArgs": true, "RedactArgv": true, "WholeRedacted": true}
	nameOf := func(call *ast.CallExpr) string {
		switch f := call.Fun.(type) {
		case *ast.SelectorExpr:
			return f.Sel.Name
		case *ast.Ident:
			return f.Name
		}
		return ""
	}
	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != ".." && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata" || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch name := nameOf(call); name {
			case "Debug", "Debugf", "Debugw", "LogWholeCommand", "LogWholeCmdArgs":
				return false
			case "Info", "Infof", "Infow", "Warn", "Warnf", "Warnw", "Error", "Errorf", "Errorw", "Fatal", "Fatalf", "Panic", "DPanic",
				"LogError", "Sprintf", "Sprint", "Printf", "Println", "Fprintf", "Fprintln", "New", "Wrap", "Wrapf":
				for _, a := range call.Args {
					ast.Inspect(a, func(n ast.Node) bool {
						if c, ok := n.(*ast.CallExpr); ok && redactors[nameOf(c)] {
							t.Errorf("%s: %s takes a command whole (%s)", fset.Position(c.Pos()), name, nameOf(c))
						}
						return true
					})
				}
				return false
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 500 {
		t.Fatalf("read %d Go files, want the module's", files)
	}
}

// Every section of git's that takes a subsection (gitSubsectionSections, git
// 2.55.0's own list) has a row here: a spaced subsection with a secret in its
// value, which must be redacted. A section added to the list needs a row; a
// row needs its section in the list.
func TestEverySectionGitTakesASubsectionInRedactsItsValue(t *testing.T) {
	rows := map[string]string{
		"alias":             `git -c "alias.my deploy.command=!curl -u admin:S3CR3T http://dep/x" "my deploy"`,
		"bitmappseudomerge": `git -c "bitmapPseudoMerge.my set.pattern=hunter two S3CR3T" gc`,
		"branch":            `git -c "branch.my topic.description=hunter two S3CR3T" log`,
		"browser":           `git -c "browser.my browser.cmd=curl -u admin:S3CR3T x" help -w log`,
		"bundle":            `git -c "bundle.my id.uri=https://u:S3CR3T@host/b" fetch`,
		"credential":        `git -c "credential.https://host/my team.username=hunter two S3CR3T" fetch`,
		"diff":              `git -c "diff.my driver.textconv=sshpass -p S3CR3T cat" diff`,
		"difftool":          `git -c "difftool.my tool.cmd=curl -u admin:S3CR3T http://dep/x" difftool`,
		"filter":            `git -c "filter.my f.smudge=curl -u admin:S3CR3T http://dep/x" checkout`,
		"gc":                `git -c "gc.my pattern.reflogExpire=hunter two S3CR3T" gc`,
		"gpg":               `git -c "gpg.my format.program=sshpass -p S3CR3T gpg" log`,
		"guitool":           `git -c "guitool.my tool.cmd=curl -u admin:S3CR3T x" gui`,
		"hook":              `git -c "hook.my hook.command=curl -u admin:S3CR3T x" commit`,
		"http":              `git -c "http.https://host/my repo.extraHeader=Authorization: Bearer S3CR3T" fetch`,
		"includeif":         `git -c "includeIf.gitdir:~/my dir/.path=hunter two S3CR3T" log`,
		"lfs":               `git -c "lfs.https://host/my repo.access=hunter two S3CR3T" fetch`,
		"maintenance":       `git -c "maintenance.my task.schedule=hunter two S3CR3T" maintenance run`,
		"man":               `git -c "man.my viewer.cmd=sshpass -p S3CR3T man" help log`,
		"merge":             `git -c "merge.my driver.driver=curl -u admin:S3CR3T x" merge`,
		"mergetool":         `git -c "mergetool.my tool.cmd=sshpass -p S3CR3T meld" mergetool`,
		"notes":             `git -c "notes.my ref.mergeStrategy=hunter two S3CR3T" notes`,
		"protocol":          `git -c "protocol.my proto.allow=hunter two S3CR3T" fetch`,
		"remote":            `git -c "remote.my origin.url=https://u:S3CR3T@host/r" fetch`,
		"sendemail":         `git -c "sendemail.my id.smtpPass=hunter two S3CR3T" send-email`,
		"sideband":          `git -c "sideband.https://host/my repo.allowControlCharacters=hunter two S3CR3T" fetch`,
		"submodule":         `git -c "submodule.my lib.url=https://u:S3CR3T@host/lib" submodule update`,
		"trailer":           `git -c "trailer.my alias.cmd=curl -u admin:S3CR3T x" commit`,
		"uploadpackfilter":  `git -c "uploadpackfilter.my filter.allow=hunter two S3CR3T" upload-pack`,
		"url":               `git -c "url.https://oauth2:S3CR3T@host/my group/.insteadOf=https://host/my group/" fetch`,
	}
	for section := range gitSubsectionSections {
		if rows[section] == "" {
			t.Errorf("git's section %q takes a subsection and has no row here", section)
		}
	}
	for section, cmd := range rows {
		if !gitSubsectionSections[section] {
			t.Errorf("a row for %q, which gitSubsectionSections does not list", section)
		}
		if got := RedactCommandLine(cmd); strings.Contains(got, "S3CR3T") || strings.Contains(got, "two") {
			t.Errorf("%s: RedactCommandLine(%q) = %q", section, cmd, got)
		}
	}
}
