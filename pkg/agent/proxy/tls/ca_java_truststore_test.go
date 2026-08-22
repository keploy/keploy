package tls

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestSetupJavaTrustStoreEnv_SetsMergedStore verifies the client-side Java setup:
// it points JAVA_TOOL_OPTIONS at a real, readable JKS that carries BOTH the
// keploy root and the system roots, and it never clobbers an existing value.
func TestSetupJavaTrustStoreEnv_SetsMergedStore(t *testing.T) {
	t.Setenv(EnvJavaToolOptions, "-Xmx256m") // a pre-existing value must survive
	if err := setupJavaTrustStoreEnv(zap.NewNop()); err != nil {
		t.Fatalf("setupJavaTrustStoreEnv: %v", err)
	}
	got := os.Getenv(EnvJavaToolOptions)
	if !strings.HasPrefix(got, "-Xmx256m ") {
		t.Fatalf("existing JAVA_TOOL_OPTIONS was not preserved: %q", got)
	}
	if !strings.Contains(got, "-Djavax.net.ssl.trustStore=") || !strings.Contains(got, "trustStorePassword=changeit") {
		t.Fatalf("JAVA_TOOL_OPTIONS missing truststore flags: %q", got)
	}
	// Extract the store path and confirm it is a valid keystore with the keploy
	// root and at least one system root, using keytool if present.
	var jks string
	for _, f := range strings.Fields(got) {
		if strings.HasPrefix(f, "-Djavax.net.ssl.trustStore=") {
			jks = strings.TrimPrefix(f, "-Djavax.net.ssl.trustStore=")
		}
	}
	t.Cleanup(func() { _ = os.Remove(jks) })
	if _, err := os.Stat(jks); err != nil {
		t.Fatalf("truststore not created: %v", err)
	}
	if _, err := exec.LookPath("keytool"); err != nil {
		t.Skip("keytool not available to inspect the store")
	}
	out, err := exec.Command("keytool", "-list", "-keystore", jks, "-storepass", "changeit").CombinedOutput()
	if err != nil {
		t.Fatalf("keytool could not read the generated store: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "keploy-root") {
		t.Fatalf("store is missing the keploy-root entry:\n%s", out)
	}
	if !strings.Contains(string(out), "system-") {
		t.Fatalf("store is missing the system roots (Java would reject real endpoints):\n%s", out)
	}
}

// TestJavaTrustsServerViaTrustStore is the end-to-end proof that a real JVM,
// pointed at a generateTrustStore JKS through JAVA_TOOL_OPTIONS, trusts a server
// whose CA is in that store — and rejects it without the store. This is exactly
// the mechanism setupJavaTrustStoreEnv uses, with a test cert standing in for
// the keploy CA.
func TestJavaTrustsServerViaTrustStore(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not available")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not available")
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	// Write the server's self-signed cert as PEM and build a JKS from it.
	certPEM := filepath.Join(dir, "srv.pem")
	pemBytes := pemEncodeCert(srv.Certificate().Raw)
	if err := os.WriteFile(certPEM, pemBytes, 0644); err != nil {
		t.Fatal(err)
	}
	jks := filepath.Join(dir, "trust.jks")
	if err := generateTrustStore(certPEM, jks); err != nil {
		t.Fatalf("generateTrustStore: %v", err)
	}

	// A tiny Java HTTPS client that prints OK on success, FAIL otherwise.
	src := filepath.Join(dir, "Client.java")
	_ = os.WriteFile(src, []byte(`import java.net.*; import javax.net.ssl.*;
public class Client {
  public static void main(String[] a) {
    try {
      HttpsURLConnection c = (HttpsURLConnection) new URL(System.getenv("URL")).openConnection();
      c.setConnectTimeout(5000); c.setReadTimeout(5000);
      int code = c.getResponseCode();
      System.out.println("OK " + code);
    } catch (Exception e) { System.out.println("FAIL " + e.getClass().getSimpleName()); }
  }
}
`), 0644)
	if out, err := exec.Command(javac, "-d", dir, src).CombinedOutput(); err != nil {
		t.Fatalf("javac: %v\n%s", err, out)
	}

	run := func(withStore bool) string {
		cmd := exec.Command(java, "-cp", dir, "Client")
		env := append(os.Environ(), "URL="+srv.URL)
		if withStore {
			env = append(env, "JAVA_TOOL_OPTIONS=-Djavax.net.ssl.trustStore="+jks+" -Djavax.net.ssl.trustStorePassword=changeit")
		} else {
			env = append(env, "JAVA_TOOL_OPTIONS=") // default cacerts
		}
		cmd.Env = env
		out, _ := cmd.CombinedOutput()
		return string(out)
	}

	if withStore := run(true); !strings.Contains(withStore, "OK ") {
		t.Fatalf("Java did not trust the server via the merged truststore: %q", withStore)
	}
	if without := run(false); !strings.Contains(without, "FAIL ") {
		t.Fatalf("control: Java should reject the self-signed server without the store, got: %q", without)
	}
}

func pemEncodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestGenerateTrustStore_SkipsNonCertSequence guards the H1 fix: a well-formed
// ASN.1 SEQUENCE that is NOT an X.509 certificate must be dropped, not stored —
// otherwise Java's all-or-nothing keystore load would throw and break ALL of the
// app's TLS. The real (parseable) cert alongside it must survive.
func TestGenerateTrustStore_SkipsNonCertSequence(t *testing.T) {
	dir := t.TempDir()
	// A valid cert (keploy CA) + a bogus but structurally-valid SEQUENCE wrapped
	// in a CERTIFICATE PEM block.
	bogus := pemEncodeCert([]byte{0x30, 0x03, 0x02, 0x01, 0x05}) // SEQUENCE{ INTEGER 5 }
	bundle := append(append([]byte{}, caCrt...), bogus...)
	bp := filepath.Join(dir, "b.pem")
	if err := os.WriteFile(bp, bundle, 0644); err != nil {
		t.Fatal(err)
	}
	jks := filepath.Join(dir, "t.jks")
	if err := generateTrustStore(bp, jks); err != nil {
		t.Fatalf("generateTrustStore: %v", err)
	}
	// If keytool is present, the store must LOAD (proving the bogus entry was
	// dropped, not stored) and contain keploy-root.
	if _, err := exec.LookPath("keytool"); err != nil {
		t.Skip("keytool not available to load the store")
	}
	out, err := exec.Command("keytool", "-list", "-keystore", jks, "-storepass", "changeit").CombinedOutput()
	if err != nil {
		t.Fatalf("store did not load (bogus entry was not dropped): %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "keploy-root") {
		t.Fatalf("real cert missing from store:\n%s", out)
	}
}
