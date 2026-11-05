package tls

import (
	"bytes"
	"encoding/pem"
	"os"

	"github.com/pavlo-v-chernykh/keystore-go/v4"
)

// jdkKeystoreTrusts says whether the JDK keystore at cacertsPath trusts the
// certificate in the PEM file caPath already, under any alias, read in Go:
// no JVM runs. On Debian and Ubuntu the system store's update puts a new
// anchor there itself, for a packaged JDK whose cacerts links to the
// keystore an update hook keeps: ca-certificates-java's hook imports it into
// /etc/ssl/certs/java/cacerts (as debian:<anchor>.pem), and Adoptium's
// rebuilds /etc/ssl/certs/adoptium/cacerts (Temurin's) from the system's
// anchors. Both write JKS. Only a JKS keystore whose integrity checks with
// storePass is read: any other (PKCS12), or one that cannot be read, says
// false, and keploy imports the CA with keytool.
func jdkKeystoreTrusts(cacertsPath, storePass, caPath string) bool {
	pemBytes, err := os.ReadFile(caPath)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	aliases, _ := jdkKeystoreAliasesHolding(cacertsPath, storePass, block.Bytes)
	return len(aliases) > 0
}

// jdkKeystoreAliasesHolding reads, in Go, the aliases under which the JDK
// keystore at cacertsPath trusts the certificate der: no JVM runs. read is
// false when the keystore is not a JKS whose integrity checks with
// storePass, or cannot be read: only keytool can say then.
func jdkKeystoreAliasesHolding(cacertsPath, storePass string, der []byte) (aliases []string, read bool) {
	f, err := os.Open(cacertsPath)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	ks := keystore.New()
	if err := ks.Load(f, []byte(storePass)); err != nil {
		return nil, false
	}
	for _, alias := range ks.Aliases() {
		e, err := ks.GetTrustedCertificateEntry(alias)
		if err == nil && bytes.Equal(e.Certificate.Content, der) {
			aliases = append(aliases, alias)
		}
	}
	return aliases, true
}

// jdkKeystoreAliasCert reads, in Go, the certificate the JDK keystore at
// cacertsPath trusts under alias: no JVM runs. read is false when the
// keystore is not a JKS whose integrity checks with storePass (a JDK 18 or
// later ships its cacerts as PKCS12), or cannot be read: only keytool can
// say then. With read true, der is nil when no trusted certificate is under
// alias.
func jdkKeystoreAliasCert(cacertsPath, storePass, alias string) (der []byte, read bool) {
	f, err := os.Open(cacertsPath)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	ks := keystore.New()
	if err := ks.Load(f, []byte(storePass)); err != nil {
		return nil, false
	}
	e, err := ks.GetTrustedCertificateEntry(alias)
	if err != nil {
		return nil, true
	}
	return e.Certificate.Content, true
}
