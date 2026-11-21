package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	composepaths "github.com/compose-spec/compose-go/v2/paths"
	"github.com/compose-spec/compose-go/v2/template"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	yamlv4 "go.yaml.in/yaml/v4"
)

// ComposeScope is what docker compose resolves its files in, beyond the files
// themselves: the compose command's own -p/--project-name (or the
// COMPOSE_PROJECT_NAME it assigns), --project-directory, --env-file and
// --compatibility values, and whether keploy's in-process compose library
// runs the files in the CLI's place. The zero value is a docker compose
// command that sets none of them, which is the usual case: compose then takes
// the project directory from the first file (the current directory for a
// document on its standard input), reads that directory's .env, and names the
// project from the environment, the files' name: or the directory.
type ComposeScope struct {
	ProjectName string
	ProjectDir  string
	EnvFiles    []string
	// Compatibility is the command's --compatibility: compose then names a
	// service's containers <project>_<service>_<number>, as it does when its
	// environment (keploy's, else the env files') sets COMPOSE_COMPATIBILITY
	// true.
	Compatibility bool
	// InProcess is set when the compose library keploy runs in-process
	// (RegisterComposeStack) runs the document, not the docker compose CLI.
	// Compatibility mode is the CLI's: the library names every container
	// with compose's own separator, whatever the command or the environment
	// say.
	InProcess bool
}

// composeCompatibilityEnv is the variable that runs the docker compose CLI in
// compatibility mode, read from the environment the files resolve against.
const composeCompatibilityEnv = "COMPOSE_COMPATIBILITY"

// composeCompatibilitySeparator is what compose puts between a container
// name's parts in compatibility mode, in composeContainerSeparator's place.
const composeCompatibilitySeparator = "_"

// composeIdentity is what the compose files make of each service's container
// name, resolved as docker compose resolves them: the project's name, the
// separator compose joins a default container name with, and each service's
// container_name after interpolation and the merge of every file ("" when the
// service sets none, or sets it to a variable that is empty).
type composeIdentity struct {
	project        string
	separator      string
	containerNames map[string]string
}

// loadComposeIdentity resolves paths, in order, as `docker compose -f p1 -f p2
// ...` resolves them in scope (resolveComposeIdentity).
func loadComposeIdentity(ctx context.Context, paths []string, scope ComposeScope) (*composeIdentity, error) {
	for _, p := range paths {
		if p == "-" {
			return nil, errors.New("keploy cannot read a compose file from its standard input (-f -); name the file instead")
		}
	}
	return resolveComposeIdentity(ctx, paths, nil, scope)
}

// loadComposeContentIdentity resolves content as `docker compose -f -` resolves
// the document on its standard input in scope (resolveComposeIdentity).
func loadComposeContentIdentity(ctx context.Context, content []byte, scope ComposeScope) (*composeIdentity, error) {
	if len(content) == 0 {
		return nil, errors.New("the compose document is empty")
	}
	return resolveComposeIdentity(ctx, []string{"-"}, content, scope)
}

