package utils

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/getsentry/sentry-go"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/linkout"
	keployLog "go.keploy.io/server/v3/utils/log"
	"go.keploy.io/server/v3/utils/plaintcp"
	"go.uber.org/zap"
	"helm.sh/helm/v3/pkg/strvals"
)

var WarningSign = "\U000026A0"

var ErrCode = 0

// IsShutdownError checks if the error is related to shutdown (EOF, connection closed, etc.)
// This is useful for gracefully handling errors during application shutdown.
func IsShutdownError(err error) bool {
	if err == nil {
		return false
	}
	// Check for EOF errors
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Check error message for common shutdown-related patterns
	errStr := err.Error()
	return strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "use of closed network connection")
}

// UnbracketHost is host without the one pair of brackets an IPv6 literal
// comes in when it is written next to a port ("[fd00::1]"). "fd00::1" and
// "[fd00::1]" are one host, and the helpers here bracket an IPv6 host
// themselves wherever the form they build needs it (net.JoinHostPort, a URL's
// host), so one passed in bracketed would come out bracketed twice.
func UnbracketHost(host string) string {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1]
	}
	return host
}

// ReplaceHost replaces currentURL's host with ipAddress, keeping its port.
//
// ipAddress may be IPv6, bare ("fd00::1", as a Kubernetes Pod IP is) or
// bracketed. An IPv6 host has to be bracketed in a URL: "http://fd00::1:8080/"
// is no URL, and url.Parse refuses it (invalid port "::1:8080" after host).
// A zone ("fe80::1%eth0") is kept and written as a URL writes it (%25).
// Any other host (a name, an IPv4 address) is put in as given.
func ReplaceHost(currentURL string, ipAddress string) (string, error) {
	// Parse the current URL
	parsedURL, err := url.Parse(currentURL)

	if err != nil {
		// Return the original URL if parsing fails
		return currentURL, err
	}

	if ipAddress == "" {
		return currentURL, fmt.Errorf("failed to replace url in case of docker env")
	}

	host := ipAddress
	if bare := UnbracketHost(ipAddress); isIPv6Literal(bare) {
		host = "[" + bare + "]"
	}
	if port := parsedURL.Port(); port != "" {
		host += ":" + port
	}
	parsedURL.Host = host
	return parsedURL.String(), nil
}

// isIPv6Literal says host is an IPv6 address, with or without a zone, an
// IPv4-mapped one ("::ffff:127.0.0.1") included: every one has colons, so
// each needs the brackets.
func isIPv6Literal(host string) bool {
	a, err := netip.ParseAddr(host)
	return err == nil && a.Is6()
}

// ReplaceGrpcHost replaces authority's host with ipAddress, keeping its port.
//
// An authority may have no port ("my-svc", "10.0.0.5", "[fd00::1]"): then
// ipAddress alone comes back, an IPv6 one bracketed as an authority writes
// it, and ResolveTestTarget gives it its port (app_port, grpcPort or 443).
// ipAddress may be IPv6, bare or bracketed. One with a zone ("fe80::1%eth0")
// is put in too, but grpc-go refuses a target with a zone.
func ReplaceGrpcHost(authority string, ipAddress string) (string, error) {
	// Check if ipAddress is empty
	if ipAddress == "" {
		return authority, fmt.Errorf("failed to replace authority in case of docker env: empty IP address")
	}

	port, hasPort, err := authorityPort(authority)
	if err != nil {
		return authority, fmt.Errorf("invalid authority format: %v", err)
	}
	host := UnbracketHost(ipAddress)
	if !hasPort {
		if isIPv6Literal(host) {
			return "[" + host + "]", nil
		}
		return host, nil
	}
	// Replace the host part with ipAddress, keeping the port. JoinHostPort
	// brackets an IPv6 host, so a bracketed one is given to it bare.
	return net.JoinHostPort(host, port), nil
}

// authorityPort is the port of a host:port authority, and hasPort false for
// one with no port: one with no colon (a name, an IPv4 address), one in
// brackets ("[fd00::1]"), or a bare IPv6 address, whose colons are the
// address's. The host itself is not checked, as ReplaceGrpcHost replaces it
// whole. Anything else net.SplitHostPort cannot split ("a:b:c", "[fd00::1")
// is an error.
func authorityPort(authority string) (port string, hasPort bool, err error) {
	_, port, err = net.SplitHostPort(authority)
	if err == nil {
		return port, true, nil
	}
	if !strings.Contains(authority, ":") || UnbracketHost(authority) != authority || isIPv6Literal(authority) {
		return "", false, nil
	}
	return "", false, err
}

func ReplaceGrpcPort(authority string, port string) (string, error) {
	// Check if port is empty
	if port == "" {
		return authority, fmt.Errorf("failed to replace port in case of docker env: empty port")
	}

	// Use net.SplitHostPort to properly handle IPv6 addresses
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		// If splitting fails, assume it's just a host without port: "[fd00::1]"
		// or "fd00::1" as much as "localhost", which JoinHostPort brackets.
		return net.JoinHostPort(UnbracketHost(authority), port), nil
	}

	// Replace the port part, keeping the host
	return net.JoinHostPort(host, port), nil
}

// ReplaceBaseURL replaces the base URL (scheme + host) of the given URL with the provided baseURL.
// It returns the updated URL as a string or an error if the operation fails.
func ReplaceBaseURL(currentURL string, baseURL string) (string, error) {
	// Parse the current URL
	parsedURL, err := url.Parse(currentURL)
	if err != nil {
		return currentURL, err
	}

	// Check if baseURL is valid
	if baseURL == "" {
		return currentURL, fmt.Errorf("failed to replace baseURL: baseURL is empty")
	}

	// Parse the new baseURL
	newBaseURL, err := url.Parse(baseURL)
	if err != nil {
		return currentURL, fmt.Errorf("invalid baseURL: %w", err)
	}

	// Replace the scheme and host
	parsedURL.Scheme = newBaseURL.Scheme
	parsedURL.Host = newBaseURL.Host

	// Return the updated URL as a string
	return parsedURL.String(), nil
}

func ReplacePort(currentURL string, port string) (string, error) {
	if port == "" {
		return currentURL, fmt.Errorf("failed to replace port in case of docker env")
	}

	parsedURL, err := url.Parse(currentURL)

	if err != nil {
		return currentURL, err
	}

	// Rebuilt from the hostname, never edited as text: replacing the port's
	// digits in Host replaced their first occurrence, which can be in the host
	// ("10.0.0.80:80" became "10.0.0.9000:80", "[fd00::8080]:8080" a
	// different host). JoinHostPort brackets an IPv6 hostname.
	parsedURL.Host = net.JoinHostPort(parsedURL.Hostname(), port)
	return parsedURL.String(), nil
}

// GetReqMeta is what a log line logged under ctx says about req: its
// method, its host (a name by its shape and tag, models.HostLogName), its
// path's shape (URLPathShape), its path tag (path_tag,
// models.URLPathTag under the session ctx carries: what the console and the
// report print beside the request where they name it whole; none for a
// request that names no method) and the names of its query parameters
// (GetReqShape). For the agent's lines about a call, which the console and
// the report name by that tag. ctx is the context of the work the line is
// logged in (a connection's, a request's): a line under a context with no
// session tags under the process's key (models.LogKeySession).
func GetReqMeta(ctx context.Context, req *http.Request) map[string]string {
	reqMeta := GetReqShape(req)
	if req != nil && req.Host != "" {
		// The host's name by its shape beside its tag under the session
		// (models.HostLogName), so the session's lines about one host match
		// up; an IP address whole.
		reqMeta["host"] = models.HostLogName(ctx, req.Host)
	}
	if req != nil && req.URL != nil {
		if tag := models.URLPathTag(ctx, req.Method, req.URL); tag != "" {
			reqMeta["path_tag"] = tag
		}
	}
	return reqMeta
}

// GetReqShape is what a log says about req with no path tag: its method, its
// host (a name by its labels' shape, models.HostShape, with no tag), its
// path's shape (URLPathShape) and the names of its query
// parameters. For a line of keploy's CLI about a test case's request (one it
// sends, stores or compares): no console or report line prints such a tag,
// so the line names the request by its shape, and the test case where it has
// one. Never a path segment, a query value, a header or the body: those are
// the application's content (a segment may be a token the recording
// redacted), and logs get pasted into issues. A query part with no "=" is not
// named either, since it may be a bare token; a bare token that holds "="
// (padded base64, ?dG9rZW4=) is named by what comes before its first "="
// (dG9rZW4).
func GetReqShape(req *http.Request) map[string]string {
	reqMeta := map[string]string{}
	if req == nil {
		return reqMeta
	}
	reqMeta["method"] = req.Method
	// The host by its labels' shape (models.HostShape): a name is the
	// app's, as a path is; an IP address whole.
	reqMeta["host"] = models.HostShape(req.Host)
	if req.URL != nil {
		reqMeta["path"] = URLPathShape(req.URL)
		if keys := queryKeys(req.URL.RawQuery); len(keys) > 0 {
			reqMeta["query keys"] = strings.Join(keys, ",")
		}
	}
	return reqMeta
}

// URLPathShape is u's path in its shape: each segment of the escaped path
// written {}, an empty one (a trailing "/", "//") kept empty, so it names
// how many segments the path has and never a value of one. A log made where
// no recording is at hand names a path this way: keploy does not know there
// which segment holds a secret the recording redacted. "" for a nil u.
func URLPathShape(u *url.URL) string {
	if u == nil {
		return ""
	}
	escaped := u.EscapedPath()
	segs := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	for i, seg := range segs {
		if seg != "" {
			segs[i] = "{}"
		}
	}
	out := strings.Join(segs, "/")
	if strings.HasPrefix(escaped, "/") {
		out = "/" + out
	}
	return out
}

