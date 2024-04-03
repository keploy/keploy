package tlsHandler

import (
	"bufio"
	"errors"
	"fmt"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
	"hash"
	"io"
	"net"
	"time"
)

func GetDestinationURL(clientHelloBuffer []byte) (string, error) {
	// Write client hello to the destination connection
	hdr := clientHelloBuffer[:recordHeaderLen]
	typ := recordType(hdr[0])
	if typ != recordTypeHandshake {
		return "", errors.New("first record received not a handshake type")
	}
	n := int(hdr[3])<<8 | int(hdr[4])
	record := clientHelloBuffer[recordHeaderLen : recordHeaderLen+n]

	clientHelloMessage := new(clientHelloMsg)
	if !clientHelloMessage.unmarshal(record) {
		return "", errors.New("failed to unmarshall the client hello buffer")
	}
	serverName := clientHelloMessage.serverName
	return serverName, nil
}

func Handshake(requestBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger) (*TLSPassThroughConnection, *TLSPassThroughConnection, error) {
	// TODO: This works for the handshake format of TLS 1.3, must write functions for other TLS versions
	clientTLSConn := TLSPassThroughConnection{
		conn: clientConn,
	}
	destTLSConn := TLSPassThroughConnection{
		conn: destConn,
	}

	// Set isHandshakeComplete of both connections to false as initial state
	clientTLSConn.isHandshakeComplete.Store(false)
	destTLSConn.isHandshakeComplete.Store(false)

	// Create bufio Readers for client and destination connections
	clientBufReader := bufio.NewReader(clientConn)
	destBufReader := bufio.NewReader(destConn)

	// Write client hello to the destination connection
	hdr := requestBuffer[:recordHeaderLen]
	typ := recordType(hdr[0])
	if typ != recordTypeHandshake {
		_, _ = destConn.Write(requestBuffer)
		return nil, nil, errors.New(fmt.Sprintf("expected clientHello, got type %d", typ))
	}
	n := int(hdr[3])<<8 | int(hdr[4])
	record := requestBuffer[recordHeaderLen : recordHeaderLen+n]

	clientHelloMessage, err := clientTLSConn.unmarshalHandshakeMessage(record)
	if err != nil {
		return nil, nil, err
	}
	clientHello, ok := clientHelloMessage.(*clientHelloMsg)
	if !ok {
		return nil, nil, errors.New("failed to cast clientHello buffer into clientHelloMsg type")
	}
	clientTLSConn.ClientRandom = clientHello.random
	destTLSConn.ClientRandom = clientHello.random
	if err != nil {
		return nil, nil, err
	}
	_, err = destConn.Write(requestBuffer)
	if err != nil {
		return nil, nil, err
	}

	// Read server hello from destination connection and write it to client connection
	responseBuffer, err := ReadBytes(destBufReader)
	if err != nil {
		return nil, nil, err
	}
	hdr = responseBuffer[:recordHeaderLen]
	typ = recordType(hdr[0])
	if typ != recordTypeHandshake {
		_, err = clientConn.Write(responseBuffer)
		return nil, nil, errors.New(fmt.Sprintf("expected serverHello, got type %d", typ))
	}
	n = int(hdr[3])<<8 | int(hdr[4])
	record = responseBuffer[recordHeaderLen : recordHeaderLen+n]

	serverHelloMessage, err := destTLSConn.unmarshalHandshakeMessage(record)
	if err != nil {
		return nil, nil, err
	}
	serverHello, ok := serverHelloMessage.(*serverHelloMsg)
	if !ok {
		return nil, nil, errors.New("failed to cast serverHello buffer into serverHelloMsg type")
	}
	clientTLSConn.ServerRandom = serverHello.random
	destTLSConn.ServerRandom = serverHello.random
	clientTLSConn.haveVers = true
	destTLSConn.haveVers = true
	clientTLSConn.CipherSuite = serverHello.cipherSuite
	destTLSConn.CipherSuite = serverHello.cipherSuite
	_, err = clientConn.Write(responseBuffer)
	if err != nil {
		return nil, nil, err
	}

	// Find out if the TLS version is 1.2 or 1.3 based on vers and supportedVersion fields
	if serverHello.vers == 771 && serverHello.supportedVersion == 0 {
		// TLSv1.2
		clientTLSConn.Vers = serverHello.vers
		destTLSConn.Vers = serverHello.vers
		clientTLSConn.In.version = serverHello.vers
		clientTLSConn.Out.version = serverHello.vers
		destTLSConn.In.version = serverHello.vers
		destTLSConn.Out.version = serverHello.vers
	} else if serverHello.vers == 771 && serverHello.supportedVersion == 772 {
		// TLSv1.3
		clientTLSConn.Vers = serverHello.supportedVersion
		destTLSConn.Vers = serverHello.supportedVersion
		clientTLSConn.In.version = serverHello.supportedVersion
		clientTLSConn.Out.version = serverHello.supportedVersion
		destTLSConn.In.version = serverHello.supportedVersion
		destTLSConn.Out.version = serverHello.supportedVersion
	}

	for {
		hdr, err := clientBufReader.Peek(recordHeaderLen)
		if err != nil {
			return nil, nil, err
		}
		typ = recordType(hdr[0])
		if typ == recordTypeApplicationData {
			break
		}
		clientBuffer, err := ReadBytes(clientBufReader)
		_, err = destConn.Write(clientBuffer)
		if err != nil {
			return nil, nil, err
		}

		hdr, err = destBufReader.Peek(recordHeaderLen)
		if err != nil {
			return nil, nil, err
		}
		typ = recordType(hdr[0])
		if typ == recordTypeApplicationData {
			break
		}
		destBuffer, err := ReadBytes(destBufReader)
		_, err = clientConn.Write(destBuffer)
		if err != nil {
			return nil, nil, err
		}
	}

	clientMultiReader := io.MultiReader(clientBufReader, clientConn)
	clientConn = &util.CustomConn{
		Conn:   clientConn,
		R:      clientMultiReader,
		Logger: logger,
	}

	destMultiReader := io.MultiReader(destBufReader, destConn)
	destConn = &util.CustomConn{
		Conn:   destConn,
		R:      destMultiReader,
		Logger: logger,
	}

	clientTLSConn.conn = clientConn
	destTLSConn.conn = destConn
	if !clientTLSConn.isHandshakeComplete.Load() {
		clientTLSConn.isHandshakeComplete.Store(true)
	}
	if !destTLSConn.isHandshakeComplete.Load() {
		destTLSConn.isHandshakeComplete.Store(true)
	}

	return &clientTLSConn, &destTLSConn, nil
}

