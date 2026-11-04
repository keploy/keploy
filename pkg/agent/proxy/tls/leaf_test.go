package tls

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// These rows pin the leaf the cfssl local signer minted under its default
// profile, read field by field off its output before mintLeaf replaced it:
// the same subject, names, usages, constraints, key ids, serial size,
// validity, signature algorithm and errors.

// mintFor runs CertForClient for host as a handshake would, turning a panic
// into a failed row (and an error) so a row can say a mint panicked rather
// than crash the test binary.
func mintFor(t *testing.T, caKey any, caCert *x509.Certificate, host string, backdate time.Time) (cert *tls.Certificate, err error) {
	t.Helper()
	hello, cleanup, herr := helperClientHello(host)
	if herr != nil {
		t.Fatal(herr)
	}
	defer cleanup()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("minting a leaf for %q panicked: %v", host, r)
			cert, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return CertForClient(zap.NewNop(), hello, caKey, caCert, backdate)
}

// leafTestCA makes a CA certificate for key, signed with alg (0: crypto/x509's
// default for the key).
func leafTestCA(t *testing.T, key crypto.Signer, alg x509.SignatureAlgorithm) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Leaf Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		SignatureAlgorithm:    alg,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

type leafExt struct {
	oid      string
	critical bool
}

// checkLeafShape asserts everything about a leaf for host that does not
// depend on how host is named (see TestLeafNamesTheHostByWhatItParsesAs).
func checkLeafShape(t *testing.T, cert *tls.Certificate, host string, ca *x509.Certificate) *x509.Certificate {
	t.Helper()
	if len(cert.Certificate) != 1 {
		t.Fatalf("chain has %d certificates, want the leaf alone", len(cert.Certificate))
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if cert.Leaf == nil || !bytes.Equal(cert.Leaf.Raw, cert.Certificate[0]) {
		t.Errorf("Leaf is not the parsed certificate")
	}
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		t.Fatalf("leaf key is %T, want an ECDSA P-256 key", cert.PrivateKey)
	}
	if !key.PublicKey.Equal(leaf.PublicKey) {
		t.Errorf("the private key does not match the leaf's public key")
	}
	if leaf.Version != 3 {
		t.Errorf("version %d, want 3", leaf.Version)
	}
	wantSubject, err := asn1.Marshal(pkix.Name{CommonName: host}.ToRDNSequence())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leaf.RawSubject, wantSubject) {
		t.Errorf("subject %x, want CN=%q alone (%x)", leaf.RawSubject, host, wantSubject)
	}
	if !bytes.Equal(leaf.RawIssuer, ca.RawSubject) {
		t.Errorf("issuer is not the CA's subject")
	}
	if leaf.KeyUsage != x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment {
		t.Errorf("key usage %v, want digital signature and key encipherment", leaf.KeyUsage)
	}
	if want := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}; !slices.Equal(leaf.ExtKeyUsage, want) || len(leaf.UnknownExtKeyUsage) != 0 {
		t.Errorf("extended key usage %v %v, want %v", leaf.ExtKeyUsage, leaf.UnknownExtKeyUsage, want)
	}
	if !leaf.BasicConstraintsValid || leaf.IsCA || leaf.MaxPathLen != -1 || leaf.MaxPathLenZero {
		t.Errorf("basic constraints valid=%v CA=%v pathlen=%d zero=%v, want CA:FALSE with no path length",
			leaf.BasicConstraintsValid, leaf.IsCA, leaf.MaxPathLen, leaf.MaxPathLenZero)
	}
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(leaf.RawSubjectPublicKeyInfo, &spki); err != nil {
		t.Fatal(err)
	}
	if ski := sha1.Sum(spki.PublicKey.Bytes); !bytes.Equal(leaf.SubjectKeyId, ski[:]) {
		t.Errorf("subject key id %x, want the SHA-1 of the public key's bits %x", leaf.SubjectKeyId, ski)
	}
	if len(ca.SubjectKeyId) == 0 || !bytes.Equal(leaf.AuthorityKeyId, ca.SubjectKeyId) {
		t.Errorf("authority key id %x, want the CA's subject key id %x", leaf.AuthorityKeyId, ca.SubjectKeyId)
	}
	if leaf.SerialNumber.Sign() <= 0 || leaf.SerialNumber.BitLen() > 159 {
		t.Errorf("serial %v (%d bits), want positive and at most 159 bits", leaf.SerialNumber, leaf.SerialNumber.BitLen())
	}
	want := []leafExt{
		{"2.5.29.15", true},  // key usage
		{"2.5.29.37", false}, // extended key usage
		{"2.5.29.19", true},  // basic constraints
		{"2.5.29.14", false}, // subject key id
		{"2.5.29.35", false}, // authority key id
		{"2.5.29.17", host == ""},
	}
	var got []leafExt
	for _, e := range leaf.Extensions {
		got = append(got, leafExt{e.Id.String(), e.Critical})
	}
	if !slices.Equal(got, want) {
		t.Errorf("extensions %v, want %v", got, want)
	}
	// crypto/x509 checks no SHA-1 signature (a P-224 CA's), so that one is
	// checked by hand.
	if leaf.SignatureAlgorithm == x509.ECDSAWithSHA1 {
		digest := sha1.Sum(leaf.RawTBSCertificate)
		if caPub, ok := ca.PublicKey.(*ecdsa.PublicKey); !ok || !ecdsa.VerifyASN1(caPub, digest[:], leaf.Signature) {
			t.Errorf("the CA did not sign the leaf")
		}
	} else if err := leaf.CheckSignatureFrom(ca); err != nil {
		t.Errorf("the CA did not sign the leaf: %v", err)
	}
	return leaf
}

