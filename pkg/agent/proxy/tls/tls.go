package tls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"time"

	"github.com/cloudflare/cfssl/helpers"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func IsTLSHandshake(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 && (data[2] == 0x00 || data[2] == 0x01 || data[2] == 0x02 || data[2] == 0x03)
}

// ExtractSNI parses the SNI extension from a raw TLS ClientHello.
// Returns empty string if SNI cannot be extracted.
// The data parameter contains the full ClientHello bytes (peeked from bufio.Reader).
func ExtractSNI(data []byte) string {
	// TLS record header: content_type(1) + version(2) + length(2) = 5 bytes
	if len(data) < 5 || data[0] != 0x16 {
		return ""
	}
	recordLen := int(data[3])<<8 | int(data[4])
	if len(data) < 5+recordLen {
		// Partial record — try to parse what we have
		recordLen = len(data) - 5
	}
	hs := data[5 : 5+recordLen]

	// Handshake header: type(1) + length(3)
	if len(hs) < 4 || hs[0] != 0x01 { // 0x01 = ClientHello
		return ""
	}
	hsLen := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
	if len(hs) < 4+hsLen {
		hsLen = len(hs) - 4
	}
	ch := hs[4 : 4+hsLen]

	// ClientHello body: version(2) + random(32) = 34 bytes minimum
	if len(ch) < 34 {
		return ""
	}
	pos := 34

	// session_id: length(1) + data
	if pos >= len(ch) {
		return ""
	}
	sidLen := int(ch[pos])
	pos++
	pos += sidLen
	if pos >= len(ch) {
		return ""
	}

	// cipher_suites: length(2) + data
	if pos+2 > len(ch) {
		return ""
	}
	csLen := int(ch[pos])<<8 | int(ch[pos+1])
	pos += 2 + csLen
	if pos >= len(ch) {
		return ""
	}

	// compression_methods: length(1) + data
	if pos >= len(ch) {
		return ""
	}
	cmLen := int(ch[pos])
	pos++
	pos += cmLen
	if pos >= len(ch) {
		return ""
	}

	// Extensions: total_length(2) + extension data
	if pos+2 > len(ch) {
		return ""
	}
	extLen := int(ch[pos])<<8 | int(ch[pos+1])
	pos += 2
	extEnd := pos + extLen
	if extEnd > len(ch) {
		extEnd = len(ch)
	}

	// Walk extensions looking for SNI (type 0x0000)
	for pos+4 <= extEnd {
		extType := int(ch[pos])<<8 | int(ch[pos+1])
		extDataLen := int(ch[pos+2])<<8 | int(ch[pos+3])
		pos += 4
		if pos+extDataLen > extEnd {
			break
		}
		if extType == 0x0000 { // SNI extension
			return parseSNIExtension(ch[pos : pos+extDataLen])
		}
		pos += extDataLen
	}
	return ""
}

// parseSNIExtension extracts the hostname from an SNI extension payload.
func parseSNIExtension(data []byte) string {
	// SNI extension: list_length(2) + [ type(1) + name_length(2) + name ]
	if len(data) < 2 {
		return ""
	}
	listLen := int(data[0])<<8 | int(data[1])
	if listLen+2 > len(data) {
		listLen = len(data) - 2
	}
	p := 2
	end := 2 + listLen
	for p+3 <= end {
		nameType := data[p]
		nameLen := int(data[p+1])<<8 | int(data[p+2])
		p += 3
		if p+nameLen > end {
			break
		}
		if nameType == 0x00 { // host_name type
			return string(data[p : p+nameLen])
		}
		p += nameLen
	}
	return ""
}

// lastSNI stores the most recent ServerName seen in GetConfigForClient,
// used for diagnostic logging on handshake failures.
var lastSNI string

func HandleTLSConnection(_ context.Context, logger *zap.Logger, conn net.Conn, backdate time.Time) (net.Conn, bool, error) {
	// 1. Load the Proxy's Signing CA (Used to generate server certs)
	caPrivKey, err := helpers.ParsePrivateKeyPEM(caPKey)
	if err != nil {
		utils.LogError(logger, err, "Failed to parse CA private key")
		return nil, false, err
	}
	caCertParsed, err := helpers.ParseCertificatePEM(caCrt)
	if err != nil {
		utils.LogError(logger, err, "Failed to parse CA certificate")
		return nil, false, err
	}

	config := &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			lastSNI = hello.ServerName
			// Check if client supports http/1.1
			clientSupportsHTTP1 := false
			for _, proto := range hello.SupportedProtos {
				if proto == "http/1.1" {
					clientSupportsHTTP1 = true
					break
				}
			}

			var nextProtos []string
			if clientSupportsHTTP1 {
				// Client supports HTTP/1.1, prefer it (safer for HTTP traffic)
				nextProtos = []string{"http/1.1"}
				logger.Debug("Client supports http/1.1, using http/1.1 only", zap.Strings("clientProtos", hello.SupportedProtos))
			} else {
				// Client only supports H2 (likely gRPC), must offer H2
				nextProtos = []string{"h2", "http/1.1"}
				logger.Debug("Client requires H2 (likely gRPC), offering H2", zap.Strings("clientProtos", hello.SupportedProtos))
			}

			return &tls.Config{
				NextProtos: nextProtos,
				GetCertificate: func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
					return CertForClient(logger, clientHello, caPrivKey, caCertParsed, backdate)
				},
				ClientAuth: tls.RequestClientCert,
				VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
					return nil
				},
				ClientCAs: nil,
			}, nil
		},
	}

	// Wrap the TCP conn with TLS
	tlsConn := tls.Server(conn, config)

	// Perform the handshake
	err = tlsConn.Handshake()
	if err != nil {
		logger.Debug("TLS handshake failed (non-fatal, may be passthrough traffic)",
			zap.String("sni", lastSNI),
			zap.String("remoteAddr", conn.RemoteAddr().String()),
			zap.Error(err))
		return nil, false, err
	}

	// 4. (Optional) Check what kind of connection happened
	// You can log this to verify if the client actually used mTLS or just standard TLS
	isMTLS := false
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) > 0 {
		logger.Debug("mTLS Handshake Success", zap.String("client_subject", state.PeerCertificates[0].Subject.CommonName))
		isMTLS = true
	} else {
		logger.Debug("Standard TLS Handshake Success (No Client Cert Provided)")
	}

	return tlsConn, isMTLS, nil
}