func EstablishKeys(clientTLSConn, destTLSConn *TLSPassThroughConnection, masterSecret []byte) {
	suite := CipherSuiteByID(clientTLSConn.CipherSuite)
	clientMAC, serverMAC, clientKey, serverKey, clientIV, serverIV :=
		keysFromMasterSecret(clientTLSConn.Vers, suite, masterSecret, clientTLSConn.ClientRandom, clientTLSConn.ServerRandom, suite.macLen, suite.keyLen, suite.ivLen)
	var clientCipher, serverCipher any
	var clientHash, serverHash hash.Hash
	if suite.cipher != nil {
		clientCipher = suite.cipher(clientKey, clientIV, false /* not for reading */)
		clientHash = suite.mac(clientMAC)
		serverCipher = suite.cipher(serverKey, serverIV, true /* for reading */)
		serverHash = suite.mac(serverMAC)
	} else {
		clientCipher = suite.aead(clientKey, clientIV)
		serverCipher = suite.aead(serverKey, serverIV)
	}

	clientTLSConn.In.prepareCipherSpec(clientTLSConn.Vers, clientCipher, clientHash)
	clientTLSConn.Out.prepareCipherSpec(clientTLSConn.Vers, serverCipher, serverHash)

	destTLSConn.In.prepareCipherSpec(clientTLSConn.Vers, serverCipher, serverHash)
	destTLSConn.Out.prepareCipherSpec(destTLSConn.Vers, clientCipher, clientHash)
}

// ReadBytes function is utilized to read the complete message from the reader until the end of the file (EOF).
// It returns the content as a byte array.
func ReadBytes(reader *bufio.Reader) ([]byte, error) {
	var buffer []byte
	const maxEmptyReads = 5
	emptyReads := 0

	for {
		buf := make([]byte, 1024)
		n, err := reader.Read(buf)

		if n > 0 {
			buffer = append(buffer, buf[:n]...)
			emptyReads = 0 // reset the counter because we got some data
		}

		if err != nil {
			if err == io.EOF {
				emptyReads++
				if emptyReads >= maxEmptyReads {
					return buffer, err // multiple EOFs in a row, probably a true EOF
				}
				time.Sleep(time.Millisecond * 100) // sleep before trying again
				continue
			}
			return buffer, err
		}

		if n < len(buf) {
			break
		}
	}

	return buffer, nil
}