func TestLeafNamesTheHostByWhatItParsesAs(t *testing.T) {
	caKey, ca := helperCA(t)
	for _, row := range []struct {
		host   string
		dns    []string
		ips    []string
		emails []string
		uris   []string
		cached bool
	}{
		{host: "api.example.com", dns: []string{"api.example.com"}, cached: true},
		{host: "*.example.com", dns: []string{"*.example.com"}, cached: true},
		{host: "UPPER.Example.COM", dns: []string{"UPPER.Example.COM"}, cached: true},
		{host: "under_score.example.com", dns: []string{"under_score.example.com"}, cached: true},
		{host: "10.1.2.3", ips: []string{"10.1.2.3"}, cached: true},
		{host: "::1", ips: []string{"::1"}, cached: true},
		{host: "joe@example.com", emails: []string{"joe@example.com"}, cached: true},
		// The bare address goes in the name, the host as given in the CN.
		{host: "Joe <joe@example.com>", emails: []string{"joe@example.com"}, cached: true},
		{host: "urn:example", uris: []string{"urn:example"}, cached: true},
		{host: "/p", uris: []string{"/p"}, cached: true},
		// No SNI, no CONNECT target and no destination IP: the leaf names
		// "" (and so its SAN is critical), and is not cached.
		{host: "", dns: []string{""}},
	} {
		t.Run(row.host, func(t *testing.T) {
			resetCertCacheForTest()
			cert, err := mintFor(t, caKey, ca, row.host, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			leaf := checkLeafShape(t, cert, row.host, ca)
			if leaf.SignatureAlgorithm != x509.ECDSAWithSHA256 {
				t.Errorf("signed with %v, want ECDSA-SHA256 for a P-256 CA", leaf.SignatureAlgorithm)
			}
			var ips, uris []string
			for _, ip := range leaf.IPAddresses {
				ips = append(ips, ip.String())
			}
			for _, u := range leaf.URIs {
				uris = append(uris, u.String())
			}
			if !slices.Equal(leaf.DNSNames, row.dns) || !slices.Equal(ips, row.ips) ||
				!slices.Equal(leaf.EmailAddresses, row.emails) || !slices.Equal(uris, row.uris) {
				t.Errorf("names dns=%q ip=%q email=%q uri=%q, want dns=%q ip=%q email=%q uri=%q",
					leaf.DNSNames, ips, leaf.EmailAddresses, uris, row.dns, row.ips, row.emails, row.uris)
			}
			if got := getCertCache().Len() == 1; got != row.cached {
				t.Errorf("cached=%v, want %v", got, row.cached)
			}
		})
	}
}

// A client verifies the leaf for the name it dialled, as a server and as a
// client certificate.
func TestLeafVerifiesForTheHostAgainstTheCA(t *testing.T) {
	caKey, ca := helperCA(t)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, host := range []string{"api.example.com", "10.1.2.3"} {
		resetCertCacheForTest()
		cert, err := mintFor(t, caKey, ca, host, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
			if _, err := cert.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
				t.Errorf("%s, usage %v: %v", host, usage, err)
			}
		}
	}
}