// resolveComposeIdentity resolves the files at paths, or content as the one
// document `-f -` reads when content is set, as docker compose resolves them
// in scope, with compose's own library: ${VAR}, ${VAR:-d}, ${VAR-d}, ${VAR:?e}
// and $$ against keploy's environment (it wins, as the shell's does for
// compose), then the env files (scope's, else COMPOSE_ENV_FILES, else the
// project directory's .env), and each later file merged over the earlier
// ones, extends and include followed.
//
// It reads what names a container and nothing more. A document that does not
// load (a required variable unset, a malformed template, an env file that
// cannot be read) is an error, as it is to compose: keploy does not fall back
// to matching the text as written, which is the guess that missed every
// container_name set from a variable.
func resolveComposeIdentity(ctx context.Context, paths []string, content []byte, scope ComposeScope) (*composeIdentity, error) {
	env := composetypes.Mapping{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	envFiles := scope.EnvFiles
	if len(envFiles) == 0 {
		envFiles = strings.FieldsFunc(os.Getenv(composeEnvFilesEnv), func(c rune) bool { return c == ',' })
	}
	files := make([]string, 0, len(envFiles))
	for _, f := range envFiles {
		// compose takes an env file from the current directory, with a
		// leading ~ expanded as the shell expands it.
		files = append(files, composepaths.ExpandUser(f))
	}
	// Nothing below changes a container's name, and each can fail a file
	// compose itself runs: a dependency on a service another file brings, a
	// service's env_file or label_file missing on this machine, a newer
	// schema than this library's. Normalization stays on: compose lowercases
	// the project's name: as it names containers.
	loadOptions := []func(*loader.Options){func(lo *loader.Options) {
		lo.SkipConsistencyCheck = true
		lo.SkipResolveEnvironment = true
		lo.SkipResolveLabels = true
		lo.SkipValidation = true
		// compose warns of an unset variable itself when it runs the
		// files; saying it here as well would say it twice. The options
		// are applied once before the loader sets interpolation up, to read
		// the files, with no Interpolate yet.
		if lo.Interpolate != nil {
			lo.Interpolate.Substitute = func(s string, m template.Mapping) (string, error) {
				return template.SubstituteWithOptions(s, m, template.WithoutLogging)
			}
		}
	}}
	o, err := cli.NewProjectOptions(paths,
		cli.WithWorkingDirectory(scope.ProjectDir),
		func(o *cli.ProjectOptions) error { o.Environment = env; return nil },
		cli.WithEnvFiles(files...),
		cli.WithDotEnv,
		cli.WithName(scope.ProjectName),
		cli.WithLoadOptions(loadOptions...),
	)
	if err != nil {
		return nil, err
	}
	project, err := loadSchemaUnchecked(func(extra ...func(*loader.Options)) (*composetypes.Project, error) {
		if content == nil {
			if err := cli.WithLoadOptions(extra...)(o); err != nil {
				return nil, err
			}
			return o.LoadProject(ctx)
		}
		return loadComposeContent(ctx, o, content, append(loadOptions[:len(loadOptions):len(loadOptions)], extra...))
	})
	if err != nil {
		return nil, err
	}
	// The loader takes an empty name when validation is skipped (a name:
	// whose variable is unset); compose refuses the project then.
	if project.Name == "" {
		return nil, errors.New("project name can't be empty. Use `--project-name` to set a valid name")
	}
	ident := &composeIdentity{project: project.Name, separator: composeContainerSeparator, containerNames: map[string]string{}}
	if !scope.InProcess && (scope.Compatibility || composeStringToBool(o.Environment[composeCompatibilityEnv])) {
		ident.separator = composeCompatibilitySeparator
	}
	for name, svc := range project.AllServices() {
		ident.containerNames[name] = svc.ContainerName
	}
	return ident, nil
}

// loadSchemaUnchecked is load(), which loads with compose's schema unchecked
// (resolveComposeIdentity's SkipValidation). compose-go's normalizer asserts the
// types the schema guarantees, so a document the schema refuses (a service's
// `volumes:` or `networks:` left empty, a service with no body) panics there,
// where compose, which checks the schema first, says why. Such a document is
// loaded once more with the schema checked, and that load's error is the one
// compose gives.
func loadSchemaUnchecked(load func(extra ...func(*loader.Options)) (*composetypes.Project, error)) (*composetypes.Project, error) {
	project, failure, err := func() (p *composetypes.Project, failure any, err error) {
		defer func() { failure = recover() }()
		p, err = load()
		return p, nil, err
	}()
	if failure == nil {
		return project, err
	}
	if _, err := load(func(lo *loader.Options) { lo.SkipValidation = false }); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("compose's loader failed on the document: %v", failure)
}

// loadComposeContent is o.LoadProject for content as the one document `-f -`
// reads, which LoadProject would read from keploy's own standard input: the
// same steps (ProjectOptions.prepare) on content, so the document is named and
// placed as compose names and places it. The project directory is
// --project-directory, else the current directory. The name is -p, else
// COMPOSE_PROJECT_NAME in the environment the document resolves against,
// else the document's name: (interpolated and normalized by the loader), else
// the directory's base, normalized.
func loadComposeContent(ctx context.Context, o *cli.ProjectOptions, content []byte, loadOptions []func(*loader.Options)) (*composetypes.Project, error) {
	workingDir, err := o.GetWorkingDir()
	if err != nil {
		return nil, err
	}
	named := false
	if o.Name == "" {
		var n struct {
			Name string `yaml:"name,omitempty"`
		}
		if err := yamlv4.Unmarshal(content, &n); err != nil {
			return nil, err
		}
		named = n.Name != ""
	}
	options := append(loadOptions[:len(loadOptions):len(loadOptions)], func(lo *loader.Options) {
		switch {
		case o.Name != "":
			lo.SetProjectName(o.Name, true)
		case o.Environment[composeProjectNameEnv] != "":
			lo.SetProjectName(o.Environment[composeProjectNameEnv], true)
		case !named:
			lo.SetProjectName(loader.NormalizeProjectName(filepath.Base(workingDir)), false)
		}
	})
	return loader.LoadWithContext(ctx, composetypes.ConfigDetails{
		ConfigFiles: []composetypes.ConfigFile{{Filename: "-", Content: content}},
		WorkingDir:  workingDir,
		Environment: o.Environment,
	}, options...)
}

// composeStringToBool is how the docker compose CLI reads a yes-or-no variable
// (its utils.StringToBool): "y", or what strconv.ParseBool takes as true,
// either in any case and with spaces around it; anything else is no.
func composeStringToBool(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "y" {
		return true
	}
	b, _ := strconv.ParseBool(s)
	return b
}

// serviceOf is the service whose container compose names name: the one whose
// container_name resolves to it or, for a service with none, whose
// <project>-<service>-<number> it is. Failing that, the service whose key is
// name, as keploy has always taken one. A name two services' containers
// share, or that names no service, is an error saying what each container is
// named, so the name keploy was given can be put right.
func (ci *composeIdentity) serviceOf(name string) (string, error) {
	services := ci.services()
	var named []string
	for _, svc := range services {
		if ci.namesContainer(svc, name) {
			named = append(named, svc)
		}
	}
	if len(named) > 1 {
		return "", fmt.Errorf("container %q is the container of more than one compose service (%s), so keploy cannot tell which is the application's", name, strings.Join(named, ", "))
	}
	if len(named) == 1 {
		return named[0], nil
	}
	if _, ok := ci.containerNames[name]; ok {
		return name, nil
	}
	described := make([]string, 0, len(services))
	for _, svc := range services {
		described = append(described, fmt.Sprintf("%s: %s", svc, ci.containerOf(svc)))
	}
	return "", fmt.Errorf("container %q not found in any of the provided docker-compose files: no service's container is named so and no service is keyed so; resolved as docker compose resolves them, the containers are (%s)", name, strings.Join(described, "; "))
}

// services is the project's service keys, sorted.
func (ci *composeIdentity) services() []string {
	services := make([]string, 0, len(ci.containerNames))
	for svc := range ci.containerNames {
		services = append(services, svc)
	}
	sort.Strings(services)
	return services
}

// namesContainer reports whether compose names service svc's container name.
func (ci *composeIdentity) namesContainer(svc, name string) bool {
	if cn := ci.containerNames[svc]; cn != "" {
		return cn == name
	}
	n, ok := strings.CutPrefix(name, ci.project+ci.separator)
	return ok && ci.isReplica(svc, n)
}

// isReplica reports whether name is <svc><separator><number>: how compose
// names a replica of service svc, which sets no container_name, in the
// project, and so how its output prints that replica's lines.
func (ci *composeIdentity) isReplica(svc, name string) bool {
	n, ok := strings.CutPrefix(name, svc+ci.separator)
	if !ok {
		return false
	}
	// compose numbers a service's containers from 1, written as %d.
	i, err := strconv.Atoi(n)
	return err == nil && i >= 1 && strconv.Itoa(i) == n
}

// serviceOfOutput is the service whose container compose prints its lines as
// name in its attached output: the one whose container_name is name, else the
// one name is <service><separator><number> of (compose's container name
// without the project's prefix), else "". Every container_name is looked at
// first, so a replica's default name cannot claim a container another service
// names.
func (ci *composeIdentity) serviceOfOutput(name string) string {
	if ci == nil {
		return ""
	}
	services := ci.services()
	for _, svc := range services {
		if cn := ci.containerNames[svc]; cn != "" && cn == name {
			return svc
		}
	}
	for _, svc := range services {
		if ci.containerNames[svc] == "" && ci.isReplica(svc, name) {
			return svc
		}
	}
	return ""
}

// containerOf is how compose names service svc's container, for an error.
func (ci *composeIdentity) containerOf(svc string) string {
	if cn := ci.containerNames[svc]; cn != "" {
		return strconv.Quote(cn)
	}
	return strconv.Quote(ci.project+ci.separator+svc+ci.separator+"1") + " (and on, one per replica)"
}
