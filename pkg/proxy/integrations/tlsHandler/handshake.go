package tlsHandler

import (
	"errors"
	"fmt"
	"go.keploy.io/server/pkg/proxy/util"
	"net"
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

func Handshake(requestBuffer []byte, clientConn, destConn net.Conn) (*TLSPassThroughConnection, *TLSPassThroughConnection, error) {
	clientTLSConn := TLSPassThroughConnection{
		conn: clientConn,
	}
	destTLSConn := TLSPassThroughConnection{
		conn: destConn,
	}

	// Set isHandshakeComplete of both connections to false as initial state
	clientTLSConn.isHandshakeComplete.Store(false)
	destTLSConn.isHandshakeComplete.Store(false)

	// Write client hello to the destination connection
	hdr := requestBuffer[:recordHeaderLen]
	typ := recordType(hdr[0])
	if typ != recordTypeHandshake {
		return nil, nil, errors.New("first record received not a handshake type")
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
	fmt.Printf("[Debug] Destination Local Address before client hello: %s\n", destConn.LocalAddr())
	_, err = destConn.Write(requestBuffer)
	if err != nil {
		return nil, nil, err
	}

	// Read server hello from destination connection and write it to client connection
	responseBuffer, err := util.ReadBytes(destConn)
	if err != nil {
		return nil, nil, err
	}
	hdr = responseBuffer[:recordHeaderLen]
	typ = recordType(hdr[0])
	if typ != recordTypeHandshake {
		return nil, nil, errors.New("first record received not a handshake type")
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
	clientTLSConn.Vers = serverHello.supportedVersion
	destTLSConn.Vers = serverHello.supportedVersion
	clientTLSConn.haveVers = true
	destTLSConn.haveVers = true
	clientTLSConn.CipherSuite = serverHello.cipherSuite
	destTLSConn.CipherSuite = serverHello.cipherSuite
	clientTLSConn.In.version = serverHello.supportedVersion
	clientTLSConn.Out.version = serverHello.supportedVersion
	destTLSConn.In.version = serverHello.supportedVersion
	clientTLSConn.Out.version = serverHello.supportedVersion

	_, err = clientConn.Write(responseBuffer)
	if err != nil {
		return nil, nil, err
	}

	// Read final change cipher spec and finished message from client
	finalBuffer, err := util.ReadBytes(clientConn)
	hdr = finalBuffer[:recordHeaderLen]
	typ = recordType(hdr[0])
	if typ != recordTypeChangeCipherSpec {
		return nil, nil, errors.New("first record received not a handshake type")
	}

	_, err = destConn.Write(finalBuffer)
	if err != nil {
		return nil, nil, err
	}

	if !clientTLSConn.isHandshakeComplete.Load() {
		clientTLSConn.isHandshakeComplete.Store(true)
	}
	if !destTLSConn.isHandshakeComplete.Load() {
		destTLSConn.isHandshakeComplete.Store(true)
	}

	return &clientTLSConn, &destTLSConn, nil
}