// queryKeys are the names of rawQuery's name=value parameters, unescaped,
// sorted and each once. A name ends at its part's first "=": a value may
// hold "=" too (base64 padding).
func queryKeys(rawQuery string) []string {
	var keys []string
	for _, part := range strings.Split(rawQuery, "&") {
		k, _, ok := strings.Cut(part, "=")
		if !ok || k == "" {
			continue
		}
		if u, err := url.QueryUnescape(k); err == nil {
			k = u
		}
		// A name that does not read as one may be a value the app sent (a
		// token passed as a parameter's name): {} (models.KeyShape).
		keys = append(keys, models.KeyShape(k))
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// IsPassThrough reports whether one of opts' bypass rules names req, to
// destPort. A rule whose regex does not compile is said, with req by its
// shape alone (GetReqShape): the line is about keploy.yml's rule, and no
// console or report line names the call a rule was tried on beside a tag.
func IsPassThrough(logger *zap.Logger, req *http.Request, destPort uint, opts models.OutgoingOptions) bool {
	passThrough := false

	for _, bypass := range opts.Rules {
		if bypass.Host != "" {
			regex, err := regexp.Compile(bypass.Host)
			if err != nil {
				LogError(logger, err, "failed to compile the host regex", zap.Any("metadata", GetReqShape(req)))
				continue
			}
			passThrough = regex.MatchString(req.Host)
			if !passThrough {
				continue
			}
		}
		if bypass.Path != "" {
			regex, err := regexp.Compile(bypass.Path)
			if err != nil {
				LogError(logger, err, "failed to compile the path regex", zap.Any("metadata", GetReqShape(req)))
				// The rule's host may have matched; a path that does not
				// compile matches no call.
				passThrough = false
				continue
			}
			passThrough = regex.MatchString(req.URL.String())
			if !passThrough {
				continue
			}
		}

		if passThrough {
			if bypass.Port == 0 || bypass.Port == destPort {
				return true
			}
			passThrough = false
		}
	}

	return passThrough
}

func kebabToCamel(s string) string {
	parts := strings.Split(s, "-")
	for i := 1; i < len(parts); i++ {
		parts[i] = cases.Title(language.English).String(parts[i])
	}
	return strings.Join(parts, "")
}

// ConfigKeyAnnotation marks a flag whose value keploy moves into the config
// itself, under the config key the annotation names, so BindFlagsToViper does
// not bind it to a key of its own name. Without the mark, record's
// --pass-through-ports (a port list, which ValidateFlags turns into
// bypassRules) was bound to record.passThroughPorts, which holds telemetry
// rules, and the config failed to load whenever the flag was passed.
const ConfigKeyAnnotation = "keploy.io/config-key"

// BindFlagsToViper binds each flag of cmd to the config key of its own name,
// at the top level and under viperKeyPrefix (the command's name by default),
// and that key to its environment variable. A flag marked with
// ConfigKeyAnnotation is left out.
func BindFlagsToViper(logger *zap.Logger, cmd *cobra.Command, viperKeyPrefix string) error {
	var bindErr error
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		if _, ok := flag.Annotations[ConfigKeyAnnotation]; ok {
			return
		}
		camelCaseName := kebabToCamel(flag.Name)
		err := viper.BindPFlag(camelCaseName, flag)
		if err != nil {
			LogError(logger, err, "failed to bind flag Name to flag")
			bindErr = err
		}
		// Construct the Viper key and the env variable name
		if viperKeyPrefix == "" {
			viperKeyPrefix = cmd.Name()
		}
		viperKey := viperKeyPrefix + "." + camelCaseName
		envVarName := strings.ToUpper(viperKeyPrefix + "_" + camelCaseName)
		envVarName = strings.ReplaceAll(envVarName, ".", "_") // Why do we need this?

		// Bind the flag to Viper with the constructed key
		err = viper.BindPFlag(viperKey, flag)
		if err != nil {
			LogError(logger, err, "failed to bind flag to config")
			bindErr = err
		}

		// Tell Viper to also read this flag's value from the corresponding env variable
		err = viper.BindEnv(viperKey, envVarName)
		logger.Debug("Binding flag to viper", zap.String("viperKey", viperKey), zap.String("envVarName", envVarName))
		if err != nil {
			LogError(logger, err, "failed to bind environment variables to config")
			bindErr = err
		}
	})
	return bindErr
}

//func ModifyToSentryLogger(ctx context.Context, logger *zap.Logger, client *sentry.Client, configDb *configdb.ConfigDb) *zap.Logger {
//	cfg := zapsentry.Configuration{
//		Level:             zapcore.ErrorLevel, //when to send message to sentry
//		EnableBreadcrumbs: true,               // enable sending breadcrumbs to Sentry
//		BreadcrumbLevel:   zapcore.InfoLevel,  // at what level should we sent breadcrumbs to sentry
//		Tags: map[string]string{
//			"component": "system",
//		},
//	}
//
//	core, err := zapsentry.NewCore(cfg, zapsentry.NewSentryClientFromClient(client))
//	//in case of err it will return noop core. So we don't need to attach it to log.
//	if err != nil {
//		logger.Debug("failed to init zap", zap.Error(err))
//		return logger
//	}
//
//	logger = zapsentry.AttachCoreToLogger(core, logger)
//	kernelVersion := ""
//	if runtime.GOOS == "linux" {
//		cmd := exec.CommandContext(ctx, "uname", "-r")
//		kernelBytes, err := cmd.Output()
//		if err != nil {
//			logger.Debug("failed to get kernel version", zap.Error(err))
//		} else {
//			kernelVersion = string(kernelBytes)
//		}
//	}
//
//	arch := runtime.GOARCH
//	installationID, err := configDb.GetInstallationId(ctx)
//	if err != nil {
//		logger.Debug("failed to get installationID", zap.Error(err))
//	}
//	sentry.ConfigureScope(func(scope *sentry.Scope) {
//		scope.SetTag("Keploy Version", Version)
//		scope.SetTag("Linux Kernel Version", kernelVersion)
//		scope.SetTag("Architecture", arch)
//		scope.SetTag("Installation ID", installationID)
//	})
//	return logger
//}

// LogError logs the error with the provided fields if the error is not context.Canceled.
func LogError(logger *zap.Logger, err error, msg string, fields ...zap.Field) {
	if logger == nil {
		fmt.Println("Failed to log error. Logger is nil.")
		return
	}
	if !errors.Is(err, context.Canceled) {
		logger.Error(msg, append(fields, zap.Error(err))...)
	}
}

func DeleteFileIfExists(logger *zap.Logger, name string) (err error) {
	//Check if file exists
	_, err = os.Stat(name)
	if os.IsNotExist(err) {
		return nil
	}
	//If it does, remove it.
	err = os.Remove(name)
	if err != nil {
		if runtime.GOOS == "windows" && strings.Contains(strings.ToLower(err.Error()), "used by another process") {
			logger.Debug("skipping removal of file still in use during shutdown", zap.String("file", name), zap.Error(err))
			return nil
		}
		LogError(logger, err, "Error removing file")
		return err
	}

	return nil
}

type GitHubRelease struct {
	TagName string               `json:"tag_name"`
	Body    string               `json:"body"`
	Assets  []GitHubReleaseAsset `json:"assets"`
}

// GitHubReleaseAsset is one file of a release. Digest is GitHub's own
// "sha256:<hex>" of it, which `keploy update` checks the download against.
type GitHubReleaseAsset struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

var Emoji = "\U0001F430" + " Keploy:"
var ConfigGuide = `
# Visit [https://keploy.io/docs/running-keploy/configuration-file/] to learn about using keploy through configuration file.
`

// ErrNoAnswer is returned when there was nobody to answer the question: stdin
// reached EOF without a line. It is NOT the same as a "no", and a caller that
// conflates them reports a decision nobody made -- which is how `keploy config
// --generate` came to write nothing in CI and exit 0. Callers that only need a
// safe default may still treat it as false; callers about to skip work on the
// strength of the answer must not.
//
// A TTY check is not a substitute: CI runners that allocate a pty (docker -t,
// Jenkins, GitLab) look interactive and still deliver EOF.
var ErrNoAnswer = errors.New("no answer: stdin ended without one")

// AskForConfirmation asks the user for confirmation. A user must type in "yes" or "no" and
// then press enter. It has fuzzy matching, so "y", "Y", "yes", "YES", and "Yes" all count as
// confirmations. An unrecognized answer or a Ctrl+C is a graceful "no"; an
// EOF is ErrNoAnswer, because nobody declined -- nobody was there.
func AskForConfirmation(ctx context.Context, s string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("%s [y/n]: ", s)

	respCh := make(chan string, 1)
	errCh := make(chan error, 1)

	go func() {
		response, err := reader.ReadString('\n')
		// ReadString hands back what it read AND io.EOF when the input ends
		// without a newline -- which is what `printf yes | keploy ...` and a
		// heredoc with no final newline both look like. Discarding the line
		// and reporting only the error turned a plain "yes" into "there was
		// nobody to answer": first a silent wrong "no", and then, once EOF
		// became ErrNoAnswer, a refusal asserting that nobody had replied to
		// a question somebody had just answered.
		if err != nil && !(errors.Is(err, io.EOF) && strings.TrimSpace(response) != "") {
			errCh <- err
			return
		}
		respCh <- response
	}()

	select {
	case <-ctx.Done():
		// Cobra caught SIGINT (Ctrl+C) and cancelled its root context
		return false, nil
	case err := <-errCh:
		// A closed / non-TTY stdin (io.EOF) is not a failure and not a
		// decline: there is simply nobody to answer. Say which it was, so a
		// caller can refuse out loud instead of recording a decision that was
		// never made.
		if errors.Is(err, io.EOF) {
			return false, ErrNoAnswer
		}
		return false, err
	case response := <-respCh:
		response = strings.ToLower(strings.TrimSpace(response))
		if response == "y" || response == "yes" {
			return true, nil
		}
		return false, nil
	}
}

// CheckFileExists reports whether anything is at this path -- including a
// symlink whose target does not exist.
//
// Lstat, not Stat. Stat follows the link, so a DANGLING keploy.yml symlink
// read as "no file here": `keploy config --generate` never asked before
// overwriting, and wrote through the link to a path the user never named.
func CheckFileExists(path string) bool {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

var Version string

// DevVersion is the version of a build that was not given one.
const DevVersion = "3-dev"

var VersionIdentifier string
var LogFile *os.File

func GetVersionAsComment() string {
	return fmt.Sprintf("# Generated by Keploy (%s)\n", Version)
}

// ConfigHeader tops a generated keploy.yml. It says what the file holds --
// only what differs from the defaults -- because a developer opening a short
// file is otherwise entitled to think Keploy forgot the rest, and names the
// command that prints the rest so nobody has to go looking for it.
func ConfigHeader() string {
	return GetVersionAsComment() +
		"# Only the settings that differ from Keploy's defaults are written here;\n" +
		"# everything else uses its default. To read or save the full set:\n" +
		"#   keploy config defaults            # print them\n" +
		"#   keploy config defaults -o FILE    # save them\n"
}

// crashReportLogTail is how much of the end of this run's log a crash report
// carries: the log holds up to log.FileCapBytes, the native agent's lines and
// --debug's among them, and a crash report must stay well under Sentry's
// 1 MB event limit, and be read into no more memory than it sends.
const crashReportLogTail = 512 << 10

// attachLogFileToSentry attaches the end of logFile, this run's log, to the
// crash report (crashReportLogTail): read through its name only while that is
// still this run's log, never whatever a symlink there points at (a root
// run's crash would otherwise send the link's target).
func attachLogFileToSentry(logFile *os.File) error {
	content, err := keployLog.ReadLogFileTail(logFile, crashReportLogTail)
	if err != nil {
		return fmt.Errorf("error reading log file: %s", err.Error())
	}

	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetExtra("logfile", string(content))
	})
	sentry.Flush(time.Second * 5)
	return nil
}

// HandleRecovery handles the common logic for recovering from a panic.
func HandleRecovery(logger *zap.Logger, r interface{}, errMsg string) {
	// With no crash-report client (telemetry off, or not yet resolved: see
	// ResolveCrashReports) nothing is reported, so the log is not read into
	// memory for a report that goes nowhere.
	if sentry.CurrentHub().Client() != nil {
		err := attachLogFileToSentry(LogFile)
		if err != nil {
			LogError(logger, err, "failed to attach log file to sentry")
		}
		sentry.CaptureException(errors.New(fmt.Sprint(r)))
	}
	// Get the stack trace
	stackTrace := debug.Stack()
	LogError(logger, nil, errMsg, zap.String("stack trace", string(stackTrace)))
}

// Recover recovers from a panic and logs the stack trace to Sentry.
// It also stops the global context.
func Recover(logger *zap.Logger) {
	if logger == nil {
		fmt.Println(Emoji + "Failed to recover from panic. Logger is nil.")
		return
	}
	// No flush on a clean return: Recover is deferred on hot goroutines, and a
	// flush waits on the process's one Sentry transport worker. Only a
	// recovered panic has an event to flush (below).
	if r := recover(); r != nil {
		HandleRecovery(logger, r, "Recovered from panic")
		// Before Stop: the process may exit as soon as the global context ends.
		sentry.Flush(2 * time.Second)
		err := Stop(logger, fmt.Sprintf("Recovered from: %s", r))
		if err != nil {
			LogError(logger, err, "failed to stop the global context")
		}
	}
}

