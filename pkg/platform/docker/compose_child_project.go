package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// composeChildProject is the slice of a compose document that
// ComposeChildOutput needs, and nothing more: the project's name, and each
// service's container_name.
//
// It replaces the compose-go loader this file used to call
// (loadComposeProject, in the in-process compose library). That library drove
// `compose up` ITSELF; reading two fields out of a document is all that is
// wanted here, and keploy WROTE this document -- composeContent is its own
// generated stack -- so there is no third-party syntax to interpret and no
// reason to carry a loader for it.
//
// What the loader did that this does not: resolve interpolation, extends,
// profiles and env files. None of it changes the two fields read here. A
// ${VAR} inside container_name would differ, and compose itself names such a
// container by the INTERPOLATED value -- so that container simply does not
// match, serviceOf answers "", and the line is kept against the project with
// no service attributed. That is the same answer the loader gave for any
// container it could not place.
type composeChildProject struct {
	name string
	// containerNames maps a service to its explicit container_name. A service
	// that sets none is present with "" -- it has to be, because the
	// <service><sep><number> fallback below only applies to those.
	containerNames map[string]string
}

// composeProjectNameEnv is the variable compose reads a project name from
// when neither the command nor the caller names one.
const composeProjectNameEnv = "COMPOSE_PROJECT_NAME"

// composeEnvFilesEnv is the variable compose reads extra env files from. Not
// read here -- see composeChildProject on why interpolation does not change
// what this file takes out of the document -- but the name travels with the
// one above, and the output tests drive both.
const composeEnvFilesEnv = "COMPOSE_ENV_FILES"

// composeContainerSeparator is what compose puts between a service and its
// replica number when the document sets no container_name (api.Separator).
const composeContainerSeparator = "-"

// loadComposeChildProject reads content as the project name and the services'
// container names.
//
// The name follows compose's own precedence: the name the caller resolved from
// the command, else COMPOSE_PROJECT_NAME, else the working directory's base.
func loadComposeChildProject(content []byte, projectName, absWorkingDir string, envFiles []string) (*composeChildProject, error) {
	var doc struct {
		// The document may name the project itself. It sits ABOVE the
		// directory and BELOW the environment in compose's precedence, which
		// is why it is read here rather than left to the caller.
		Name     string `yaml:"name"`
		Services map[string]struct {
			ContainerName string `yaml:"container_name"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("read the compose document keploy generated: %w", err)
	}

	p := &composeChildProject{
		name:           composeChildProjectName(projectName, doc.Name, absWorkingDir, envFiles),
		containerNames: make(map[string]string, len(doc.Services)),
	}
	for svc, def := range doc.Services {
		p.containerNames[svc] = strings.TrimSpace(def.ContainerName)
	}
	return p, nil
}

// composeChildProjectName follows compose's precedence, highest first: the
// name the caller resolved from the command, COMPOSE_PROJECT_NAME, the name
// the document gives itself, and last the working directory's base.
//
// Only the last is normalised. compose takes the first three as written --
// they were chosen -- and derives the last, so that is the one it has to make
// a legal project name out of.
func composeChildProjectName(projectName, documentName, absWorkingDir string, envFiles []string) string {
	if name := strings.TrimSpace(projectName); name != "" {
		return name
	}
	if name := strings.TrimSpace(os.Getenv(composeProjectNameEnv)); name != "" {
		return name
	}
	// THE ENV FILES COUNT, and dropping them was this reader's first mistake:
	// `docker compose --env-file x.env` with COMPOSE_PROJECT_NAME in that file
	// names the project from it, so the tails would have been recorded under a
	// project nothing reads back.
	if name := strings.TrimSpace(composeEnvFileValue(composeProjectNameEnv, absWorkingDir, envFiles)); name != "" {
		return name
	}
	if name := strings.TrimSpace(documentName); name != "" {
		return name
	}
	return normalizeComposeProjectNameForChild(filepath.Base(absWorkingDir))
}

// composeEnvFileValue reads key out of the env files compose reads, in
// compose's own order: --env-file when given, else COMPOSE_ENV_FILES, else the
// project directory's .env. A later file wins, as a later --env-file does.
//
// The PROCESS environment is not consulted here -- the caller checked it
// first, because a variable set in the shell beats one in a file.
func composeEnvFileValue(key, absWorkingDir string, envFiles []string) string {
	files := envFiles
	if len(files) == 0 {
		files = strings.FieldsFunc(os.Getenv(composeEnvFilesEnv), func(c rune) bool { return c == ',' })
	}
	if len(files) == 0 {
		if os.Getenv("COMPOSE_DISABLE_ENV_FILE") != "" {
			return ""
		}
		files = []string{filepath.Join(absWorkingDir, ".env")}
	}
	value := ""
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if abs, err := filepath.Abs(f); err == nil {
			f = abs
		}
		raw, err := os.ReadFile(f) //nolint:gosec // a path the user named on the compose command
		if err != nil {
			continue // compose ignores a .env that is not there; an unreadable --env-file fails its own run
		}
		if v, ok := envFileValue(string(raw), key); ok {
			value = v
		}
	}
	return value
}

// envFileValue finds key in one env file's contents. A later assignment wins,
// as it does for the shell `.env` format: blank lines and # comments are
// skipped, `export ` is allowed, and a value may be quoted.
func envFileValue(contents, key string) (string, bool) {
	value, found := "", false
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		value, found = v, true
	}
	return value, found
}

// normalizeComposeProjectNameForChild is compose's NormalizeProjectName: lower
// case, and only [a-z0-9_-], with leading separators dropped.
func normalizeComposeProjectNameForChild(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "_-")
}

// serviceOf answers which service compose printed `name` for, in the same two
// passes the compose-go version used.
//
// The order is load-bearing: an explicit container_name is checked FIRST
// across every service, because a document may both name one service's
// container and leave another's to the <service><sep><number> default, and the
// default could otherwise claim a name that belongs to the explicit one.
func (p *composeChildProject) serviceOf(name string) string {
	if p == nil {
		return ""
	}
	for svc, container := range p.containerNames {
		if container != "" && container == name {
			return svc
		}
	}
	for svc, container := range p.containerNames {
		if container != "" {
			continue
		}
		number, ok := strings.CutPrefix(name, svc+composeContainerSeparator)
		if ok && number != "" && strings.Trim(number, "0123456789") == "" {
			return svc
		}
	}
	return ""
}

// composeWorkingDir is the absolute working directory compose resolves
// relative paths against: the caller's, else the process's.
func composeWorkingDir(workingDir string) (string, error) {
	workingDir = strings.TrimSpace(workingDir)
	if workingDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve compose working directory: %w", err)
		}
		workingDir = wd
	}
	abs, err := filepath.Abs(workingDir)
	if err != nil {
		return "", fmt.Errorf("resolve compose working directory: %w", err)
	}
	return abs, nil
}
