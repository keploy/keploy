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
)

// ComposeScope is what docker compose resolves its files in, beyond the files
// themselves: the compose command's own -p/--project-name (or the
// COMPOSE_PROJECT_NAME it assigns), --project-directory and --env-file values.
// The zero value is a command that sets none of them, which is the usual case:
// compose then takes the project directory from the first file, reads that
// directory's .env, and names the project from the environment, the files'
// name: or the directory.
type ComposeScope struct {
	ProjectName string
	ProjectDir  string
	EnvFiles    []string
}

// composeIdentity is what the compose files make of each service's container
// name, resolved as docker compose resolves them: the project's name, and each
// service's container_name after interpolation and the merge of every file
// ("" when the service sets none, or sets it to a variable that is empty).
type composeIdentity struct {
	project        string
	containerNames map[string]string
}

// loadComposeIdentity resolves paths, in order, as `docker compose -f p1 -f p2
// ...` resolves them in scope, with compose's own library: ${VAR}, ${VAR:-d},
// ${VAR-d}, ${VAR:?e} and $$ against keploy's environment (it wins, as the
// shell's does for compose), then the env files (scope's, else
// COMPOSE_ENV_FILES, else the project directory's .env), and each later file
// merged over the earlier ones, extends and include followed.
//
// It reads what names a container and nothing more. A file that does not load
// (a required variable unset, a malformed template, an env file that cannot be
// read) is an error, as it is to compose: keploy does not fall back to
// matching the text as written, which is the guess that missed every
// container_name set from a variable.
func loadComposeIdentity(ctx context.Context, paths []string, scope ComposeScope) (*composeIdentity, error) {
	for _, p := range paths {
		if p == "-" {
			return nil, errors.New("keploy cannot read a compose file from its standard input (-f -); name the file instead")
		}
	}
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
		abs, err := filepath.Abs(composepaths.ExpandUser(f))
		if err != nil {
			return nil, fmt.Errorf("resolve compose env file %q: %w", f, err)
		}
		files = append(files, abs)
	}
	o, err := cli.NewProjectOptions(paths,
		cli.WithWorkingDirectory(scope.ProjectDir),
		func(o *cli.ProjectOptions) error { o.Environment = env; return nil },
		cli.WithEnvFiles(files...),
		cli.WithDotEnv,
		cli.WithName(scope.ProjectName),
		// Every profile: a service's container is named the same whether or
		// not its profile is active, and an inactive one is still a service
		// keploy was asked for by name.
		cli.WithProfiles([]string{"*"}),
		// Nothing below changes a container's name, and each can fail a file
		// compose itself runs: a service's env_file or label_file missing on
		// this machine, a newer schema than this library's, a dependency on a
		// service another file brings.
		cli.WithResolvedPaths(false),
		cli.WithNormalization(false),
		cli.WithConsistency(false),
		cli.WithoutEnvironmentResolution,
		cli.WithoutLabelsResolution,
		cli.WithLoadOptions(func(lo *loader.Options) {
			lo.SkipValidation = true
			// compose warns of an unset variable itself when it runs the
			// files; saying it here as well would say it twice. The options
			// are applied once before the loader sets interpolation up, to
			// read the files, with no Interpolate yet.
			if lo.Interpolate != nil {
				lo.Interpolate.Substitute = func(s string, m template.Mapping) (string, error) {
					return template.SubstituteWithOptions(s, m, template.WithoutLogging)
				}
			}
		}),
	)
	if err != nil {
		return nil, err
	}
	project, err := o.LoadProject(ctx)
	if err != nil {
		return nil, err
	}
	ident := &composeIdentity{project: project.Name, containerNames: map[string]string{}}
	for name, svc := range project.AllServices() {
		ident.containerNames[name] = svc.ContainerName
	}
	return ident, nil
}

// serviceOf is the service whose container compose names name: the one whose
// container_name resolves to it or, for a service with none, whose
// <project>-<service>-<number> it is. Failing that, the service whose key is
// name, as keploy has always taken one. A name two services' containers
// share, or that names no service, is an error saying what each container is
// named, so the name keploy was given can be put right.
func (ci *composeIdentity) serviceOf(name string) (string, error) {
	services := make([]string, 0, len(ci.containerNames))
	for svc := range ci.containerNames {
		services = append(services, svc)
	}
	sort.Strings(services)
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

// namesContainer reports whether compose names service svc's container name.
func (ci *composeIdentity) namesContainer(svc, name string) bool {
	if cn := ci.containerNames[svc]; cn != "" {
		return cn == name
	}
	n, ok := strings.CutPrefix(name, ci.project+composeContainerSeparator+svc+composeContainerSeparator)
	if !ok {
		return false
	}
	// compose numbers a service's containers from 1, written as %d.
	i, err := strconv.Atoi(n)
	return err == nil && i >= 1 && strconv.Itoa(i) == n
}

// containerOf is how compose names service svc's container, for an error.
func (ci *composeIdentity) containerOf(svc string) string {
	if cn := ci.containerNames[svc]; cn != "" {
		return strconv.Quote(cn)
	}
	return strconv.Quote(ci.project+composeContainerSeparator+svc+composeContainerSeparator+"1") + " (and on, one per replica)"
}
