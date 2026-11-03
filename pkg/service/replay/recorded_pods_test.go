package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The recorded pod's identity reaches the agent from the set's config
// metadata, and a set without one hands the agent nothing to alias.
func TestRecordedPodsOfReadsTheSetMetadata(t *testing.T) {
	conf := &models.TestSet{Metadata: map[string]interface{}{
		"name":                         "test-set-7d9f-api-55f5fd9659-fzsfr",
		models.PodIdentityMetaName:     "api-55f5fd9659-fzsfr",
		models.PodIdentityMetaUID:      "0b1c4d6e-1",
		models.PodIdentityMetaIP:       "10.244.1.17",
		models.PodIdentityMetaHostIP:   "192.168.1.9",
		models.PodIdentityMetaNode:     "worker-1",
		models.PodIdentityMetaHostname: "api-55f5fd9659-fzsfr",
	}}
	got := recordedPodsOf(conf)
	if len(got) != 1 || got[0].Name != "api-55f5fd9659-fzsfr" || got[0].IP != "10.244.1.17" || got[0].HostIP != "192.168.1.9" {
		t.Fatalf("recordedPodsOf = %+v", got)
	}
	for _, conf := range []*models.TestSet{nil, {}, {Metadata: map[string]interface{}{"name": "test-set-0"}}} {
		if got := recordedPodsOf(conf); got != nil {
			t.Fatalf("a set without a recorded pod gave %+v", got)
		}
	}
}