// GenerateGithubActions generates a GitHub Actions workflow file for Keploy
func GenerateGithubActions(logger *zap.Logger, appCmd string) {
	// Determine the path based on the alias "keploy"
	logger.Debug("Generating GitHub Actions workflow file")
	// Define the content of the GitHub Actions workflow file
	actionsFileContent := `name: Keploy
on:
  push:
    branches:
      - main
  pull_request:
    types: [opened, reopened, synchronize]
jobs:
  e2e-test:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v4
      - name: Test-Report
        uses: keploy/testgpt@main
        with:
          working-directory: ./
          keploy-path: ./
          command: ` + appCmd + `
`

	// Define the file path where the GitHub Actions workflow file will be saved
	filePath := "/githubactions/keploy.yml"

	//create the file path
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		logger.Error("Error creating directory for GitHub Actions workflow file", zap.Error(err))
		return
	}
	// Write the content to the file
	if err := os.WriteFile(filePath, []byte(actionsFileContent), 0644); err != nil {
		logger.Error("Error writing GitHub Actions workflow file", zap.Error(err))
		return
	}

	logger.Info("GitHub Actions workflow file generated successfully", zap.String("path", filePath))
}

// GetLatestGitHubRelease fetches the latest version and release body from GitHub releases with a timeout.
func GetLatestGitHubRelease(ctx context.Context, logger *zap.Logger) (GitHubRelease, error) {
	// GitHub repository details
	repoOwner := "keploy"
	repoName := "keploy"

	return getGitHubRelease(ctx, logger, fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName))
}

// getGitHubRelease reads the release at apiURL. Anything but a 200 carrying a
// tag is an error: a rate-limited answer decodes to an empty release, which
// read as a version other than the running one -- an update to "".
func getGitHubRelease(ctx context.Context, logger *zap.Logger, apiURL string) (GitHubRelease, error) {
	client := http.Client{
		Timeout: 4 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return GitHubRelease{}, err
	}

	resp, err := client.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return GitHubRelease{}, ErrGitHubAPIUnresponsive
		}
		return GitHubRelease{}, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			LogError(logger, err, "failed to close response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return GitHubRelease{}, fmt.Errorf("GitHub answered %s for %s", resp.Status, apiURL)
	}
	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return GitHubRelease{}, err
	}
	if release.TagName == "" {
		return GitHubRelease{}, fmt.Errorf("GitHub's answer for %s names no release", apiURL)
	}
	return release, nil
}

// ExtractCmdTypeFromArgs pulls an explicit --cmd-type out of raw argv.
//
// ShouldReexecWithSudo has to decide whether keploy needs root before cobra
// has parsed anything and before any config file is read, so it cannot ask
// the resolved config what the command type is. Returns "" when the flag is
// absent; the caller decides what an unrecognised value means.
func ExtractCmdTypeFromArgs(args []string) string {
	// Both spellings: cli/provider's aliasNormalizeFunc maps cmdType ->
	// cmd-type, so cobra accepts --cmdType and Changed("cmd-type") is true
	// for it. Matching only the kebab form would let the camelCase spelling
	// silently skip this privilege decision — and the repo's own CI scripts
	// use camelCase aliases throughout.
	return strings.ToLower(strings.TrimSpace(extractFlagFromArgs(args, "--cmd-type", "--cmdType")))
}

// ExtractFromContainerFromArgs pulls an explicit --from-container out of raw
// argv, for the same reason ExtractCmdTypeFromArgs exists: the flag implies a
// docker run, and therefore root, and that has to be known before cobra parses.
//
// Without this, `keploy mock record --from-container app` with no --cmd-type
// starts unprivileged and dies later writing /proc/sys/kernel/perf_event_paranoid.
func ExtractFromContainerFromArgs(args []string) string {
	// NOT lowercased: this is a container name, and docker names are
	// case-sensitive. ExtractCmdTypeFromArgs lowercases because kind names are
	// lowercase by definition.
	return strings.TrimSpace(extractFlagFromArgs(args, "--from-container", "--fromContainer"))
}

// extractFlagFromArgs returns the value of the last occurrence of any of the
// given flag names in raw argv, in either `--flag value` or `--flag=value`
// form. Returns "" when absent.
func extractFlagFromArgs(args []string, names ...string) string {
	// Last occurrence wins, matching how cobra resolves a repeated flag.
	found := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break // everything after the terminator is positional
		}
		for _, name := range names {
			if arg == name {
				if i+1 < len(args) {
					found = args[i+1]
					i++
				} else {
					found = ""
				}
				break
			}
			if strings.HasPrefix(arg, name+"=") {
				found = arg[len(name)+1:]
				break
			}
		}
	}
	return found
}

// keployValueLongFlags lists known value-taking keploy CLI long flags
// (normalized by removing hyphens and lowercasing so both kebab-case and
// camelCase spellings match). Used by ExtractCommandFromArgs so an argument
// value starting with "-c" (e.g. `--app-name -ci`) is consumed by its owning
// flag rather than mistaken for the `-c` shorthand. Flags with NoOptDefVal
// (such as boolean flags and `--enable-sampling`) must NOT be listed here
// because pflag never consumes the following argv token for them. Unknown
// flags are left unskipped so newly added boolean flags before `-c` still work.
var keployValueLongFlags = map[string]bool{
	"allowegress":            true,
	"apitimeout":             true,
	"appid":                  true,
	"appname":                true,
	"basepath":               true,
	"builddelay":             true,
	"cajavahome":             true,
	"clientlogfile":          true,
	"clientpid":              true,
	"clientpidns":            true,
	"cmdtype":                true,
	"configpath":             true,
	"consumerstallgrace":     true,
	"containername":          true,
	"coveragereport":         true,
	"coveragereportpath":     true,
	"delay":                  true,
	"dnsport":                true,
	"dockerhost":             true,
	"driven":                 true,
	"flakycheckretry":        true,
	"format":                 true,
	"fromcontainer":          true,
	"grpcport":               true,
	"halfclosegrace":         true,
	"healthpath":             true,
	"healthpolltimeout":      true,
	"healthscheme":           true,
	"healthurl":              true,
	"host":                   true,
	"incomingproxyport":      true,
	"jacocoagentpath":        true,
	"keploycontainer":        true,
	"keploynetwork":          true,
	"language":               true,
	"maxfailattempts":        true,
	"maxmemoryperconn":       true,
	"maxmemoryperconnection": true,
	"memorylimit":            true,
	"metadata":               true,
	"mincoverage":            true,
	"mode":                   true,
	"mongopassword":          true,
	"name":                   true,
	"networkname":            true,
	"onmiss":                 true,
	"output":                 true,
	"passthroughports":       true,
	"path":                   true,
	"port":                   true,
	"protodir":               true,
	"protofile":              true,
	"protoinclude":           true,
	"proxyport":              true,
	"queuesize":              true,
	"recordtimer":            true,
	"reportpath":             true,
	"run1":                   true,
	"run2":                   true,
	"runonly":                true,
	"selectedtests":          true,
	"serverport":             true,
	"services":               true,
	"sseport":                true,
	"storageformat":          true,
	"tc":                     true,
	"teardownprogressfile":   true,
	"testcase":               true,
	"testrun":                true,
	"tests":                  true,
	"testsets":               true,
	"tlsprivatekeypath":      true,
	"tokenfile":              true,
	"upstreamtlscacert":      true,
	"upstreamtls.cacert":     true,
	"urlmethods":             true,
}

var keployValueShortFlags = map[string]bool{
	"-b": true, "-d": true, "-l": true, "-n": true,
	"-o": true, "-p": true, "-r": true, "-s": true, "-t": true,
}

func isKnownKeployValueFlag(arg string) bool {
	if strings.Contains(arg, "=") {
		return false
	}
	if strings.HasPrefix(arg, "--") {
		key := strings.ToLower(strings.ReplaceAll(arg[2:], "-", ""))
		return keployValueLongFlags[key]
	}
	return keployValueShortFlags[arg]
}

// ExtractCommandFromArgs returns the value of -c/--command in raw argv, read
// the way cobra reads it: the last occurrence wins, the shorthand takes its
// value as the next argument or joined to it (-cVALUE, -c=VALUE), and nothing
// after the "--" terminator is a flag. Returns "" when absent.
//
// Known value-taking keploy flags skip their following argument so values
// beginning with "-c" (e.g. `--app-name -ci`) are not misread as the `-c`
// shorthand.
//
// ShouldReexecWithSudo decides from this value whether keploy needs root,
// before cobra parses anything. Reading another value than the one cobra runs
// (the first of two -c, or none for -cVALUE) re-executes a native command
// under sudo, or starts a docker command unprivileged, which then fails
// writing /proc/sys/kernel/perf_event_paranoid.
func ExtractCommandFromArgs(args []string) string {
	found := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return found
		case arg == "-c" || arg == "--command":
			found = ""
			if i+1 < len(args) {
				i++
				found = args[i]
			}
		case strings.HasPrefix(arg, "--command="):
			found = arg[len("--command="):]
		case strings.HasPrefix(arg, "-c"):
			// pflag drops the "=" of -c=VALUE only when a value follows it:
			// a bare -c= is the shorthand with the value "=".
			found = arg[len("-c"):]
			if len(found) > 1 && found[0] == '=' {
				found = found[1:]
			}
		case isKnownKeployValueFlag(arg):
			if i+1 < len(args) {
				i++
			}
		}
	}
	return found
}

// FindDockerCmd checks if the cli is related to docker or not, it also returns if it is a docker compose file.
//
// Podman's spellings are the same kinds: its CLI takes Docker's run and start
// flags, and `podman compose` runs a compose provider. Which engine a command
// runs on is engine.Detect's to say; a build that cannot drive Podman refuses
// the command (engine.Supported), rather than running it as a native process
// that keploy would capture nothing from.
func FindDockerCmd(cmd string) CmdType {
	if cmd == "" {
		return Empty
	}
	// Lowercased, and with its words joined by one space: `docker  run` and
	// `docker<TAB>run` are `docker run` to the shell, and so to keploy.
	cmdLower := strings.Join(strings.Fields(strings.ToLower(cmd)), " ")

	// Define patterns for Docker and Docker Compose
	dockerRunPatterns := []string{"docker run", "sudo docker run", "docker container run", "sudo docker container run",
		"podman run", "sudo podman run", "podman container run", "sudo podman container run"}
	dockerStartPatterns := []string{"docker start", "sudo docker start", "docker container start", "sudo docker container start",
		"podman start", "sudo podman start", "podman container start", "sudo podman container start"}
	dockerComposePatterns := []string{"docker-compose", "sudo docker-compose", "docker compose", "sudo docker compose",
		"podman-compose", "sudo podman-compose", "podman compose", "sudo podman compose"}

	// Check for Docker Compose command patterns and file extensions
	for _, pattern := range dockerComposePatterns {
		if strings.HasPrefix(cmdLower, pattern) {
			return DockerCompose
		}
	}
	// Check for Docker start command patterns
	for _, pattern := range dockerStartPatterns {
		if strings.HasPrefix(cmdLower, pattern) {
			return DockerStart
		}
	}
	// Check for Docker run command patterns
	for _, pattern := range dockerRunPatterns {
		if strings.HasPrefix(cmdLower, pattern) {
			return DockerRun
		}
	}
	return Native
}

