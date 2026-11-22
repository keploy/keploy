package models

// InstallCommand is the command keploy prints to install Keploy from
// keploy.io: the upgrade banner and the refusal for a Podman app name it.
//
// It runs the installer in the calling shell, so the PATH the installer sets
// stays set there, and it writes nothing in the directory it runs in. The
// form it replaces,
// `curl --silent -O -L https://keploy.io/install.sh && source install.sh`,
// first saved the installer as ./install.sh, which overwrote a repository's
// own install.sh whenever it was run from the repo root.
//
// It hands the script over through /dev/stdin and a here-string rather than
// `source <(curl ...)`: bash 3.2, macOS's /bin/bash, sources a process
// substitution as an empty file, returns 0 and installs nothing. When the
// download fails, the echo hands over a script that says so and returns 1,
// so the failure shows at the install line rather than at a later
// "keploy: command not found".
const InstallCommand = `source /dev/stdin <<<"$(curl -fsSL https://keploy.io/install.sh || echo 'echo Keploy installer download failed >&2; return 1')"`
