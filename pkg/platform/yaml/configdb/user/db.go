// Package user provides functionality for working with keploy user configs like installation id.
package user

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/denisbrodbeck/machineid"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/telemetry"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
	yamlLib "gopkg.in/yaml.v3"
)

type KeployConfig struct {
	UpdatePrompt string `yaml:"updatePrompt" json:"updatePrompt"`
}

type Db struct {
	logger *zap.Logger
	cfg    *config.Config
}

func HomeDir() string {
	configFolder := "/.keploy"
	if runtime.GOOS == "windows" {
		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		return home + configFolder
	}
	return os.Getenv("HOME") + configFolder
}

func (db *Db) ReadKeployConfig() (*KeployConfig, error) {
	path := HomeDir() + "/keploy.yaml"
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Decode the yaml file
	var data KeployConfig
	err = yaml.Unmarshal(content, &data)
	if err != nil {
		utils.LogError(db.logger, err, "failed to unmarshal keploy.yaml")
		return nil, err
	}
	return &data, nil
}

func (db *Db) WriteKeployConfig(data *KeployConfig) error {
	// Open the keploy.yaml file
	path := HomeDir() + "/keploy.yaml"
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			db.logger.Error("failed to close file", zap.Error(err))
		}
	}()
	updatedData, err := yamlLib.Marshal(data)
	if err != nil {
		return err
	}
	// Truncate the file before writing to it.
	err = file.Truncate(0)
	if err != nil {
		return err
	}
	_, err = file.Write(updatedData)
	if err != nil {
		return err
	}
	return nil
}

func New(logger *zap.Logger, cfg *config.Config) *Db {
	return &Db{
		logger: logger,
		cfg:    cfg,
	}
}

// machineID reads the OS machine id (/etc/machine-id, IOPlatformUUID,
// MachineGuid). Package variables below are injectable for deterministic tests.
var (
	machineID          = machineid.ID
	hostnameFn         = os.Hostname
	readFileFn         = os.ReadFile
	randReadFn         = rand.Read
	osLinkFn           = os.Link
	commandLine        = func() []string { return os.Args }
	preRemoveCorruptFn = func() {}

	persistMu         sync.Mutex
	processFallbackMu sync.Mutex
	processFallbackID string
)

const (
	// installationAppID keys the installation id to keploy (installationID).
	installationAppID = "keploy"
	// installationIDFileName is the plain-text persisted installation id file
	// under ~/.keploy/. Deliberately distinct from installation-id.yaml.
	installationIDFileName = "installation-id"
	// ciFallbackDomainKey domain-separates deterministic CI repository hashes.
	ciFallbackDomainKey = "keploy-ci-installation-id-v1"
	// fallbackDomainKey domain-separates deterministic container/host fallback hashes.
	fallbackDomainKey = "keploy-fallback-installation-id-v1"
	// maxPersistedIDBytes caps reads from ~/.keploy/installation-id so a huge
	// or corrupt file is never loaded into memory.
	maxPersistedIDBytes = 256
	// minPersistedIDLen and maxPersistedIDLen bound valid persisted/env/flag IDs.
	minPersistedIDLen = 16
	maxPersistedIDLen = 128

	procBootIDPath    = "/proc/sys/kernel/random/boot_id"
	procCgroupPath    = "/proc/self/cgroup"
	procMountInfoPath = "/proc/self/mountinfo"
)

func (db *Db) debugf(msg string, fields ...zap.Field) {
	if db != nil && db.logger != nil {
		db.logger.Debug(msg, fields...)
	}
}

func (db *Db) validEnvInstallationID(envName string) (string, bool) {
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return "", false
	}
	if !isValidPersistedID(raw) {
		db.debugf("ignoring invalid installation id from environment", zap.String("env", envName))
		return "", false
	}
	return raw, true
}

