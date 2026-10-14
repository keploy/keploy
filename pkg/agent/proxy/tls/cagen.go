package tls

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// caCNPrefix is the human-readable prefix of every generated MITM CA's
// Common Name. The run id is appended so a cert viewer shows which run
// minted a leaf, and so the native install filename and JDK alias can be
// derived from it. The prefix is also what distinguishes a keploy-generated
// CA from the retired static "My Custom CA" in logs and cert listings.
const caCNPrefix = "Keploy MITM CA "

// caState holds the parsed MITM signing CA for the lifetime of one run.
//
// It replaces the committed, //go:embed-ed static CA (asset/ca.crt +
// asset/ca.key) whose private key was a public shared secret: anyone with the
// repo could mint leaves every keploy install trusted. The signing key now
// lives here, in the agent's memory. Only the per-pod (Kubernetes) and
// per-user (Windows) storage backends persist it to disk, and never into a
// location the application container can read (see castore.go).
type caState struct {
	certPEM []byte            // PEM of the CA certificate (public, safe to hand to the client)
	keyPEM  []byte            // PEM of the CA private key (secret; persisted only by fileStore)
	der     []byte            // DER of the CA certificate
	cert    *x509.Certificate // parsed CA certificate
	signer  crypto.Signer     // CA private key, passed to CertForClient
	fp      [32]byte          // sha256(der), used to label the keploy-root JKS alias
	runID   string            // short random id; part of the CN, install filename and JDK alias
}

// activeCA is the CA the agent signs MITM leaves with for this run. It is set
// once by SetupCAForApp — after the storage backend has loaded or generated the
// CA — and read on every handshake by HandleTLSConnection → CertForClient.
//
// A nil pointer means SetupCAForApp has not run in this process. A few callers
// reach HandleTLSConnection without ever calling SetupCAForApp (the proxy relay
// path, and the integrations e2e TLS harness), so getActiveCA lazily generates
// an ephemeral in-memory CA on first use rather than nil-dereferencing. That
// ephemeral CA is still per-run and keyless-on-disk, so it carries no security
// regression — it only means those paths get a self-consistent CA with no
// system-store install.
var (
	activeCA   atomic.Pointer[caState]
	activeCAMu sync.Mutex
)

// setActiveCA publishes ca as the CA for this run. SetupCAForApp calls it under
// the same mutex getActiveCA uses for its lazy path, so a late first handshake
// can never race in and overwrite the backend-selected CA with an ephemeral one.
func setActiveCA(ca *caState) {
	activeCAMu.Lock()
	activeCA.Store(ca)
	activeCAMu.Unlock()
}

// getActiveCA returns the CA for this run, generating an ephemeral one the first
// time it is needed if SetupCAForApp never ran. It returns nil only when
// generation itself fails, in which case the caller must fail the handshake
// rather than fall back to a shared key (there is none to fall back to).
func getActiveCA(logger *zap.Logger) *caState {
	if ca := activeCA.Load(); ca != nil {
		return ca
	}
	activeCAMu.Lock()
	defer activeCAMu.Unlock()
	if ca := activeCA.Load(); ca != nil {
		return ca
	}
	ca, err := generateRunCA()
	if err != nil {
		utils.LogError(logger, err, "failed to generate an ephemeral MITM CA; TLS interception will fail for this connection")
		return nil
	}
	logger.Debug("generated an ephemeral MITM CA (SetupCAForApp did not run in this process)",
		zap.String("run_id", ca.runID))
	activeCA.Store(ca)
	return ca
}

// ActiveCACertPEM returns the PEM of this run's CA public certificate, or nil if
// no CA has been established yet. It is the public half only — never the signing
// key — and is what the native client fetches over the control-plane API to
// point the app's trust env vars at.
func ActiveCACertPEM() []byte {
	if ca := activeCA.Load(); ca != nil {
		return ca.certPEM
	}
	return nil
}

// activeCADER returns the DER of the CA certificate for this run, or nil if no
// CA has been established yet. generateTrustStore uses it to give the keploy
// root the stable "keploy-root" alias; a nil result just means every entry
// falls back to the generic "system-<sha>" alias, which is degraded but correct.
func activeCADER() []byte {
	if ca := activeCA.Load(); ca != nil {
		return ca.der
	}
	return nil
}

// generateRunCA mints a fresh, self-signed MITM CA for one run.
//
// ECDSA P-256 matches the leaf key type cfssl mints (CertForClient) and the
// retired static CA, so nothing downstream has to learn a new algorithm. The
// validity window is deliberately wide — NotBefore 20 years in the past,
// NotAfter 10 years ahead — because enterprise time-freezing replays past
// recordings against a frozen clock and signs leaves with
// NotBefore = recordTime − 1 year (CertForClient). The CA must be valid at that
// frozen instant, and 20 years covers every realistic recording age while a
// fresh key per run means the long window is not a security liability.
func generateRunCA() (*caState, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA private key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA serial number: %w", err)
	}

	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, fmt.Errorf("failed to generate CA run id: %w", err)
	}
	runID := hex.EncodeToString(idBytes)

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCNPrefix + runID},
		NotBefore:             now.AddDate(-20, 0, 0),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	return caStateFromDERAndKey(der, key, runID)
}

// caStateFromDERAndKey assembles a caState from freshly-created or loaded CA
// material. runID, when empty, is derived from the certificate's CN so a CA
// reloaded from disk (fileStore) keeps the same install filename and JDK alias
// it used on first generation.
func caStateFromDERAndKey(der []byte, key crypto.Signer, runID string) (*caState, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}
	if runID == "" {
		runID = runIDFromCN(cert.Subject.CommonName)
	}
	ecKey := toECDSA(key)
	if ecKey == nil {
		return nil, fmt.Errorf("CA private key is not ECDSA; this build only mints ECDSA CAs")
	}
	keyDER, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CA private key: %w", err)
	}
	return &caState{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		der:     der,
		cert:    cert,
		signer:  key,
		fp:      sha256.Sum256(der),
		runID:   runID,
	}, nil
}

// caStateFromPEM reconstructs a caState from a persisted cert+key pair (the
// fileStore load path). It accepts only the EC key form this package writes.
func caStateFromPEM(certPEM, keyPEM []byte) (*caState, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("persisted CA certificate is not a CERTIFICATE PEM block")
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("persisted CA key is not a PEM block")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse persisted CA key: %w", err)
	}
	return caStateFromDERAndKey(certBlock.Bytes, key, "")
}

// runIDFromCN extracts the run id suffix from a "Keploy MITM CA <id>" CN. A CN
// that does not carry the prefix (a hand-made CA, say) yields an empty id, which
// only degrades the alias/filename naming, not correctness.
func runIDFromCN(cn string) string {
	if strings.HasPrefix(cn, caCNPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(cn, caCNPrefix))
	}
	return ""
}

// toECDSA narrows a crypto.Signer this package is known to hold (it only ever
// stores ECDSA keys) to *ecdsa.PrivateKey for marshalling. A non-ECDSA signer
// would be a programmer error introduced by a future backend.
func toECDSA(key crypto.Signer) *ecdsa.PrivateKey {
	if ec, ok := key.(*ecdsa.PrivateKey); ok {
		return ec
	}
	return nil
}