// RunSubcommandEnd is the offset in cmd just past the run subcommand of a
// docker or podman (by base name), taken as `run` or `container run` right
// after the engine word, where the flags for the container it starts go; -1
// when there is none. A docker-named word without that after it (the value of
// sudo's -u, a directory) is passed over.
func RunSubcommandEnd(cmd string) int {
	type word struct {
		text string
		end  int
	}
	var words []word
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	for i := 0; i < len(cmd); {
		for i < len(cmd) && isSpace(cmd[i]) {
			i++
		}
		from := i
		for i < len(cmd) && !isSpace(cmd[i]) {
			i++
		}
		if i > from {
			words = append(words, word{cmd[from:i], i})
		}
	}
	for i, w := range words {
		base := strings.ToLower(w.text[strings.LastIndexAny(w.text, `/\`)+1:])
		if base = strings.TrimSuffix(base, ".exe"); base != "docker" && base != "podman" {
			continue
		}
		next := i + 1
		if next < len(words) && strings.EqualFold(words[next].text, "container") {
			next++
		}
		if next < len(words) && strings.EqualFold(words[next].text, "run") {
			return words[next].end
		}
	}
	return -1
}

type CmdType string

// CmdType constants
const (
	DockerRun     CmdType = "docker-run"
	DockerStart   CmdType = "docker-start"
	DockerCompose CmdType = "docker-compose"
	// FromContainer takes an ALREADY RUNNING container and re-creates it under
	// keploy's namespaces through the Docker Engine API, rather than being
	// handed a command string to splice flags into and shell out. It is the
	// only kind with no user command at all - see HasUserCommand.
	FromContainer CmdType = "from-container"
	Native        CmdType = "native"
	Empty         CmdType = ""
)

// Keys returns an array containing the keys of the given map.
func Keys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

//func FetchHomeDirectory(isNewConfigPath bool) string {
//	var configFolder = "/.keploy-config"
//
//	if isNewConfigPath {
//		configFolder = "/.keploy"
//	}
//	if runtime.GOOS == "windows" {
//		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
//		if home == "" {
//			home = os.Getenv("USERPROFILE")
//		}
//		return home + configFolder
//	}
//
//	return os.Getenv("HOME") + configFolder
//}

func GetAbsPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return absPath, nil
}

func GetCurrentBinaryPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}

	// Resolve the full path (to avoid issues with symbolic links)
	executablePath, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}

	return executablePath, nil
}

func ToAbsPath(logger *zap.Logger, originalPath string) string {
	path := originalPath
	//if user provides relative path
	if len(path) > 0 && path[0] != '/' {
		absPath, err := filepath.Abs(path)
		if err != nil {
			LogError(logger, err, "failed to get the absolute path from relative path")
		}
		path = absPath
	} else if len(path) == 0 { // if user doesn't provide any path
		cdirPath, err := os.Getwd()
		if err != nil {
			LogError(logger, err, "failed to get the path of current directory")
		}
		path = cdirPath
	}
	path += "/keploy"
	return path
}

// MkdirAllForUser creates path and any missing parent directories with perm
// using os.OpenRoot (refusing symlinks escaping the anchor directory). When
// running under sudo, it restores ownership of only the directories newly
// created by this call to the invoking user via open file descriptors in
// bottom-up order, avoiding symlink-following chown races as root.
func MkdirAllForUser(logger *zap.Logger, path string, perm os.FileMode) error {
	if path == "" {
		return os.MkdirAll(path, perm)
	}
	clean := filepath.Clean(path)
	perm = perm.Perm()

	var base string
	var relParts []string
	if !filepath.IsAbs(clean) {
		parts := strings.Split(filepath.ToSlash(clean), "/")
		i := 0
		for i < len(parts) && (parts[i] == "." || parts[i] == "..") {
			i++
		}
		if i == 0 {
			base = "."
		} else {
			base = filepath.FromSlash(strings.Join(parts[:i], "/"))
		}
		relParts = parts[i:]
	} else {
		parent := filepath.Dir(clean)
		if parent == clean {
			// Filesystem root already exists.
			return nil
		}
		relParts = append(relParts, filepath.Base(clean))
		base = parent
		for {
			if _, err := os.Lstat(base); err == nil {
				break
			}
			nextParent := filepath.Dir(base)
			if nextParent == base {
				break
			}
			relParts = append(relParts, filepath.Base(base))
			base = nextParent
		}
		for i, j := 0, len(relParts)-1; i < j; i, j = i+1, j-1 {
			relParts[i], relParts[j] = relParts[j], relParts[i]
		}
	}

	r, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	if logger == nil {
		logger = zap.NewNop()
	}

	type createdDir struct {
		f    *os.File
		path string
	}
	var created []createdDir
	defer func() {
		for i := len(created) - 1; i >= 0; i-- {
			RestoreFileOwnershipOf(logger, created[i].f, created[i].path)
			_ = created[i].f.Close()
		}
	}()

	cur := r
	defer func() {
		if cur != nil && cur != r {
			_ = cur.Close()
		}
	}()
	at := base
	for _, seg := range relParts {
		if seg == "" || seg == "." {
			continue
		}
		mkErr := cur.Mkdir(seg, perm)
		didCreate := mkErr == nil
		next, err := cur.OpenRoot(seg)
		if err != nil {
			if mkErr != nil && !errors.Is(mkErr, fs.ErrExist) {
				return mkErr
			}
			return err
		}
		nextPath := filepath.Join(at, seg)
		if didCreate {
			if linfo, lerr := cur.Lstat(seg); lerr == nil && linfo.IsDir() && linfo.Mode()&os.ModeSymlink == 0 {
				if f, ferr := cur.Open(seg); ferr == nil {
					if finfo, serr := f.Stat(); serr == nil && os.SameFile(linfo, finfo) {
						created = append(created, createdDir{f: f, path: nextPath})
					} else {
						_ = f.Close()
					}
				}
			}
		}
		if cur != r {
			_ = cur.Close()
		}
		cur, at = next, nextPath
	}
	return nil
}

// makeDirectory creates a directory if not exists with standard permissions (0o755)
func makeDirectory(path string) error {
	return MkdirAllForUser(nil, path, 0o755)
}

// SetCoveragePath takes a goCovPath and sets the coverage path accordingly.
// It returns an error if the path is a file or if the path does not exist.
func SetCoveragePath(logger *zap.Logger, goCovPath string) (string, error) {
	if goCovPath == "" {
		// Calculate the current path and create a coverage-reports directory
		currentPath, err := GetAbsPath("")
		if err != nil {
			LogError(logger, err, "failed to get the current working directory")
			return "", err
		}
		goCovPath = currentPath + "/coverage-reports"
		if err := MkdirAllForUser(logger, goCovPath, 0o755); err != nil {
			LogError(logger, err, "failed to create coverage-reports directory", zap.String("CoverageReportPath", goCovPath))
			return "", err
		}
		return goCovPath, nil
	}

	goCovPath, err := GetAbsPath(goCovPath)
	if err != nil {
		LogError(logger, err, "failed to get the absolute path for the coverage report path", zap.String("CoverageReportPath", goCovPath))
		return "", err
	}
	// Check if the path is a directory
	dirInfo, err := os.Stat(goCovPath)
	if err != nil {
		if os.IsNotExist(err) {
			LogError(logger, err, "the provided path does not exist", zap.String("CoverageReportPath", goCovPath))
			return "", err
		}
		LogError(logger, err, "failed to check the coverage report path", zap.String("CoverageReportPath", goCovPath))
		return "", err
	}
	if !dirInfo.IsDir() {
		msg := "the coverage report path is not a directory. Please provide a valid path to a directory for go coverage reports"

		LogError(logger, nil, msg, zap.String("CoverageReportPath", goCovPath))
		return "", errors.New("the path provided is not a directory")
	}

	return goCovPath, nil
}

type ErrType string

// ErrType constants to get the type of error, during init or runtime
const (
	Init    ErrType = "init"
	Runtime ErrType = "runtime"
)

type CmdError struct {
	Type ErrType
	Err  error
}

// InterruptProcessTree interrupts an entire process tree using the given signal
func InterruptProcessTree(logger *zap.Logger, ppid int, sig syscall.Signal) error {
	_, _, err := interruptProcessTree(logger, ppid, sig, nil, false, nil)
	return err
}

// InterruptProcessTreeKilled is InterruptProcessTree, and says why it had to
// kill, "" when it did not: a process group's leader had not exited
// TreeInterruptGrace after sig, and the group was sent SIGKILL; or, on
// Windows, the tree had not exited windowsInterruptGrace after its
// CTRL_BREAK, and was force-killed (taskkill /F). A stop that could not tell
// that the tree had exited without a kill says so too: one whose signal could
// not be sent, whose force-kill failed, or whose state it could not read. As
// InterruptProcessTree, it waits for each group until no process of it runs
// (groupRunning): a leader that has exited can leave processes in its group
// that run on, and a group of zombies only has exited.
func InterruptProcessTreeKilled(logger *zap.Logger, ppid int, sig syscall.Signal) (killed string, err error) {
	killed, _, err = interruptProcessTree(logger, ppid, sig, nil, false, nil)
	return killed, err
}

// Reached is a process that a signal sent to its process group reached: its
// pid and its start time (in clock ticks after boot, as /proc/<pid>/stat
// gives it), which together name it while it runs, and whether it ignored
// the signal when the signal was sent.
type Reached struct {
	PID     int
	Start   uint64
	Ignores bool
}

// InterruptProcessTreeReached is InterruptProcessTreeKilled, and also says
// which processes its signals reached: each process of a group it signalled,
// as /proc showed that group just before its signal, and whether the process
// ignored the signal then. A process the group gained as /proc was read is
// not among them. Off Linux it says none.
func InterruptProcessTreeReached(logger *zap.Logger, ppid int, sig syscall.Signal) (killed string, reached []Reached, err error) {
	return interruptProcessTree(logger, ppid, sig, groupReached, false, nil)
}

// InterruptProcessTreeStop is InterruptProcessTreeReached for a stop that
// waits on the app's exit, not for a number of seconds (a test set's end, a
// recording's end, under the proven switch): it waits for each process its
// signal reached in each group to exit, on its exit event (Linux,
// stop_events_linux.go), and kills
// a group only when it made no progress on its way out for StopStallBudget
// of the time both it and keploy could run (ExitClock). A starved group,
// whether the host is or only the group is, takes as long as it takes.
// Elsewhere it is InterruptProcessTreeReached, with its grace.
//
// Each step a group takes on its way out is a movement of moved, the
// teardown scope of the run whose app it stops (nil: none).
func InterruptProcessTreeStop(logger *zap.Logger, ppid int, sig syscall.Signal, moved *TeardownScope) (killed string, reached []Reached, err error) {
	return interruptProcessTree(logger, ppid, sig, groupReached, true, moved)
}

// groupStopByEvents, where it is set (Linux), is how InterruptProcessTreeStop
// waits for a group it signalled: until each of its members exits, while they
// (as the signal reached them) make progress. It kills the group on a stall,
// or at the user's Ctrl+C, and says why ("" when it did not kill).
// Each step the group takes on its way out is a movement of moved.
var groupStopByEvents func(logger *zap.Logger, leader int, members []Reached, moved *TeardownScope) (killed string)

// TreeInterruptGrace is how long InterruptProcessTree waits for each process
// group of the tree to exit (no process of it runs) after its signal before
// it sends SIGKILL.
const TreeInterruptGrace = 3 * time.Second

// windowsInterruptGrace is how long InterruptProcessTree waits on Windows for
// the tree to exit after its CTRL_BREAK before it ends it with taskkill.
const windowsInterruptGrace = 10 * time.Second

// interruptProcessTree is the work of the four above. snapshot, when set,
// reads which processes a group's signal is about to reach; byEvents (with
// snapshot) has it wait for each group on its exit event, under the stall
// rule, rather than for TreeInterruptGrace.
func interruptProcessTree(logger *zap.Logger, ppid int, sig syscall.Signal, snapshot func(pgid int, sig syscall.Signal) []Reached, byEvents bool, moved *TeardownScope) (killed string, reached []Reached, err error) {
	// Windows: /proc and process group semantics don't exist. Prefer console ctrl events, fallback to taskkill.
	if runtime.GOOS == "windows" {
		logger.Debug("Interrupting process tree on windows", zap.Int("pid", ppid), zap.String("signal", sig.String()))

		// List ALL descendant processes (recursively) before attempting to kill
		listAllCmd := exec.Command("powershell", "-Command",
			fmt.Sprintf(`$parent = %d
$processes = Get-CimInstance Win32_Process
function Get-ChildProcesses($parentId) {
    $children = $processes | Where-Object {$_.ParentProcessId -eq $parentId}
    foreach ($child in $children) {
        $child
        Get-ChildProcesses $child.ProcessId
    }
}
Get-ChildProcesses $parent | Select-Object ProcessId, ParentProcessId, Name | Format-List`, ppid))
		// Ids and names only, as the listing after the kill: a process's
		// command line can carry a credential (an inline -e TOKEN=...).
		if output, err := listAllCmd.CombinedOutput(); err == nil {
			logger.Debug("ALL descendant processes before kill", zap.Int("parent_pid", ppid), zap.String("output", string(output)))
		} else {
			logger.Debug("Failed to list descendant processes", zap.Int("pid", ppid), zap.Error(err), zap.String("output", string(output)))
		}

		// Prefer a console Ctrl+Break event so the app can exit cleanly.
		// why is what a kill from here on is said with.
		why := fmt.Sprintf("it had not exited %s after keploy's CTRL_BREAK (the grace keploy's stop gives it on Windows), so keploy killed it", windowsInterruptGrace)
		if sig == syscall.SIGINT {
			if err := SendSignal(logger, -ppid, sig); err == nil {
				if err := waitForProcessExit(ppid, windowsInterruptGrace, logger); err != nil {
					logger.Error("error waiting for process to exit", zap.Int("pid", ppid), zap.Error(err))
				}
				if running, err := isProcessRunning(ppid); err == nil && !running {
					return "", nil, nil
				}
			} else {
				logger.Debug("failed to send console ctrl event", zap.Int("pid", ppid), zap.Error(err))
				why = fmt.Sprintf("keploy could not send it a CTRL_BREAK (%v), so keploy killed it", err)
			}
		} else {
			why = fmt.Sprintf("keploy cannot send %s on Windows, so keploy killed it", sig)
		}

		// Try graceful taskkill as a fallback (without /F).
		cmd := exec.Command("taskkill", "/PID", strconv.Itoa(ppid), "/T")
		if output, err := cmd.CombinedOutput(); err != nil {
			logger.Debug("taskkill graceful failed", zap.Int("pid", ppid), zap.Error(err), zap.String("output", string(output)))
		} else {
			logger.Debug("taskkill graceful succeeded", zap.Int("pid", ppid), zap.String("output", string(output)))
		}

		// Force kill as the last resort if the process tree is still alive,
		// or its state cannot be read: either way the stop says it killed,
		// as it cannot tell that the tree exited on its own. So does a force
		// kill that failed.
		running, err := isProcessRunning(ppid)
		if err != nil {
			logger.Debug("failed to check process state after graceful taskkill", zap.Int("pid", ppid), zap.Error(err))
			why = fmt.Sprintf("%s; and keploy could not read whether it had exited (%v)", strings.TrimSuffix(why, ", so keploy killed it"), err) + ", so keploy killed it"
		}
		if running || err != nil {
			killed = why
			forcedCmd := exec.Command("taskkill", "/PID", strconv.Itoa(ppid), "/T", "/F")
			if output, ferr := forcedCmd.CombinedOutput(); ferr != nil {
				logger.Error("taskkill forced failed", zap.Int("pid", ppid), zap.Error(ferr), zap.String("output", string(output)))
				killed = fmt.Sprintf("%s, and its taskkill /F failed (%v)", why, ferr)
			} else {
				logger.Debug("taskkill forced succeeded", zap.Int("pid", ppid), zap.String("output", string(output)))
			}
		}

		// Check for remaining descendant processes after kill
		verifyAllCmd := exec.Command("powershell", "-Command",
			fmt.Sprintf(`$parent = %d
$processes = Get-CimInstance Win32_Process
function Get-ChildProcesses($parentId) {
    $children = $processes | Where-Object {$_.ParentProcessId -eq $parentId}
    foreach ($child in $children) {
        $child
        Get-ChildProcesses $child.ProcessId
    }
}
Get-ChildProcesses $parent | Select-Object ProcessId, ParentProcessId, Name | Format-List`, ppid))
		if output, err := verifyAllCmd.CombinedOutput(); err == nil {
			logger.Debug("ALL descendant processes after kill", zap.Int("parent_pid", ppid), zap.String("output", string(output)))
		}

		// Wait a short while for processes to exit (same helper used elsewhere)
		if err := waitForProcessExit(ppid, 3*time.Second, logger); err != nil {
			logger.Error("error waiting for process to exit", zap.Int("pid", ppid), zap.Error(err))
		}
		return killed, nil, nil
	}

	// Non-windows (existing logic)
	logger.Debug("Interrupting process tree", zap.Int("pid", ppid), zap.String("signal", sig.String()))

	var uniqueProcess []int
	children, groupOf, err := processTree(ppid)
	if err != nil {
		// The tree cannot be read: its leader's own group (keploy starts
		// the app in a group of its own) is still interrupted, waited for
		// and, if need be, killed, as below. Returning here sent no signal
		// at all, and the app was killed when the command's WaitDelay ran
		// out, unsaid. A process of the tree in a group of its own is then
		// not interrupted.
		logger.Warn("keploy could not read the application's process tree; it interrupts the application's own process group only",
			zap.Int("pid", ppid), zap.Error(err))
		uniqueProcess = []int{ppid}
	} else {
		children = append(children, ppid)

		logger.Debug("Found child PIDs", zap.Ints("children", children))

		uniqueProcess, err = uniqueProcessGroups(children, groupOf)
		if err != nil {
			logger.Error("failed to find unique process groups", zap.Int("pid", ppid), zap.Error(err))
			uniqueProcess = children
		}
	}

	logger.Debug("Unique process groups", zap.Ints("uniqueProcess", uniqueProcess))

	// Send signal to interrupt each process and wait for them to exit one by one
	for _, pid := range uniqueProcess {
		var members []Reached
		if snapshot != nil {
			members = snapshot(pid, sig)
		}
		err := signalGroup(logger, -pid, sig)
		if err != nil {
			logger.Error("error sending signal to the process group id", zap.Int("pgid", pid), zap.Error(err))
			// The group was not asked to stop: what ends it now (the
			// command's WaitDelay kills it) is no exit of its own.
			if killed == "" {
				killed = fmt.Sprintf("its process group %d could not be signalled (%v), so keploy could not let it stop on its own", pid, err)
			}
			continue
		}
		reached = append(reached, members...)
		// A stop that waits on the app's exit kills only a group that
		// stopped making progress on its way out.
		if byEvents && snapshot != nil && groupStopByEvents != nil {
			if why := groupStopByEvents(logger, pid, members, moved); why != "" && killed == "" {
				killed = why
			}
			continue
		}
		// Wait, up to the grace, until no process of the group runs: its
		// leader, or one it left in its group, such as the server of
		// `sh -c 'server &'` whose sh no one reaps (a zombie leader). One
		// the caller was handed, as it ignored the signal, is the caller's.
		handed := handedOver(pid, members)
		waitForGroupExit(pid, handed, TreeInterruptGrace, logger)
		// If it ignored the graceful signal (or was too contention-slow to exit
		// within the wait), escalate to SIGKILL so it cannot keep holding resources
		// (e.g. a listening port) and race the next app start with
		// "address already in use" (the go-docker-timefreeze flake).
		if running, rerr := groupRunning(pid, handed); rerr != nil {
			logger.Debug("could not verify process state after graceful signal", zap.Int("pid", pid), zap.Error(rerr))
		} else if running {
			logger.Debug("process still alive after graceful signal; escalating to SIGKILL", zap.Int("pgid", pid))
			if killed == "" {
				killed = fmt.Sprintf("a process of it had not exited %s later (the grace keploy's stop gives each of its process groups), so keploy killed it", TreeInterruptGrace)
			}
			if killErr := signalGroup(logger, -pid, syscall.SIGKILL); killErr != nil {
				logger.Error("error sending SIGKILL to the process group id", zap.Int("pgid", pid), zap.Error(killErr))
			} else {
				waitForGroupExit(pid, handed, 3*time.Second, logger)
			}
		}
	}
	return killed, reached, nil
}

// processTree and signalGroup are the tree interrupt's reads of the process
// tree and its signals to a process group (on macOS, signalGroupByMembers);
// a test fails them.
var (
	processTree = findChildPIDs
	signalGroup = SendSignal
)

// groupExitPoll is how often waitForGroupExit reads whether a group runs.
const groupExitPoll = 100 * time.Millisecond

// waitForGroupExit waits until no process of process group pgid runs but
// those in except (groupRunning), or timeout has passed. A group whose state
// cannot be read is waited for no more: the caller reads it again, and says
// why.
func waitForGroupExit(pgid int, except []Reached, timeout time.Duration, logger *zap.Logger) {
	deadline := time.Now().Add(timeout)
	for {
		if running, err := groupRunning(pgid, except); err != nil || !running {
			logger.Debug("Process group exited", zap.Int("pgid", pgid), zap.Error(err))
			return
		}
		if time.Now().After(deadline) {
			logger.Debug("Timed out waiting for process group to exit", zap.Int("pgid", pgid))
			return
		}
		time.Sleep(groupExitPoll)
	}
}

// groupRunning reports whether a process of process group pgid has not
// exited: its leader, pid pgid, runs (isProcessRunning; a zombie has
// exited), or another process of its group does (groupRuns). A group counts
// as exited only when no process of it runs: a leader that has exited,
// reaped or a zombie, can leave processes in its group that run on, as
// `sh -c 'server &'` leaves its server, and those can still hold the app's
// port. A group of zombies only has exited, and costs no wait. A process of
// except (handedOver), named by its pid and start time, is not counted.
func groupRunning(pgid int, except []Reached) (bool, error) {
	if running, err := isProcessRunning(pgid); err != nil || running {
		return running, err
	}
	return groupRunsExcept(pgid, except)
}

// handedOver is what of members, the processes of group leader's that its
// signal reached (InterruptProcessTreeReached and InterruptProcessTreeStop
// return them), the stop leaves to its caller: each that ignored the
// signal, but the leader. The stop of what the app leaves behind asks those
// with SIGTERM (adopt_linux.go, stopAdopted), and its interrupt neither
// waits for them nor kills them. The leader, and every process the signal
// reached that acts on it, the interrupt waits for. Where no snapshot is
// read (InterruptProcessTreeKilled; macOS) nothing is handed over.
func handedOver(leader int, members []Reached) []Reached {
	var handed []Reached
	for _, m := range members {
		if m.Ignores && m.PID != leader {
			handed = append(handed, m)
		}
	}
	return handed
}

// waitedFor is members without what handedOver leaves to the caller.
func waitedFor(leader int, members []Reached) []Reached {
	var waited []Reached
	for _, m := range members {
		if !m.Ignores || m.PID == leader {
			waited = append(waited, m)
		}
	}
	return waited
}

// waitForProcessExit waits for the process to exit or times out after the specified duration
func waitForProcessExit(pid int, timeout time.Duration, logger *zap.Logger) error {
	// Create a timeout channel
	timeoutCh := time.After(timeout)

	// Loop to check if the process is running
	for {
		select {
		case <-timeoutCh:
			// If the timeout is reached, log the timeout and break
			logger.Debug("Timed out waiting for process to exit", zap.Int("pid", pid))
			return nil
		default:
			// Check if the process is running
			isRunning, err := isProcessRunning(pid)
			if err != nil {
				return err
			}

			if !isRunning {
				logger.Debug("Process exited", zap.Int("pid", pid))
				return nil
			}

			// Wait for 1 second before checking again
			time.Sleep(300 * time.Millisecond)
		}
	}
}

// isProcessRunning checks if a particular process is still running using os.FindProcess
func isProcessRunning(pid int) (bool, error) {
	// On Windows use PowerShell to check PID presence because signalling with 0 fails.
	if runtime.GOOS == "windows" {
		// Use exit code instead of parsing output to avoid locale-dependent string matching.
		// PowerShell exits with 0 when process exists, 1 when not found.
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			fmt.Sprintf("if (Get-Process -Id %d -ErrorAction SilentlyContinue) { exit 0 } else { exit 1 }", pid))
		err := cmd.Run()
		if err != nil {
			// Non-zero exit code means process not found
			if exitError, ok := err.(*exec.ExitError); ok && exitError.ExitCode() != 0 {
				return false, nil
			}
			// Other errors (like command not found) are treated as errors
			return false, err
		}
		// Exit code 0 means process was found
		return true, nil
	}

	// POSIX: try to find the process and signal 0 to check existence
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, err // Process lookup failed
	}

	// Attempt to signal the process (0 means no action, just error check)
	err = process.Signal(syscall.Signal(0))
	if err != nil {
		// If error occurs when signaling, it means the process is no longer running
		// If there’s another error, consider the process as still running
		if errors.Is(err, os.ErrProcessDone) || err == syscall.ESRCH {
			return false, nil
		}
		return true, nil
	}
	// A zombie has exited: it only waits for its parent to reap it.
	if exitedUnreaped(pid) {
		return false, nil
	}

	// No error means the process is running
	return true, nil
}

// uniqueProcessGroups returns the process group of each of pids, as groupOf
// reads it, each group once.
func uniqueProcessGroups(pids []int, groupOf func(pid int) (int, error)) ([]int, error) {
	uniqueGroups := make(map[int]bool)
	var uniqueGPIDs []int

	for _, pid := range pids {
		pgid, err := groupOf(pid)
		if err != nil {
			return nil, err
		}
		if !uniqueGroups[pgid] {
			uniqueGroups[pgid] = true
			uniqueGPIDs = append(uniqueGPIDs, pgid)
		}
	}

	return uniqueGPIDs, nil
}

// ephemeralPortRange reports the kernel's local port range and whether it could
// actually be determined.
//
// known=false means "this platform's range is unknown to us", NOT "assume
// Linux". Guessing is worse than not acting: macOS and Windows both default to
// 49152-65535, so assuming the Linux 32768-60999 would make GetAvailablePort
// allocate from 61000-65534 — entirely INSIDE the real ephemeral range on those
// platforms — while shrinking the pool from 16384 to 4535 and making collisions
// materially more likely than plain :0.
func ephemeralPortRange() (lo, hi uint32, known bool) {
	return ephemeralPortRangeFrom(os.ReadFile)
}

// ephemeralPortRangeFrom takes the file reader as a parameter so the
// unknown-platform path is testable. On Linux the read always succeeds, which
// would otherwise leave that branch dead code no test can reach — and it is the
// branch that decides whether the allocator acts at all.
func ephemeralPortRangeFrom(readFile func(string) ([]byte, error)) (lo, hi uint32, known bool) {
	b, err := readFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return 0, 0, false
	}
	return parseEphemeralPortRange(string(b))
}

// Linux's documented default, used when ip_local_port_range exists but its
// contents are malformed.
const (
	defaultEphemeralLo uint32 = 32768
	defaultEphemeralHi uint32 = 60999
)

// parseEphemeralPortRange parses the two-field content of ip_local_port_range.
// Split out from the file read so its fallbacks are testable: on a normal Linux
// box the read always succeeds, which would leave every fallback branch dead
// code no test could reach.
func parseEphemeralPortRange(content string) (lo, hi uint32, ok bool) {
	f := strings.Fields(content)
	if len(f) != 2 {
		return defaultEphemeralLo, defaultEphemeralHi, true
	}
	l, err1 := strconv.ParseUint(f[0], 10, 32)
	h, err2 := strconv.ParseUint(f[1], 10, 32)
	if err1 != nil || err2 != nil || l == 0 || h < l || h > 65535 {
		return defaultEphemeralLo, defaultEphemeralHi, true
	}
	return uint32(l), uint32(h), true
}

// GetAvailablePort finds and returns an available port on the system.
//
// It deliberately avoids the kernel's local port range, because that range is
// where EVERY bind(0) allocator on the machine draws from — including a second
// keploy process doing exactly this, and Docker's dynamic host-port publisher.
// The probe listener must be CLOSED before the number can be handed to whoever
// will actually bind it, so anything drawing from the same range can take it in
// between, and the eventual holder is a LIVE process that never releases it —
// which is why the agent's bind retry cannot recover and the run dies with
// "keploy-agent did not become ready in time".
//
// Measured on Linux 6.12 with the default range 32768-60999: 3000/3000
// bind(":0") allocations landed inside it, none above. (Outbound connections
// are NOT the thief — the kernel partitions the two, handing bind(0) odd ports
// and connect() even ones: 1500/1500 vs 1000/1000 in the same measurement. The
// port that failed in CI, 34467, is odd, i.e. it came from a bind(0)-class
// allocator, not from an outbound socket.)
//
// So candidates come from ABOVE the range's high-water mark. This NARROWS the
// window rather than closing it — nothing short of passing the listener itself
// closes it — but the port no longer competes with every other bind(0) on the
// host.
//
// Its probe listeners are plain TCP, never Go's default MPTCP: for as long as
// one is open it listens on every interface.
func GetAvailablePort() (uint32, error) {
	return availablePortAbove(hostEphemeralRange, 0)
}

// hostEphemeralRange reads the host's ephemeral port range for
// GetAvailablePort and GetAvailableContainerPort (ephemeralPortRange).
var hostEphemeralRange = ephemeralPortRange

// containerEphemeralHi is the top of a container's ephemeral port range: the
// kernel gives each new network namespace Linux's default range, 32768-60999,
// whatever the host's is.
const containerEphemeralHi = defaultEphemeralHi

// GetAvailableContainerPort is GetAvailablePort for a port a container
// listens on that the host publishes as itself (127.0.0.1:P:P), as a Docker
// agent's: free on the host, and above the container's own ephemeral range
// too. On a user-defined network, as every compose stack's is, Docker's
// embedded DNS resolver listens in the container's namespace, on 127.0.0.11,
// on a TCP and a UDP port the kernel drew from that range before the
// container's process ran. A port the CLI found free on the host could be one
// of them, and the process in the container then cannot bind it, nor move off
// it: the CLI reaches it through the publish. A host whose range ends below
// 60999, or reaches 65535, or is unknown (macOS, Windows), would otherwise
// hand out ports inside it.
func GetAvailableContainerPort() (uint32, error) {
	return availablePortAbove(hostEphemeralRange, containerEphemeralHi)
}

// availablePortAbove is GetAvailablePort with the host's ephemeral range read
// by ephemeral (ephemeralPortRange's signature), whose candidates are above
// floor too.
func availablePortAbove(ephemeral func() (lo, hi uint32, known bool), floor uint32) (uint32, error) {
	top := floor
	if _, ephemeralHi, known := ephemeral(); known && ephemeralHi < 65535 && ephemeralHi > top {
		top = ephemeralHi
	}
	if top > 0 {
		lo := top + 1
		span := 65536 - lo // inclusive of 65535
		// A random start, not a scan from the bottom. Scanning upward returns
		// the LOWEST free port every time, so two keploy processes started
		// moments apart — the ordinary CI pattern — are handed an identical
		// number, each having verified it free; whichever binds second loses.
		start := lo
		if n, err := rand.Int(rand.Reader, big.NewInt(int64(span))); err == nil {
			start = lo + uint32(n.Int64())
		}
		for i := uint32(0); i < span; i++ {
			port := lo + ((start - lo + i) % span)
			ln, err := plaintcp.Listen(context.Background(), "tcp", fmt.Sprintf(":%d", port))
			if err != nil {
				continue
			}
			// A failed Close leaves this process holding the port for its
			// lifetime, so the candidate is unusable AND leaked — return the
			// error rather than continuing and leaking one fd per candidate.
			if err := ln.Close(); err != nil {
				return 0, fmt.Errorf("failed to release probe listener on port %d: %w", port, err)
			}
			return port, nil
		}
	}

	// Either the platform's ephemeral range is unknown, or every port above it
	// is taken, or the range reaches 65535 leaving no space (with no floor).
	// Fall back to the OS assignment rather than failing: a port that may race
	// is strictly better than no port at all, and this is the historical
	// behaviour. A container's port that then meets one of the resolver's
	// fails the agent's start at its bind, as no port at all would.
	listener, err := plaintcp.Listen(context.Background(), "tcp", ":0")
	if err != nil {
		return 0, fmt.Errorf("failed to find available port: %w", err)
	}
	defer listener.Close()

	addr := listener.Addr().(*net.TCPAddr)
	return uint32(addr.Port), nil
}

// isPortAvailable checks if a specific port is available on the system, with
// a plain TCP probe listener (as GetAvailablePort's).
func isPortAvailable(port uint32) bool {
	listener, err := plaintcp.Listen(context.Background(), "tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	defer listener.Close()
	return true
}

// EnsureAvailablePorts checks if the proxy and DNS ports are available.
// If they are available, it returns them unchanged.
// If not, it allocates new available ports for them.
//
// exclude lists ports already handed out in this same setup. Nothing binds
// them yet — the agent, proxy and DNS ports are all allocated before any of
// them is used — so without this the second and third draws can legitimately
// return a port the first already claimed, and two subsystems then race for
// it. Narrowing allocation to above the ephemeral range makes the pool smaller
// and that coincidence correspondingly more likely, so the exclusion is not
// optional bookkeeping.
func EnsureAvailablePorts(port uint32, exclude ...uint32) (uint32, error) {
	taken := make(map[uint32]struct{}, len(exclude))
	for _, p := range exclude {
		taken[p] = struct{}{}
	}
	if _, clash := taken[port]; !clash && isPortAvailable(port) {
		return port, nil
	}
	var newPort uint32
	var err error
	// Bounded: a handful of draws is plenty to miss a set this small, and a
	// bound means an exhausted pool surfaces as an error rather than a hang.
	for i := 0; i < 16; i++ {
		newPort, err = GetAvailablePort()
		if err != nil {
			break
		}
		if _, clash := taken[newPort]; !clash {
			return newPort, nil
		}
	}
	if err == nil {
		return 0, fmt.Errorf("failed to allocate a port distinct from %v", exclude)
	}
	return 0, fmt.Errorf("failed to allocate new port: %w", err)
}

type shellWord struct {
	start int
	value string
}

func isShellSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// scanShellWords tokenizes a POSIX-like shell command into words with their
// starting byte offsets in cmd, respecting single quotes, double quotes, and
// backslash escapes. Returns ok=false on an unterminated quote or trailing
// backslash.
func scanShellWords(cmd string) ([]shellWord, bool) {
	var words []shellWord
	n := len(cmd)
	i := 0
	for i < n {
		for i < n {
			if isShellSpace(cmd[i]) {
				i++
				continue
			}
			if cmd[i] == '\\' && i+1 < n && cmd[i+1] == '\n' {
				i += 2
				continue
			}
			break
		}
		if i >= n {
			break
		}

		start := i
		var b strings.Builder

		for i < n {
			c := cmd[i]
			if isShellSpace(c) {
				break
			}
			switch c {
			case '\'':
				i++
				closeIdx := strings.IndexByte(cmd[i:], '\'')
				if closeIdx < 0 {
					return nil, false
				}
				b.WriteString(cmd[i : i+closeIdx])
				i += closeIdx + 1
			case '"':
				i++
				closed := false
				for i < n {
					dc := cmd[i]
					if dc == '"' {
						i++
						closed = true
						break
					}
					if dc == '\\' && i+1 < n {
						next := cmd[i+1]
						switch next {
						case '"', '\\', '$', '`':
							b.WriteByte(next)
							i += 2
							continue
						case '\n':
							i += 2
							continue
						}
					}
					b.WriteByte(dc)
					i++
				}
				if !closed {
					return nil, false
				}
			case '\\':
				if i+1 >= n {
					return nil, false
				}
				if cmd[i+1] == '\n' {
					i += 2
					continue
				}
				b.WriteByte(cmd[i+1])
				i += 2
			default:
				b.WriteByte(c)
				i++
			}
		}

		words = append(words, shellWord{start: start, value: b.String()})
	}
	return words, true
}