// agentInstallationIDArg reads telemetry.AgentInstallationIDFlag from args
// when invoking `keploy agent` (pre-cobra, mirroring utils/log.clientLogFileArg).
func agentInstallationIDArg(args []string) (string, bool) {
	if len(args) < 2 || args[1] != "agent" {
		return "", false
	}
	flag := "--" + telemetry.AgentInstallationIDFlag
	val := ""
	for i := 2; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			val = v
		} else if a == flag && i+1 < len(args) {
			val = args[i+1]
			i++
		}
	}
	val = strings.TrimSpace(val)
	return val, val != ""
}

// GetInstallationID returns a stable, non-empty, shell-safe installation id
// for this machine, container, or CI repository. Resolution order:
//  1. Explicit override (validated):
//     - Native agent's hidden `--installation-id` CLI flag (forwarded by the
//     CLI over sudo before any fallback file can be created).
//     - Explicit KEPLOY_INSTALLATION_ID environment variable anywhere.
//     - INSTALLATION_ID environment variable only inside a container keploy
//     started (KEPLOY_INDOCKER=true), matching #5279 so generic host/CI
//     INSTALLATION_ID variables (e.g. GitHub App installation IDs) are never
//     trusted on the host.
//  2. Keyed hash (HMAC-SHA256) of the OS machine id when KEPLOY_INDOCKER != "true"
//     (ignoring empty or systemd "uninitialized" placeholders).
//  3. Pre-existing valid persisted id in ~/.keploy/installation-id (regular file).
//  4. Deterministic HMAC-SHA256 of provider-gated, uniquely-scoped CI
//     repository/pipeline identifiers (only when that provider's CI marker is
//     present, so developer Codespaces keep distinct per-user IDs).
//  5. Newly generated random 256-bit hex id persisted atomically (0600, fsynced,
//     with sudo ownership hand-back under sudo) in ~/.keploy/installation-id.
//  6. Deterministic HMAC-SHA256 of stable container/host signals when ~/.keploy
//     cannot be persisted (read-only FS, missing $HOME, or unsafe file type).
//  7. Last-resort process-cached random (or SHA-256) id when no distinguishing
//     signals exist, so GetInstallationID never returns an empty string.
//
// Every returned ID consists solely of ASCII [A-Za-z0-9_-] characters safe for
// shell aliases, cmd.exe, and Docker Compose YAML.
func (db *Db) GetInstallationID(_ context.Context) (string, error) {
	// 1. Explicit override (native agent flag, namespaced env, or in-Docker env).
	if raw, ok := agentInstallationIDArg(commandLine()); ok {
		if isValidPersistedID(raw) {
			return raw, nil
		}
		db.debugf("ignoring invalid installation id from agent flag")
	}
	if id, ok := db.validEnvInstallationID("KEPLOY_INSTALLATION_ID"); ok {
		return id, nil
	}
	inDocker := os.Getenv("KEPLOY_INDOCKER") == "true"
	if inDocker {
		if id, ok := db.validEnvInstallationID("INSTALLATION_ID"); ok {
			return id, nil
		}
	}

	// 2. OS machine id outside Docker containers.
	if !inDocker {
		raw, err := machineID()
		if err != nil {
			db.debugf("failed to get machine id", zap.Error(err))
		} else if raw = strings.TrimSpace(raw); raw != "" && !strings.EqualFold(raw, "uninitialized") {
			return installationID(raw), nil
		}
	}

	// 3. Reuse an existing persisted installation id if one is already on disk.
	home, dir, hasHome := installationIDDir()
	if hasHome {
		if existing, _, _, err := readPersistedInstallationID(filepath.Join(dir, installationIDFileName)); err == nil && existing != "" {
			return existing, nil
		}
	}

	// 4. Deterministic per-repository CI id before minting a random file so
	//    ephemeral CI runners (which have a fresh writable $HOME every job)
	//    report one stable installation id per repository instead of one per job.
	if id, ok := deriveDeterministicCIID(); ok {
		return id, nil
	}

	// 5. Persisted installation id under ~/.keploy/installation-id.
	if hasHome {
		id, err := loadOrCreatePersistedInstallationID(home, dir)
		if err == nil && id != "" {
			return id, nil
		}
		if err != nil {
			db.debugf("failed to persist installation id, falling back to deterministic hash", zap.Error(err))
		}
	}

	// 6. Deterministic hash of stable container/host inputs (read-only FS / missing $HOME).
	if id, ok := deriveDeterministicFallbackID(); ok {
		return id, nil
	}

	// 7. Last-resort process-cached id so GetInstallationID never returns "".
	return processCachedFallbackID(), nil
}

