package agent

import "path/filepath"

// keyFolder is the folder a claim's key names: the key is the test process's
// folder relative to the project (models.InstanceReq.Key, "." for the
// project's own). The project is the folder keploy.yml is read from
// (config.ConfigPath, which keploy hands its native agent as --config-path,
// made absolute as the CLI read its config; the agent's working folder, the
// CLI's, when it names none); keploy's proven commands run only from it, and
// name it to the test command as KEPLOY_MOCK_PROJECT, which keploytest joins
// its key to. The project is resolved, as keploytest resolves the folder it
// claims with, and the key joined as it is written: a key that is a link
// keeps its own name, as the record names a linked folder in the repository
// (setName), and stays the folder the test's ID names. A claim that sends no
// folder, as an integration written by hand does since keploy's scope
// protocol v1 asks only for the key (models.ScopeUpgradeRemedy), is then
// kept as keploytest's is: each test begun under it carries the folder on its
// window (models.ScopeWindow.Dir), which a record writes as the test's folder
// (models.MappedTestCase.Dir) relative to the repository's top, which cloud
// set the test is in. A claim with no folder used to leave every one of its
// tests with none, so keploy mock record refused to commit the recording of
// every framework but those of keploy's integrations. key is the claim's
// validated key: clean, relative and inside the project.
func (a *Agent) keyFolder(key string) string {
	project := "."
	if a.config != nil && a.config.ConfigPath != "" {
		project = a.config.ConfigPath
	}
	if abs, err := filepath.Abs(project); err == nil {
		project = abs
	}
	if r, err := filepath.EvalSymlinks(project); err == nil {
		project = r
	}
	return filepath.Join(project, filepath.FromSlash(key))
}