func shellBaseName(word string) string {
	if idx := strings.LastIndexAny(word, "/\\"); idx >= 0 {
		return word[idx+1:]
	}
	return word
}

func isEnvAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	return eq > 0 && !strings.HasPrefix(word, "-")
}

var sudoValueLongFlags = map[string]bool{
	"--user": true, "--group": true, "--host": true, "--prompt": true,
	"--close-from": true, "--chdir": true, "--chroot": true, "--role": true,
	"--type": true, "--other-user": true, "--command-timeout": true,
}

const sudoValueShortFlags = "CDRTUghprtu"

var dockerGlobalBoolFlags = map[string]bool{
	"-D": true, "--debug": true, "--tls": true, "--tlsverify": true,
	"-v": true, "--version": true, "-h": true, "--help": true,
	"-r": true, "--remote": true, "--rootless-cni": true,
	"--syslog": true, "--transient-store": true,
}

var dockerRunBoolFlags = map[string]bool{
	"--detach": true, "--rm": true, "--interactive": true, "--tty": true,
	"--privileged": true, "--init": true, "--read-only": true,
	"--publish-all": true, "--no-healthcheck": true, "--oom-kill-disable": true,
	"--disable-content-trust": true, "--quiet": true, "--help": true,
	"--replace": true, "--rmi": true, "--no-hosts": true, "--env-host": true,
	"--read-only-tmpfs": true, "--http-proxy": true, "--sig-proxy": true,
	"--use-api-socket": true, "--tls-verify": true, "--no-hostname": true,
	"--passwd": true, "--rootfs": true, "--unsetenv-all": true,
}