// installationID is the id keploy sends for the machine whose OS id is raw:
// the hex HMAC-SHA256 of installationAppID keyed by raw, which is
// machineid.ProtectedID("keploy"). It names the machine alike on every run
// and cannot be turned back into raw; whoever holds a raw id an older keploy
// sent can compute it, so the two can be joined.
func installationID(raw string) string {
	mac := hmac.New(sha256.New, []byte(raw))
	mac.Write([]byte(installationAppID))
	return hex.EncodeToString(mac.Sum(nil))
}

// installationIDDir returns the user's home directory and ~/.keploy directory
// when configured, or ("", "", false) when $HOME / Windows profile env vars
// are unset (avoiding writes to "/.keploy" at the filesystem root or relative
// drive paths like "C:.keploy" when HOMEDRIVE is set without HOMEPATH).
func installationIDDir() (home, dir string, ok bool) {
	home = resolveHome(runtime.GOOS, os.Getenv)
	if home == "" {
		return "", "", false
	}
	return home, filepath.Join(home, ".keploy"), true
}

func resolveHome(goos string, getenv func(string) string) string {
	if goos == "windows" {
		drive := strings.TrimSpace(getenv("HOMEDRIVE"))
		path := strings.TrimSpace(getenv("HOMEPATH"))
		if drive != "" && path != "" {
			return drive + path
		}
		return strings.TrimSpace(getenv("USERPROFILE"))
	}
	return strings.TrimSpace(getenv("HOME"))
}

func loadOrCreatePersistedInstallationID(home, dir string) (string, error) {
	persistMu.Lock()
	defer persistMu.Unlock()

	path := filepath.Join(dir, installationIDFileName)
	existing, missing, corruptInfo, err := readPersistedInstallationID(path)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}

	uid, gid, needChown, err := sudoTargetOwner(home)
	if err != nil {
		return "", err
	}

	candidate, err := randomHexID()
	if err != nil {
		return "", err
	}

	_, dirStatErr := os.Lstat(dir)
	dirMissing := errors.Is(dirStatErr, os.ErrNotExist)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if dirMissing && needChown {
		if err := chownPathFn(dir, uid, gid); err != nil {
			_ = os.Remove(dir)
			return "", err
		}
	}

	tmp, err := os.CreateTemp(dir, ".installation-id-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if needChown {
		if err := chownFileFn(tmp, uid, gid); err != nil {
			_ = tmp.Close()
			return "", err
		}
	}
	if _, err := tmp.WriteString(candidate + "\n"); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	if !missing {
		// Corrupt regular file: verify the same inode is still at path before
		// removing it, so if another process replaced the corrupt file while
		// we were writing/fsyncing tmp, we return that winner instead of
		// deleting and clobbering it.
		preRemoveCorruptFn()
		curInfo, statErr := os.Lstat(path)
		if statErr == nil && !os.SameFile(corruptInfo, curInfo) {
			if winner, _, _, readErr := readPersistedInstallationID(path); readErr == nil && winner != "" {
				return winner, nil
			}
			return "", fmt.Errorf("installation id file changed during corrupt recovery")
		}
		if statErr == nil {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
		}
	}

	// Use hard-link creation first so two concurrent processes racing on first
	// boot (or corrupt-file recovery) do not overwrite each other's ID.
	linkErr := osLinkFn(tmpPath, path)
	if linkErr == nil {
		syncDir(dir)
		return candidate, nil
	}
	if errors.Is(linkErr, os.ErrExist) {
		if winner, _, _, readErr := readPersistedInstallationID(path); readErr == nil && winner != "" {
			return winner, nil
		}
		return "", linkErr
	}

	// Fall back to atomic rename only when the filesystem rejects hard links.
	if err := os.Rename(tmpPath, path); err != nil {
		return "", err
	}
	cleanup = false
	syncDir(dir)
	return candidate, nil
}