func TestLeafSerialIsTwentyRandomBytesWithTheTopBitCleared(t *testing.T) {
	resetCertCacheForTest()
	caKey, ca := helperCA(t)
	widest := 0
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		cert, err := mintFor(t, caKey, ca, fmt.Sprintf("h%d.example.com", i), time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		n := cert.Leaf.SerialNumber
		if n.Sign() <= 0 || n.BitLen() > 159 {
			t.Errorf("serial %v has %d bits, want positive and at most 159", n, n.BitLen())
		}
		widest = max(widest, n.BitLen())
		seen[n.String()] = true
	}
	// 32 serials of 159 random bits all under 153 bits: odds 2^-224.
	if widest < 153 {
		t.Errorf("the widest of 32 serials has %d bits, want about 159", widest)
	}
	if len(seen) != 32 {
		t.Errorf("%d distinct serials of 32", len(seen))
	}
}

func TestLeafValidityIsAYearBeforeTheBackdateToAYearFromNow(t *testing.T) {
	caKey, ca := helperCA(t)
	t.Run("live", func(t *testing.T) {
		resetCertCacheForTest()
		before := time.Now()
		cert, err := mintFor(t, caKey, ca, "live.example.com", time.Time{})
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		nb, na := cert.Leaf.NotBefore, cert.Leaf.NotAfter
		if nb.Before(before.AddDate(-1, 0, 0).Truncate(time.Second)) || nb.After(after.AddDate(-1, 0, 0)) {
			t.Errorf("NotBefore %v, want a year before now (%v)", nb, before)
		}
		if na.Before(before.AddDate(1, 0, 0).Truncate(time.Second)) || na.After(after.AddDate(1, 0, 0)) {
			t.Errorf("NotAfter %v, want a year from now (%v)", na, before)
		}
	})
	t.Run("frozen", func(t *testing.T) {
		resetCertCacheForTest()
		backdate := time.Date(2023, 5, 6, 7, 8, 9, 500000000, time.FixedZone("UTC+1", 3600))
		before := time.Now()
		cert, err := mintFor(t, caKey, ca, "frozen.example.com", backdate)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		if want := time.Date(2022, 5, 6, 6, 8, 9, 0, time.UTC); !cert.Leaf.NotBefore.Equal(want) {
			t.Errorf("NotBefore %v, want %v: a year before the backdate", cert.Leaf.NotBefore, want)
		}
		if na := cert.Leaf.NotAfter; na.Before(before.AddDate(1, 0, 0).Truncate(time.Second)) || na.After(after.AddDate(1, 0, 0)) {
			t.Errorf("NotAfter %v, want a year from now, whatever the backdate", na)
		}
	})
}

