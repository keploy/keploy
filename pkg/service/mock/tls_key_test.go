package mock

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/kubeconfig"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// keyInstrumentation is emptyInstrumentation that keeps the options the
// recording asked the agent to capture with, and whether it set the agent up.
type keyInstrumentation struct {
	emptyInstrumentation
	opts  *models.OutgoingOptions
	setUp bool
}

func (k *keyInstrumentation) Setup(ctx context.Context, cmd string, o models.SetupOptions) error {
	k.setUp = true
	return k.emptyInstrumentation.Setup(ctx, cmd, o)
}

func (k *keyInstrumentation) GetOutgoing(ctx context.Context, o models.OutgoingOptions) (<-chan *models.Mock, error) {
	k.opts = &o
	return k.emptyInstrumentation.GetOutgoing(ctx, o)
}

// A client that authenticates with a certificate (mutual TLS) sends it to
// keploy's proxy, which presents it to the server with the key
// record.tlsPrivateKeyPath names. keploy mock record hands the agent that
// key, as keploy record does: without it the agent could not dial the server
// for such a client, and the call was lost.
func TestMockRecordHandsTheAgentTheTLSPrivateKey(t *testing.T) {
	noKubeconfig(t)
	const key = "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIA==\n-----END EC PRIVATE KEY-----\n"
	path := filepath.Join(t.TempDir(), "client.key")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	inst := &keyInstrumentation{emptyInstrumentation: emptyInstrumentation{out: make(chan *models.Mock)}}
	cfg := &config.Config{}
	cfg.Mock.Name = "kube"
	cfg.Record.TLSPrivateKeyPath = path

	if err := New(zap.NewNop(), inst, newStatefulMockDB(), nil, FileStore{}, nil, cfg).Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if inst.opts == nil {
		t.Fatal("the recording never asked the agent to capture")
	}
	if inst.opts.TLSPrivateKey != key {
		t.Fatalf("the agent was handed TLSPrivateKey %q, want record.tlsPrivateKeyPath's file %q", inst.opts.TLSPrivateKey, key)
	}
}

// noKubeconfig keeps the machine's kubeconfig out of a test.
func noKubeconfig(t *testing.T) {
	t.Helper()
	t.Setenv(kubeconfig.EnvVar, filepath.Join(t.TempDir(), "no-kubeconfig"))
}

// kubectl and helm send their kubeconfig user's client certificate: keploy
// mock record hands the agent that user's key after record.tlsPrivateKeyPath's,
// read from the kubeconfig the test command reads.
func TestMockRecordHandsTheAgentTheKubeconfigUsersKey(t *testing.T) {
	const key = "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIA==\n-----END EC PRIVATE KEY-----\n"
	const kube = "-----BEGIN RSA PRIVATE KEY-----\nMIIB\n-----END RSA PRIVATE KEY-----\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "client.key"), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(cfgPath, []byte("users:\n- name: kind-kp\n  user:\n    client-key-data: "+base64.StdEncoding.EncodeToString([]byte(kube))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(kubeconfig.EnvVar, cfgPath)
	inst := &keyInstrumentation{emptyInstrumentation: emptyInstrumentation{out: make(chan *models.Mock)}}
	cfg := &config.Config{}
	cfg.Mock.Name = "kube"
	cfg.Record.TLSPrivateKeyPath = filepath.Join(dir, "client.key")

	if err := New(zap.NewNop(), inst, newStatefulMockDB(), nil, FileStore{}, nil, cfg).Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if inst.opts == nil || inst.opts.TLSPrivateKey != key+kube {
		t.Fatalf("the agent was handed %+v, want record.tlsPrivateKeyPath's key, then the kubeconfig user's", inst.opts)
	}
}

// With no record.tlsPrivateKeyPath the agent is handed no key, as before.
func TestMockRecordHandsNoKeyWhenNoneIsSet(t *testing.T) {
	noKubeconfig(t)
	inst := &keyInstrumentation{emptyInstrumentation: emptyInstrumentation{out: make(chan *models.Mock)}}
	cfg := &config.Config{}
	cfg.Mock.Name = "kube"

	if err := New(zap.NewNop(), inst, newStatefulMockDB(), nil, FileStore{}, nil, cfg).Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if inst.opts == nil || inst.opts.TLSPrivateKey != "" {
		t.Fatalf("the agent was handed %+v, want a capture with no key", inst.opts)
	}
}

// A record.tlsPrivateKeyPath keploy cannot read is refused before the agent
// is set up or the test command runs, naming the setting and the path: a
// recording without the key would lose every call of a client that
// authenticates with a certificate.
func TestMockRecordRefusesAnUnreadableTLSPrivateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.key")
	inst := &keyInstrumentation{emptyInstrumentation: emptyInstrumentation{out: make(chan *models.Mock)}}
	cfg := &config.Config{}
	cfg.Mock.Name = "kube"
	cfg.Record.TLSPrivateKeyPath = path

	err := New(zap.NewNop(), inst, newStatefulMockDB(), nil, FileStore{}, nil, cfg).Record(context.Background())
	if err == nil {
		t.Fatal("Record went on with a record.tlsPrivateKeyPath it cannot read")
	}
	if msg := err.Error(); !strings.Contains(msg, "record.tlsPrivateKeyPath") || !strings.Contains(msg, path) {
		t.Fatalf("the refusal %q does not name record.tlsPrivateKeyPath and %s", msg, path)
	}
	if inst.setUp || inst.opts != nil {
		t.Fatalf("the agent was set up (%v) or asked to capture (%v) before the refusal", inst.setUp, inst.opts != nil)
	}
}