const dockerRunValueShortFlags = "acehlmpuvw"

func sudoFlagTakesNext(opt string) bool {
	if strings.Contains(opt, "=") {
		return false
	}
	if strings.HasPrefix(opt, "--") {
		return sudoValueLongFlags[opt]
	}
	letters := opt[1:]
	at := strings.IndexAny(letters, sudoValueShortFlags)
	return at >= 0 && at == len(letters)-1
}

func dockerRunOptionsStart(words []shellWord) int {
	i := 0
	for i < len(words) && isEnvAssignment(words[i].value) {
		i++
	}
	if i < len(words) && strings.EqualFold(shellBaseName(words[i].value), "sudo") {
		i++
		for i < len(words) && strings.HasPrefix(words[i].value, "-") && words[i].value != "-" {
			opt := words[i].value
			i++
			if opt == "--" {
				break
			}
			if sudoFlagTakesNext(opt) {
				i++
			}
		}
		for i < len(words) && isEnvAssignment(words[i].value) {
			i++
		}
	}
	if i >= len(words) {
		return -1
	}
	prog := strings.TrimSuffix(strings.ToLower(shellBaseName(words[i].value)), ".exe")
	if prog != "docker" && prog != "podman" {
		return -1
	}
	i++

	for i < len(words) && strings.HasPrefix(words[i].value, "-") && words[i].value != "-" {
		opt := words[i].value
		i++
		if opt == "--" {
			break
		}
		if strings.Contains(opt, "=") || dockerGlobalBoolFlags[opt] {
			continue
		}
		if strings.HasPrefix(opt, "--") {
			i++
		} else {
			letters := opt[1:]
			if at := strings.IndexAny(letters, "Hcl"); at >= 0 && at == len(letters)-1 {
				i++
			}
		}
	}
	if i >= len(words) {
		return -1
	}
	sub := strings.ToLower(words[i].value)
	if sub == "container" {
		i++
		if i >= len(words) || !strings.EqualFold(words[i].value, "run") {
			return -1
		}
		return i + 1
	}
	if sub == "run" {
		return i + 1
	}
	return -1
}