func TestLeafSignatureAlgorithmFollowsTheCAKey(t *testing.T) {
	rsaOf := func(bits int) *rsa.PublicKey {
		return &rsa.PublicKey{N: new(big.Int).Lsh(big.NewInt(1), uint(bits-1)), E: 65537}
	}
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name string
		pub  crypto.PublicKey
		want x509.SignatureAlgorithm
	}{
		{"P-256", &ecdsa.PublicKey{Curve: elliptic.P256()}, x509.ECDSAWithSHA256},
		{"P-384", &ecdsa.PublicKey{Curve: elliptic.P384()}, x509.ECDSAWithSHA384},
		{"P-521", &ecdsa.PublicKey{Curve: elliptic.P521()}, x509.ECDSAWithSHA512},
		{"P-224", &ecdsa.PublicKey{Curve: elliptic.P224()}, x509.ECDSAWithSHA1},
		{"RSA-1024", rsaOf(1024), x509.SHA1WithRSA},
		{"RSA-2047", rsaOf(2047), x509.SHA1WithRSA},
		{"RSA-2048", rsaOf(2048), x509.SHA256WithRSA},
		{"RSA-3071", rsaOf(3071), x509.SHA256WithRSA},
		{"RSA-3072", rsaOf(3072), x509.SHA384WithRSA},
		{"RSA-4095", rsaOf(4095), x509.SHA384WithRSA},
		{"RSA-4096", rsaOf(4096), x509.SHA512WithRSA},
		{"Ed25519", ed.Public(), x509.UnknownSignatureAlgorithm},
	} {
		if got := leafSignatureAlgorithm(row.pub); got != row.want {
			t.Errorf("%s: %v, want %v", row.name, got, row.want)
		}
	}
}

// The algorithm is the one a leaf is signed with, for CA keys where
// crypto/x509's own default differs (RSA-3072, P-224) and where it is the
// only one (Ed25519).
func TestLeafIsSignedWithTheCAKeysAlgorithm(t *testing.T) {
	rsa3072, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	p224, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name  string
		key   crypto.Signer
		caAlg x509.SignatureAlgorithm
		want  x509.SignatureAlgorithm
	}{
		{"RSA-3072", rsa3072, 0, x509.SHA384WithRSA},
		{"P-224", p224, x509.ECDSAWithSHA256, x509.ECDSAWithSHA1},
		{"P-384", p384, 0, x509.ECDSAWithSHA384},
		{"Ed25519", ed, 0, x509.PureEd25519},
	} {
		t.Run(row.name, func(t *testing.T) {
			resetCertCacheForTest()
			ca := leafTestCA(t, row.key, row.caAlg)
			cert, err := mintFor(t, row.key, ca, "api.example.com", time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			leaf := checkLeafShape(t, cert, "api.example.com", ca)
			if leaf.SignatureAlgorithm != row.want {
				t.Errorf("signed with %v, want %v", leaf.SignatureAlgorithm, row.want)
			}
		})
	}
}

// Each failure is an error, mints nothing and caches nothing.
func TestLeafFailuresAreErrors(t *testing.T) {
	caKey, ca := helperCA(t)
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name  string
		key   any
		ca    *x509.Certificate
		host  string
		want  string
		alsoA string
	}{
		{name: "the CA key is not a signer", key: "not a key", ca: ca, host: "api.example.com", want: "failed to typecast the caPrivKey"},
		{name: "no CA certificate", key: caKey, ca: nil, host: "api.example.com", want: "failed to sign server certificate"},
		{name: "the CA key is not the CA certificate's", key: otherKey, ca: ca, host: "api.example.com",
			want: "failed to sign server certificate", alsoA: "doesn't match parent's PublicKey"},
		// A name a certificate cannot carry. cfssl panicked on it
		// ("Unable to wrap a wrapped error"), out of the client's handshake.
		{name: "a host that is not ASCII", key: caKey, ca: ca, host: "bad\xffhost.example.com", want: "failed to sign server certificate"},
		// A URI crypto/x509 writes but will not read back (an empty label).
		{name: "a URI with an empty label", key: caKey, ca: ca, host: "http://a..b/", want: "failed to sign server certificate", alsoA: "invalid domain"},
	} {
		t.Run(row.name, func(t *testing.T) {
			resetCertCacheForTest()
			cert, err := mintFor(t, row.key, row.ca, row.host, time.Time{})
			if err == nil || cert != nil {
				t.Fatalf("got a leaf (%v), want an error", cert != nil)
			}
			if !strings.HasPrefix(err.Error(), row.want) || !strings.Contains(err.Error(), row.alsoA) {
				t.Errorf("error %q, want it to start %q and name %q", err, row.want, row.alsoA)
			}
			if n := getCertCache().Len(); n != 0 {
				t.Errorf("%d leaves cached after a failure", n)
			}
		})
	}
}
