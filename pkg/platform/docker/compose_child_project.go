package docker

// composeProjectNameEnv is the variable compose reads a project name from
// when the command names none.
const composeProjectNameEnv = "COMPOSE_PROJECT_NAME"

// composeEnvFilesEnv is the variable compose reads its env files from when
// the command names none.
const composeEnvFilesEnv = "COMPOSE_ENV_FILES"

// composeContainerSeparator is what compose puts between a container name's
// parts, <project>-<service>-<number>, when the service sets no
// container_name (api.Separator).
const composeContainerSeparator = "-"