func dockerRunFlagTakesNext(w string) bool {
	if strings.Contains(w, "=") {
		return false
	}
	if strings.HasPrefix(w, "--") {
		return !dockerRunBoolFlags[w]
	}
	letters := w[1:]
	at := strings.IndexAny(letters, dockerRunValueShortFlags)
	return at >= 0 && at == len(letters)-1
}

func EnsureRmBeforeName(cmd string) string {
	words, ok := scanShellWords(cmd)
	if !ok {
		return cmd
	}
	optStart := dockerRunOptionsStart(words)
	if optStart < 0 {
		return cmd
	}

	hasRm := false
	nameStart := -1

	for i := optStart; i < len(words); i++ {
		w := words[i].value
		if w == "--" || w == "-" || !strings.HasPrefix(w, "-") {
			// Stop at the image name (or end-of-options marker): everything
			// after this belongs to the container command, not docker run.
			break
		}

		if w == "--rm" || strings.HasPrefix(w, "--rm=") {
			hasRm = true
			continue
		}

		if w == "--name" {
			if i+1 < len(words) {
				if nameStart == -1 && words[i+1].value != "" {
					nameStart = words[i].start
				}
				i++ // consume --name's value
			}
			continue
		}
		if val, ok := strings.CutPrefix(w, "--name="); ok {
			// Docker accepts both "--name app" and "--name=app"
			// (see dockerLongFlagValue).
			if nameStart == -1 && val != "" {
				nameStart = words[i].start
			}
			continue
		}

		if dockerRunFlagTakesNext(w) {
			i++
		}
	}

	if !hasRm && nameStart != -1 {
		return cmd[:nameStart] + "--rm " + cmd[nameStart:]
	}
	return cmd
}

// dockerLongFlagValue returns the value of the first occurrence of any of
// names in cmd, accepting both "--flag value" and "--flag=value". Docker
// accepts either spelling for every long flag, so a parser that understands
// only one of them silently reads nothing from perfectly valid commands.
func dockerLongFlagValue(cmd string, names ...string) string {
	parts := strings.Fields(cmd)
	for i, part := range parts {
		for _, name := range names {
			if part == name && i+1 < len(parts) {
				return parts[i+1]
			}
			// An empty "--flag=" is not a value: keep scanning, so it cannot
			// swallow a real occurrence later in the command.
			if value, ok := strings.CutPrefix(part, name+"="); ok && value != "" {
				return value
			}
		}
	}
	return ""
}

// ContainerNameFromDockerRun extracts the value of the --name flag from a
// `docker run` command, supporting both "--name foo" and "--name=foo". It
// returns "" when no --name is present. Used to free a leftover container name
// before a re-run when the --container-name flag wasn't supplied, and to
// resolve the container keploy must instrument.
func ContainerNameFromDockerRun(cmd string) string {
	return dockerLongFlagValue(cmd, "--name")
}

// NetworkNameFromDockerRun extracts the value of --network or --net from a
// `docker run` command, in either spelling. It returns "" when neither is
// present.
func NetworkNameFromDockerRun(cmd string) string {
	return dockerLongFlagValue(cmd, "--network", "--net")
}

func isGoBinary(logger *zap.Logger, filePath string) bool {
	f, err := elf.Open(filePath)
	if err != nil {
		logger.Debug(fmt.Sprintf("failed to open file %s", filePath), zap.Error(err))
		return false
	}
	defer func() {
		if err := f.Close(); err != nil {
			LogError(logger, err, "failed to close file", zap.String("file", filePath))
		}
	}()

	// Check for section names typical to Go binaries
	sections := []string{".go.buildinfo", ".gopclntab"}
	for _, section := range sections {
		if sect := f.Section(section); sect != nil {
			return true
		}
	}
	return false
}

// DetectLanguage detects the language of the test command and returns the executable
func DetectLanguage(logger *zap.Logger, cmd string) (models.Language, string) {
	if cmd == "" {
		return models.Unknown, ""
	}
	fields := strings.Fields(cmd)

	// Find the actual executable by skipping environment variable assignments
	executable := ""
	for _, field := range fields {
		// Skip environment variable assignments (KEY=VALUE format)
		if strings.Contains(field, "=") && !strings.HasPrefix(field, "/") {
			continue
		}
		// This is the actual executable
		executable = field
		break
	}

	if executable == "" {
		return models.Unknown, ""
	}

	// Check for Python
	pythonRegex := regexp.MustCompile(`(?i)(^|.*/)(python(\d+(\.\d+)*)?)$`)
	if pythonRegex.MatchString(executable) {
		return models.Python, executable
	}

	// Check for Node.js
	if executable == "node" || executable == "npm" || executable == "yarn" {
		return models.Javascript, executable
	}

	// Check for Java
	if executable == "java" {
		return models.Java, executable
	}

	// Check for Go
	if executable == "go" || (isGoBinary(logger, executable)) {
		return models.Go, executable
	}

	return models.Unknown, executable
}

// FileExists checks if a file exists and is not a directory at the given path.
func FileExists(path string) (bool, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return fileInfo.Mode().IsRegular(), nil
}

// ExpandPath expands a given path, replacing the tilde with the user's home directory
func ExpandPath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		homeDir, err := getHomeDir()
		if err != nil {
			return "", err
		}
		return strings.Replace(path, "~", homeDir, 1), nil
	}
	return path, nil
}

// getHomeDir retrieves the appropriate home directory based on the execution context
func getHomeDir() (string, error) {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if usr, err := user.Lookup(sudoUser); err == nil {
			return usr.HomeDir, nil
		}
	}
	// Fallback to the current user's home directory
	if usr, err := user.Current(); err == nil {
		return usr.HomeDir, nil
	}
	// Fallback if neither method works
	return "", errors.New("failed to retrieve current user info")
}

