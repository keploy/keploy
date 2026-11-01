package docker

import (
	"strings"
	"testing"
)

// TestAppGateParksAppUntilReleaseFile pins the #4894 shape: the app waits on
// keploy-app-gate completing (CLI event), not on the agent becoming
// service_healthy (wall-clock).
func TestAppGateParksAppUntilReleaseFile(t *testing.T) {
	out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    command: ["echo", "booted"]
`)

	svcs, _ := out["services"].(map[string]interface{})
	gate, _ := svcs[KeployAppGateServiceName].(map[string]interface{})
	if gate == nil {
		t.Fatalf("%s service missing from generated compose", KeployAppGateServiceName)
	}
	if gate["image"] != keployAppGateImage {
		t.Errorf("gate image = %v, want %s", gate["image"], keployAppGateImage)
	}
	cmd, _ := gate["command"].([]interface{})
	joined := ""
	for _, c := range cmd {
		joined += c.(string) + " "
	}
	if !strings.Contains(joined, AgentReleaseFile) {
		t.Errorf("gate command %v does not wait on %s", cmd, AgentReleaseFile)
	}
	if !strings.Contains(joined, "rm -f "+AgentReleaseFile) {
		t.Errorf("gate command %v must clear a stale release file before waiting", cmd)
	}
	vols, _ := gate["volumes"].([]interface{})
	ro := false
	for _, v := range vols {
		s, _ := v.(string)
		if strings.HasSuffix(s, ":ro") && strings.Contains(s, KeployTLSVolumeName) {
			ro = true
		}
	}
	if ro {
		t.Errorf("gate TLS volume must be RW so it can clear a stale %s; got %v", AgentReleaseFile, vols)
	}

	deps, ok := svcField(t, out, "app", "depends_on").(map[string]interface{})
	if !ok {
		t.Fatalf("app depends_on missing or wrong type")
	}
	agentDep, _ := deps["keploy-agent"].(map[string]interface{})
	if agentDep["condition"] != "service_started" {
		t.Errorf("keploy-agent condition = %v, want service_started (not service_healthy)", agentDep["condition"])
	}
	gateDep, _ := deps[KeployAppGateServiceName].(map[string]interface{})
	if gateDep["condition"] != "service_completed_successfully" {
		t.Errorf("%s condition = %v, want service_completed_successfully",
			KeployAppGateServiceName, gateDep["condition"])
	}
}

// TestAppGateRewritesPriorHealthyDependency upgrades a user's leftover
// service_healthy gate on keploy-agent to service_started and adds the app-gate.
func TestAppGateRewritesPriorHealthyDependency(t *testing.T) {
	out := runModifyForAgent(t, `services:
  app:
    image: alpine:3.21
    depends_on:
      keploy-agent:
        condition: service_healthy
      db:
        condition: service_started
  db:
    image: alpine:3.21
`)
	deps, ok := svcField(t, out, "app", "depends_on").(map[string]interface{})
	if !ok {
		t.Fatalf("app depends_on missing")
	}
	agentDep, _ := deps["keploy-agent"].(map[string]interface{})
	if agentDep["condition"] != "service_started" {
		t.Errorf("keploy-agent condition stayed %v; want service_started", agentDep["condition"])
	}
	if deps[KeployAppGateServiceName] == nil {
		t.Errorf("missing %s dependency: %v", KeployAppGateServiceName, deps)
	}
	if deps["db"] == nil {
		t.Errorf("user db dependency dropped: %v", deps)
	}
}
