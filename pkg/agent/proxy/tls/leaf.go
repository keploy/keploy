package tls

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/mail"
	"net/url"
	"time"
)

// mintLeaf signs a fresh leaf certificate for host with the run's CA, valid
// from notBefore to notAfter.
//
// The certificate is the one the cfssl local signer made under its default
// signing profile, which minted these leaves before: the standard library
// makes it now, so cfssl and the modules it linked are out of the binary, and
// clients see the same leaf as before, field for field:
//   - a fresh ECDSA P-256 key, the leaf alone in the chain (no CA after it);
//   - a random serial of 20 bytes with the top bit cleared, so it is positive
//     and at most the 20 octets RFC 5280 allows;
//   - the subject is CN=host alone, and host is the one subject alternative
//     name, of the kind it parses as (leafNamedFor);
//   - key usage digital signature and key encipherment, extended key usage
//     server auth then client auth, basic constraints CA:FALSE;
//   - the subject key id is the SHA-1 of the public key's bits (RFC 5280
//     4.2.1.2, method 1), and the authority key id is the CA's;
//   - signed with the algorithm leafSignatureAlgorithm picks for the CA key.
//
// It fails, and mints nothing, where the old signer failed: with no CA
// certificate, a CA key that does not match it, or a name crypto/x509 will
// not read back. One difference: a host a certificate cannot carry (a name
// that is not ASCII) is an error here, where cfssl panicked ("Unable to wrap
// a wrapped error") in the handshake of the client that sent it.
func mintLeaf(host string, caKey crypto.Signer, caCert *x509.Certificate, notBefore, notAfter time.Time) (*tls.Certificate, error) {
	if caCert == nil {
		return nil, errors.New("failed to sign server certificate: no CA certificate to sign with")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate the server key: %w", err)
	}
	pub, err := key.PublicKey.Bytes()
	if err != nil {
		return nil, fmt.Errorf("failed to encode the server public key: %w", err)
	}
	// For an EC key the public key's bits are its uncompressed point
	// (RFC 5480 2.2), which is what PublicKey.Bytes returns.
	ski := sha1.Sum(pub)

	serial := make([]byte, 20)
	// crypto/rand.Read never returns an error: it ends the program instead.
	_, _ = rand.Read(serial)
	serial[0] &= 0x7F

	tmpl := &x509.Certificate{
		SerialNumber:          new(big.Int).SetBytes(serial),
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		SubjectKeyId:          ski[:],
		SignatureAlgorithm:    leafSignatureAlgorithm(caKey.Public()),
	}
	leafNamedFor(tmpl, host)

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign server certificate: %w", err)
	}
	// Parsed once here, before the leaf is cached and shared: crypto/tls
	// parses Certificate.Leaf lazily otherwise, which is not goroutine-safe.
	// crypto/x509 writes some names it will not read back (a URI with an
	// empty label); cfssl failed those as a signing error too.
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("failed to sign server certificate: %w", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// leafNamedFor puts host on tmpl as the subject alternative name of the kind
// it parses as, tried in this order: an IP address, an email address (its
// bare address), a URI, else a DNS name. An empty host is a DNS name too, as
// it was: the leaf then names "" and its empty subject makes the SAN
// extension critical (RFC 5280 4.2.1.6).
func leafNamedFor(tmpl *x509.Certificate, host string) {
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else if addr, err := mail.ParseAddress(host); err == nil {
		tmpl.EmailAddresses = []string{addr.Address}
	} else if uri, err := url.ParseRequestURI(host); err == nil {
		tmpl.URIs = []*url.URL{uri}
	} else {
		tmpl.DNSNames = []string{host}
	}
}

// leafSignatureAlgorithm is the algorithm a leaf is signed with, by the CA's
// key: SHA-256, SHA-384 or SHA-512 with ECDSA on P-256, P-384 and P-521, and
// ECDSA with SHA-1 on any other curve; with RSA, SHA-512 from 4096 bits,
// SHA-384 from 3072, SHA-256 from 2048 and SHA-1 under it. Any other key
// (Ed25519) gets the zero value, so crypto/x509 picks its only algorithm.
func leafSignatureAlgorithm(caPub crypto.PublicKey) x509.SignatureAlgorithm {
	switch pub := caPub.(type) {
	case *rsa.PublicKey:
		switch bits := pub.N.BitLen(); {
		case bits >= 4096:
			return x509.SHA512WithRSA
		case bits >= 3072:
			return x509.SHA384WithRSA
		case bits >= 2048:
			return x509.SHA256WithRSA
		default:
			return x509.SHA1WithRSA
		}
	case *ecdsa.PublicKey:
		switch pub.Curve {
		case elliptic.P256():
			return x509.ECDSAWithSHA256
		case elliptic.P384():
			return x509.ECDSAWithSHA384
		case elliptic.P521():
			return x509.ECDSAWithSHA512
		default:
			return x509.ECDSAWithSHA1
		}
	default:
		return x509.UnknownSignatureAlgorithm
	}
}