func IsDockerCmd(kind CmdType) bool {
	return (kind == DockerRun || kind == DockerStart || kind == DockerCompose || kind == FromContainer)
}

// HasUserCommand reports whether this kind carries a command string that keploy
// runs through a shell. Every kind did until FromContainer, which launches the
// app through the Docker Engine API instead - so the checks written against
// "there is a command here" (detach-mode scanning, name extraction, the
// wrapped-command exec itself) have nothing to act on and must be skipped
// rather than run against an empty string.
func HasUserCommand(kind CmdType) bool {
	return kind != FromContainer && kind != Empty
}

// PermissionError holds information about files/directories with permission issues
type PermissionError struct {
	Path     string
	OwnerUID uint32
	IsRead   bool // true if it's a read permission issue, false if write
}

// AddToGitIgnore adds ignoreString to path's .gitignore, if it is not there
// yet. The file is opened inside path, and path from the project
// (OpenRecordingDir): keploy/.gitignore comes with a cloned repository, so
// one that links out of path, or a path that links out of the project, is
// refused, with why, and what it leads to is not written. A link that stays
// inside is followed.
func AddToGitIgnore(logger *zap.Logger, path string, ignoreString string) error {
	root, err := OpenRecordingDir(path)
	if err != nil {
		return fmt.Errorf("error opening the folder of the .gitignore file: %w", err)
	}
	defer func() { _ = root.Close() }()

	file, err := root.OpenFile(".gitignore", os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		refused := fmt.Sprintf("the .gitignore file is written only inside %q", path)
		if out := linkout.Explain(root, path, ".gitignore", refused, err); out != nil {
			return out
		}
		return fmt.Errorf("%s: %w", refused, err)
	}

	defer func() {
		if err := file.Close(); err != nil {
			logger.Error("error closing .gitignore file", zap.Error(err))
		}
	}()

	scanner := bufio.NewScanner(file)
	found := false
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == ignoreString {
			found = true
			break
		}
	}

	if !found {
		if _, err := file.WriteString("\n" + ignoreString + "\n"); err != nil {
			return fmt.Errorf("error writing to .gitignore file: %v", err)
		}
	}

	return nil
}

// GitIgnores reports whether git ignores path, a path inside dir (relative
// to dir, or absolute), as `git add` would read it: by every .gitignore that
// applies, a later entry that un-ignores it (a "!*.yaml") included, the
// repository's info/exclude, and the global excludes of the HOME keploy runs
// with. known is false when git cannot say (it is not installed, dir is in no
// git work tree, or git refused or took too long), and then ignored says
// nothing. A path git tracks already is not ignored. Under sudo git runs as
// the user sudo ran keploy for (asInvokingUser): git refuses, as root, a work
// tree another user owns. sudo usually keeps root's HOME, so that user's own
// global excludes are then not read: they can only ignore more, never
// un-ignore a path, so the answer errs only toward not ignored.
func GitIgnores(dir, path string) (ignored, known bool) {
	git, err := exec.LookPath("git")
	if err != nil {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "-C", dir, "check-ignore", "-q", "--", path)
	cmd.SysProcAttr = asInvokingUser()
	err = cmd.Run()
	if err == nil {
		return true, true
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

func Hash(data []byte) string {
	hasher := sha256.New()
	hasher.Write(data)
	return hex.EncodeToString(hasher.Sum(nil))
}

// cdIntoOneThatExists is what a user does about a working directory that is
// gone.
const cdIntoOneThatExists = "cd into a directory that exists and run keploy again"

// GetLastDirectory is the name of the directory keploy runs in. It fails once
// that directory is gone, on every platform, in the same words.
func GetLastDirectory() (string, error) {
	return lastDirectory(os.Getwd)
}

func lastDirectory(getwd func() (string, error)) (string, error) {
	dir, err := workingDirectory(getwd)
	if err != nil {
		return "", err
	}
	return filepath.Base(dir), nil
}

// workingDirectory is getwd's answer, once confirmWorkingDirectory says it
// still leads to the directory the process is in.
func workingDirectory(getwd func() (string, error)) (string, error) {
	dir, err := getwd()
	if err == nil {
		err = confirmWorkingDirectory(dir)
	}
	// Linux's Getwd fails here, and on macOS the stat of its answer does.
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("the working directory is gone: %w; %s", err, cdIntoOneThatExists)
	}
	if err != nil {
		return "", err
	}
	return dir, nil
}

// confirmWorkingDirectory fails unless dir, os.Getwd's answer, still leads to
// the directory the process is in. Getwd does not say so itself: on Linux it
// fails once the directory is removed, but macOS's getcwd goes on answering the
// path the directory last had, which by then leads nowhere or to a newer
// directory of that name. A path this user may not walk, as under `sudo -u`
// below a private directory, says nothing of the kind, and is let through.
func confirmWorkingDirectory(dir string) error {
	here, err := os.Stat(".")
	if err == nil {
		var there os.FileInfo
		there, err = os.Stat(dir)
		if err == nil && !os.SameFile(here, there) {
			return fmt.Errorf("the working directory is gone: %s is another directory now; %s", dir, cdIntoOneThatExists)
		}
	}
	if errors.Is(err, os.ErrPermission) {
		return nil
	}
	return err
}

func IsFileEmpty(filePath string) (bool, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return false, err
	}
	return fileInfo.Size() == 0, nil
}

func IsXMLResponse(resp *models.HTTPResp) bool {
	if resp == nil || resp.Header == nil {
		return false
	}

	contentType, exists := resp.Header["Content-Type"]
	if !exists || contentType == "" {
		return false
	}
	return strings.Contains(contentType, "application/xml") || strings.Contains(contentType, "text/xml")
}

// TrimSpaces removes unwanted spaces around unescaped ',' and '='
func TrimSpaces(input string) string {
	var output strings.Builder
	var lastWasEscape bool  // tracks if the previous rune was a backslash
	var skippingSpaces bool // set when we just wrote a separator

	for _, ch := range input {
		// after writing a separator, drop any spaces
		if skippingSpaces {
			if ch == ' ' {
				continue
			}
			skippingSpaces = false
		}

		// handle escape character
		if ch == '\\' && !lastWasEscape {
			lastWasEscape = true
			output.WriteRune(ch)
			continue
		}

		// if this is an unescaped separator, trim before & skip after
		if (ch == ',' || ch == '=') && !lastWasEscape {
			// remove trailing spaces before the separator
			trimmed := strings.TrimRight(output.String(), " ")
			output.Reset()
			output.WriteString(trimmed)

			// write the separator itself
			output.WriteRune(ch)

			// skip any spaces that follow
			skippingSpaces = true
			lastWasEscape = false
			continue
		}

		// normal character (or escaped separator)
		output.WriteRune(ch)
		lastWasEscape = false
	}

	return output.String()
}

func ParseMetadata(metadataStr string) (map[string]interface{}, error) {
	if metadataStr == "" {
		return nil, nil
	}
	m := make(map[string]interface{})
	if err := strvals.ParseInto(metadataStr, m); err != nil {
		return nil, fmt.Errorf("cannot parse metadata: %w", err)
	}
	return m, nil
}

// // XMLToMap converts an XML string into a map[string]interface{}
// func XMLToMap(data string) (map[string]any, error) {
// 	mv, err := mxj.NewMapXml([]byte(data))
// 	if err != nil {
// 		return nil, err
// 	}
// 	return mv, nil
// }

// // MapToXML converts a map[string]interface{} into an XML string
// func MapToXML(data map[string]any) (string, error) {
// 	mv := mxj.Map(data)
// 	xmlBytes, err := mv.Xml()
// 	if err != nil {
// 		return "", err
// 	}
// 	return string(xmlBytes), nil
// }

func NetworkToHostShort(net uint16) uint16 {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, net)
	return binary.LittleEndian.Uint16(b)
}

func ParseGRPCPath(p string) (serviceFull, method string, err error) {
	// Trim whitespace and validate input
	p = strings.TrimSpace(p)
	if p == "" {
		return "", "", fmt.Errorf("gRPC path cannot be empty")
	}

	// Store original path for error messages
	originalPath := p
	p = strings.TrimPrefix(p, "/")

	// Split path into components
	parts := strings.Split(p, "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid gRPC path %q: expected format '/package.Service/Method', got %d path segments", originalPath, len(parts))
	}

	serviceFull = strings.TrimSpace(parts[0])
	method = strings.TrimSpace(parts[1])

	// Validate service name is not empty
	if serviceFull == "" {
		return "", "", fmt.Errorf("invalid gRPC path %q: service name cannot be empty", originalPath)
	}

	// Validate method name is not empty
	if method == "" {
		return "", "", fmt.Errorf("invalid gRPC path %q: method name cannot be empty", originalPath)
	}

	// Validate service name format (should contain at least package.Service)
	if !strings.Contains(serviceFull, ".") {
		return "", "", fmt.Errorf("invalid service name %q in path %q: expected format 'package.Service'", serviceFull, originalPath)
	}

	// Validate service name doesn't start or end with dots
	if strings.HasPrefix(serviceFull, ".") || strings.HasSuffix(serviceFull, ".") {
		return "", "", fmt.Errorf("invalid service name %q in path %q: cannot start or end with '.'", serviceFull, originalPath)
	}

	// Validate service name doesn't contain consecutive dots
	if strings.Contains(serviceFull, "..") {
		return "", "", fmt.Errorf("invalid service name %q in path %q: cannot contain consecutive dots", serviceFull, originalPath)
	}

	// Validate method name format (basic identifier validation)
	if !isValidGRPCIdentifier(method) {
		return "", "", fmt.Errorf("invalid method name %q in path %q: must be a valid identifier", method, originalPath)
	}

	// Validate service parts are valid identifiers
	serviceParts := strings.Split(serviceFull, ".")
	for i, part := range serviceParts {
		if !isValidGRPCIdentifier(part) {
			return "", "", fmt.Errorf("invalid service name component %q at position %d in path %q: must be a valid identifier", part, i, originalPath)
		}
	}

	return serviceFull, method, nil
}

// isValidGRPCIdentifier validates if a string is a valid gRPC identifier
// gRPC identifiers must start with a letter and contain only letters, digits, and underscores
func isValidGRPCIdentifier(name string) bool {
	if len(name) == 0 {
		return false
	}

	// First character must be a letter or underscore
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_') {
		return false
	}

	// Remaining characters must be letters, digits, or underscores
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}

	return true
}

// GetFullCommandUsed is the command line keploy was run with, as a test
// report records it (cmdUsed): os.Args, its secrets redacted by RedactArgv,
// the redactor of every line that names a command whole (the command given
// with -c is read as the command line it is, so its password, token or
// logon is <redacted> and its hosts and paths are kept), and an argument
// that holds a space or a quote Go-quoted. Reports are committed, kept as CI
// artifacts and uploaded: a secret RedactArgv reads in the command reaches
// none.
func GetFullCommandUsed() string {
	args := RedactArgv(os.Args)

	// Build the command string with proper quoting for arguments containing spaces
	var parts []string
	for _, arg := range args {
		if strings.Contains(arg, " ") || strings.Contains(arg, "\"") {
			// Quote the argument if it contains spaces or quotes
			parts = append(parts, fmt.Sprintf("%q", arg))
		} else {
			parts = append(parts, arg)
		}
	}

	cmdStr := strings.Join(parts, " ")

	return cmdStr
}
