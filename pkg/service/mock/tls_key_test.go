package mock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
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

// With no record.tlsPrivateKeyPath the agent is handed no key, as before.
func TestMockRecordHandsNoKeyWhenNoneIsSet(t *testing.T) {
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