func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// readPersistedInstallationID reads and validates path. It rejects non-regular
// files (symlinks, FIFOs, directories) via os.Lstat before opening so startup
// can never follow a symlink or block on a FIFO while holding persistMu.
// It returns:
//   - (id, false, info, nil) when path holds a valid persisted id
//   - ("", true, nil, nil) when path does not exist
//   - ("", false, info, nil) when path is a regular file that is empty or corrupt
//   - ("", false, nil, err) on I/O errors or non-regular file types
func readPersistedInstallationID(path string) (id string, missing bool, info os.FileInfo, err error) {
	info, err = os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", true, nil, nil
		}
		return "", false, nil, err
	}
	if !info.Mode().IsRegular() {
		return "", false, nil, fmt.Errorf("installation id path %s is not a regular file (mode %s)", path, info.Mode().Type())
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", true, nil, nil
		}
		return "", false, nil, err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxPersistedIDBytes+1))
	if err != nil {
		return "", false, nil, err
	}
	if len(data) > maxPersistedIDBytes {
		return "", false, info, nil
	}
	trimmed := strings.TrimSpace(string(data))
	if !isValidPersistedID(trimmed) {
		return "", false, info, nil
	}
	return trimmed, false, info, nil
}

func isValidPersistedID(id string) bool {
	if len(id) < minPersistedIDLen || len(id) > maxPersistedIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		isLower := c >= 'a' && c <= 'z'
		isUpper := c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isUpper && !isDigit && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func randomHexID() (string, error) {
	var b [32]byte
	if _, err := randReadFn(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type idSignal struct {
	key string
	val string
}

func hmacSignals(domainKey string, signals []idSignal) string {
	mac := hmac.New(sha256.New, []byte(domainKey))
	for _, s := range signals {
		_, _ = fmt.Fprintf(mac, "%s=%d:%s\n", s.key, len(s.val), s.val)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// normalizeServerScope normalizes an optional server or organization URL/host
// scope (such as GITHUB_SERVER_URL, CI_SERVER_URL, JENKINS_URL, or
// SYSTEM_COLLECTIONURI) for deterministic hashing, stripping trailing slashes
// and any embedded credentials.
func normalizeServerScope(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		u.User = nil
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		u.Path = strings.TrimRight(u.Path, "/")
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	}
	return strings.TrimRight(strings.ToLower(s), "/")
}

// parseCodeBuildIdentity extracts (region, account, project) from
// CODEBUILD_BUILD_ID ("<project>:<uuid>") and CODEBUILD_BUILD_ARN
// ("arn:<partition>:codebuild:<region>:<account>:build/<project>:<uuid>").
func parseCodeBuildIdentity(buildID, buildARN string) (region, account, project string, ok bool) {
	project, _, hasColon := strings.Cut(strings.TrimSpace(buildID), ":")
	project = strings.TrimSpace(project)
	if !hasColon || project == "" {
		return "", "", "", false
	}
	parts := strings.Split(strings.TrimSpace(buildARN), ":")
	if len(parts) < 6 || parts[0] != "arn" || parts[2] != "codebuild" {
		return "", "", "", false
	}
	region = strings.TrimSpace(parts[3])
	account = strings.TrimSpace(parts[4])
	if region == "" || account == "" {
		return "", "", "", false
	}
	return region, account, project, true
}

func envTrim(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// jenkinsPipelineName strips the trailing branch/PR path segment from a
// Jenkins multibranch or organization-folder JOB_NAME (e.g.
// "acme/payments/PR-123" or "acme/payments/feature%2Ffoo" -> "acme/payments")
// when BRANCH_NAME is set AND matches that trailing segment (raw or
// URL-decoded), so every branch and PR of the same repository shares one stable
// ID while parameterized freestyle/pipeline jobs that define their own
// BRANCH_NAME keep their full JOB_NAME.
func jenkinsPipelineName(jobName, branchName string) string {
	if jobName == "" {
		return ""
	}
	if branchName != "" {
		if i := strings.LastIndex(jobName, "/"); i > 0 {
			tail := jobName[i+1:]
			decoded, err := url.PathUnescape(tail)
			if tail == branchName || (err == nil && decoded == branchName) {
				return jobName[:i]
			}
		}
	}
	return jobName
}

// deriveDeterministicCIID derives a stable installation ID from provider-gated,
// uniquely-scoped CI repository/pipeline identifiers without mixing in
// ephemeral per-job values (hostname, container ID, boot_id, or runner name).
//
// Each provider's variables are read ONLY when that provider's own CI marker
// is set, so non-CI environments such as GitHub Codespaces (which set
// GITHUB_REPOSITORY without GITHUB_ACTIONS) or local emulators (nektos/act
// setting ACT=true without GITEA_ACTIONS/FORGEJO_ACTIONS) never collapse users
// onto a shared repository ID. Provider server/organization scopes are included
// alongside project names so distinct customers never collide on generic names.
func deriveDeterministicCIID() (string, bool) {
	var signals []idSignal
	add := func(key, val string) {
		if val != "" {
			signals = append(signals, idSignal{key: key, val: val})
		}
	}

	switch {
	case envTrim("GITHUB_ACTIONS") != "" && (envTrim("ACT") == "" || envTrim("GITEA_ACTIONS") != "" || envTrim("FORGEJO_ACTIONS") != ""):
		if repo := envTrim("GITHUB_REPOSITORY"); repo != "" {
			add("ci:github:server", normalizeServerScope(os.Getenv("GITHUB_SERVER_URL")))
			add("ci:github:repo", repo)
		}
	case envTrim("GITLAB_CI") != "":
		if proj := envTrim("CI_PROJECT_PATH"); proj != "" {
			add("ci:gitlab:server", normalizeServerScope(os.Getenv("CI_SERVER_URL")))
			add("ci:gitlab:project", proj)
		}
	case envTrim("JENKINS_URL") != "":
		if job := jenkinsPipelineName(envTrim("JOB_NAME"), envTrim("BRANCH_NAME")); job != "" {
			if srv := normalizeServerScope(os.Getenv("JENKINS_URL")); srv != "" {
				add("ci:jenkins:server", srv)
				add("ci:jenkins:job", job)
			}
		}
	case envTrim("CIRCLECI") != "":
		user := envTrim("CIRCLE_PROJECT_USERNAME")
		repo := envTrim("CIRCLE_PROJECT_REPONAME")
		if user != "" && repo != "" {
			add("ci:circleci:user", user)
			add("ci:circleci:repo", repo)
		}
	case envTrim("TRAVIS") != "":
		if slug := envTrim("TRAVIS_REPO_SLUG"); slug != "" {
			add("ci:travis:repo", slug)
		}
	case envTrim("BUILDKITE") != "":
		org := envTrim("BUILDKITE_ORGANIZATION_SLUG")
		pipe := envTrim("BUILDKITE_PIPELINE_SLUG")
		if org != "" && pipe != "" {
			add("ci:buildkite:org", org)
			add("ci:buildkite:pipeline", pipe)
		}
	case envTrim("TF_BUILD") != "":
		repo := envTrim("BUILD_REPOSITORY_NAME")
		coll := normalizeServerScope(os.Getenv("SYSTEM_COLLECTIONURI"))
		projID := envTrim("SYSTEM_TEAMPROJECTID")
		// Azure Repos puts a bare repo name in BUILD_REPOSITORY_NAME; require
		// org/project scope (or an explicit owner/repo slash) so bare names
		// never collide across unrelated Azure organizations.
		if repo != "" && (coll != "" || projID != "" || strings.Contains(repo, "/")) {
			add("ci:azure:collection", coll)
			add("ci:azure:project", projID)
			add("ci:azure:repo", repo)
		}
	case envTrim("BITBUCKET_PIPELINE_UUID") != "" || envTrim("BITBUCKET_BUILD_NUMBER") != "":
		if repo := envTrim("BITBUCKET_REPO_FULL_NAME"); repo != "" {
			add("ci:bitbucket:repo", repo)
		}
	case envTrim("CODEBUILD_BUILD_ID") != "":
		if region, account, project, ok := parseCodeBuildIdentity(os.Getenv("CODEBUILD_BUILD_ID"), os.Getenv("CODEBUILD_BUILD_ARN")); ok {
			add("ci:codebuild:region", region)
			add("ci:codebuild:account", account)
			add("ci:codebuild:project", project)
		}
	// Harness CI sets DRONE=true and DRONE_REPO on every build, so check
	// HARNESS_BUILD_ID before DRONE and require HARNESS_ACCOUNT_ID.
	case envTrim("HARNESS_BUILD_ID") != "":
		acct := envTrim("HARNESS_ACCOUNT_ID")
		repo := envTrim("DRONE_REPO")
		if repo == "" {
			repo = envTrim("CI_REPO")
		}
		if acct != "" && repo != "" {
			add("ci:harness:account", acct)
			add("ci:harness:repo", repo)
		}
	case envTrim("DRONE") != "":
		if repo := envTrim("DRONE_REPO"); repo != "" {
			add("ci:drone:server", normalizeServerScope(os.Getenv("DRONE_SYSTEM_HOST")))
			add("ci:drone:repo", repo)
		}
	case envTrim("WOODPECKER") != "" || strings.EqualFold(envTrim("CI"), "woodpecker"):
		if repo := envTrim("CI_REPO"); repo != "" {
			add("ci:woodpecker:server", normalizeServerScope(os.Getenv("CI_SYSTEM_HOST")))
			add("ci:woodpecker:repo", repo)
		}
	case envTrim("SEMAPHORE") != "":
		if repo := envTrim("SEMAPHORE_GIT_REPO_SLUG"); repo != "" {
			add("ci:semaphore:org", normalizeServerScope(os.Getenv("SEMAPHORE_ORGANIZATION_URL")))
			add("ci:semaphore:repo", repo)
		}
	case envTrim("APPVEYOR") != "":
		if repo := envTrim("APPVEYOR_REPO_NAME"); repo != "" {
			add("ci:appveyor:account", envTrim("APPVEYOR_ACCOUNT_NAME"))
			add("ci:appveyor:repo", repo)
		}
	}

	if len(signals) == 0 {
		return "", false
	}
	return hmacSignals(ciFallbackDomainKey, signals), true
}

func deriveDeterministicFallbackID() (string, bool) {
	var signals []idSignal
	hasDistinguishing := false

	if host, err := hostnameFn(); err == nil {
		if host = strings.TrimSpace(host); host != "" {
			signals = append(signals, idSignal{key: "hostname", val: host})
			if !isGenericHostname(host) {
				hasDistinguishing = true
			}
		}
	}

	if cid := detectContainerID(); cid != "" {
		signals = append(signals, idSignal{key: "container_id", val: cid})
		hasDistinguishing = true
	}

	// Only consult kernel boot_id when neither container ID nor a non-generic
	// hostname is present: boot_id changes on host reboot, so excluding it
	// when stable container/host signals exist keeps restarted containers
	// stable across host reboots while still giving bare-metal "localhost"
	// boxes without machine-id/$HOME a boot-stable ID.
	if !hasDistinguishing { // boot_id fallback gate
		if b, err := readFileFn(procBootIDPath); err == nil {
			if bootID := strings.TrimSpace(string(b)); bootID != "" {
				signals = append(signals, idSignal{key: "boot_id", val: bootID})
				hasDistinguishing = true
			}
		}
	}

	if !hasDistinguishing {
		return "", false
	}

	signals = append(signals,
		idSignal{key: "os", val: runtime.GOOS},
		idSignal{key: "arch", val: runtime.GOARCH},
	)

	return hmacSignals(fallbackDomainKey, signals), true
}

func isGenericHostname(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "localhost.localdomain", "ip6-localhost", "ip6-loopback":
		return true
	default:
		return false
	}
}

func detectContainerID() string {
	if b, err := readFileFn(procCgroupPath); err == nil {
		if cid := extractContainerIDFromCgroup(string(b)); cid != "" {
			return cid
		}
	}
	if b, err := readFileFn(procMountInfoPath); err == nil {
		if cid := extractContainerIDFromMountinfo(string(b)); cid != "" {
			return cid
		}
	}
	return ""
}

func extractContainerIDFromCgroup(content string) string {
	for _, line := range strings.Split(content, "\n") {
		// Format: hierarchy-ID:controller-list:cgroup-path
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if tok := extractHex64Token(parts[2]); tok != "" {
			return tok
		}
	}
	return ""
}

var mountinfoHexPrefixes = []string{
	"/containers/",
	"/sandboxes/",
	"/docker/",
	"/containerd/",
	"/crio/",
	"/libpod/",
}

// isSelfContainerMountPoint limits /proc/self/mountinfo inspection to the
// container's own bind-mounted identity files so a process running on a Docker
// or Kubernetes host never picks up another container's mount entry.
func isSelfContainerMountPoint(mountPoint string) bool {
	switch mountPoint {
	case "/etc/hostname", "/etc/hosts", "/etc/resolv.conf":
		return true
	default:
		return false
	}
}

func extractContainerIDFromMountinfo(content string) string {
	for _, line := range strings.Split(content, "\n") {
		// /proc/self/mountinfo fields: mountID parentID major:minor root mountPoint ...
		fields := strings.Fields(line)
		if len(fields) < 5 || !isSelfContainerMountPoint(fields[4]) {
			continue
		}
		root := fields[3]
		for _, prefix := range mountinfoHexPrefixes {
			idx := strings.Index(root, prefix)
			if idx < 0 {
				continue
			}
			rest := root[idx+len(prefix):]
			seg := nextPathSegment(rest)
			if tok := extractHex64Token(seg); tok != "" {
				return tok
			}
		}
		if idx := strings.Index(root, "/pods/"); idx >= 0 {
			seg := nextPathSegment(root[idx+len("/pods/"):])
			if isValidPersistedID(seg) {
				return seg
			}
		}
	}
	return ""
}

func nextPathSegment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' || s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

func extractHex64Token(s string) string {
	start := -1
	for i := 0; i <= len(s); i++ {
		if i < len(s) && isHexByte(s[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			if i-start == 64 {
				return strings.ToLower(s[start:i])
			}
			start = -1
		}
	}
	return ""
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func processCachedFallbackID() string {
	processFallbackMu.Lock()
	defer processFallbackMu.Unlock()
	if processFallbackID != "" {
		return processFallbackID
	}
	if id, err := randomHexID(); err == nil {
		processFallbackID = id
		return processFallbackID
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s:%d:%d:%d:%s:%s",
		fallbackDomainKey, os.Getpid(), os.Getppid(), time.Now().UnixNano(), runtime.GOOS, runtime.GOARCH)
	processFallbackID = hex.EncodeToString(h.Sum(nil))
	return processFallbackID
}
