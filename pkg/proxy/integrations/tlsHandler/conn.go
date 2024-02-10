package tlsHandler

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type TLSPassThroughConnection struct {
	conn                net.Conn
	isHandshakeComplete atomic.Bool
	Vers                uint16 // TLS version
	haveVers            bool   // version has been negotiated
	CipherSuite         uint16

	// input/output
	In, Out   halfConn
	rawInput  bytes.Buffer // raw input, starting with a record header
	input     bytes.Reader // application data waiting to be read, from rawInput.Next
	hand      bytes.Buffer // handshake data waiting to be read
	buffering bool         // whether records are buffered In sendBuf
	sendBuf   []byte       // a buffer of records waiting to be sent

	// bytesSent counts the bytes of application data sent.
	// packetsSent counts packets.
	bytesSent   int64
	packetsSent int64

	// retryCount counts the number of consecutive non-advancing records
	// received by Conn.readRecord. That is, records that neither advance the
	// handshake, nor deliver application data. Protected by In.Mutex.
	retryCount int

	// activeCall indicates whether Close has been call In the low bit.
	// the rest of the bits are the number of goroutines In Conn.Write.
	activeCall atomic.Int32

	ClientRandom []byte
	tmp          [16]byte
}

// LocalAddr returns the local network address.
func (tpc *TLSPassThroughConnection) LocalAddr() net.Addr {
	return tpc.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (tpc *TLSPassThroughConnection) RemoteAddr() net.Addr {
	return tpc.conn.RemoteAddr()
}

// SetDeadline sets the read and write deadlines associated with the connection.
// A zero value for t means [Conn.Read] and [Conn.Write] will not time Out.
// After a Write has timed Out, the TLS state is corrupt and all future writes will return the same error.
func (tpc *TLSPassThroughConnection) SetDeadline(t time.Time) error {
	return tpc.conn.SetDeadline(t)
}

// SetReadDeadline sets the read deadline on the underlying connection.
// A zero value for t means [Conn.Read] will not time Out.
func (tpc *TLSPassThroughConnection) SetReadDeadline(t time.Time) error {
	return tpc.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the write deadline on the underlying connection.
// A zero value for t means [Conn.Write] will not time Out.
// After a [Conn.Write] has timed Out, the TLS state is corrupt and all future writes will return the same error.
func (tpc *TLSPassThroughConnection) SetWriteDeadline(t time.Time) error {
	return tpc.conn.SetWriteDeadline(t)
}

// NetConn returns the underlying connection that is wrapped by tpc.
// Note that writing to or reading from this connection directly will corrupt the
// TLS session.
func (tpc *TLSPassThroughConnection) NetConn() net.Conn {
	return tpc.conn
}

// A halfConn represents one direction of the record layer
// connection, either sending or receiving.
type halfConn struct {
	sync.Mutex

	err     error  // first permanent error
	version uint16 // protocol version
	cipher  any    // cipher algorithm
	mac     hash.Hash
	seq     [8]byte // 64-bit sequence number

	scratchBuf [13]byte // to avoid allocs; interface method args escape

	nextCipher any       // next encryption state
	nextMac    hash.Hash // next MAC algorithm

	trafficSecret []byte // current TLS 1.3 traffic secret
}

type permanentError struct {
	err net.Error
}

func (e *permanentError) Error() string   { return e.err.Error() }
func (e *permanentError) Unwrap() error   { return e.err }
func (e *permanentError) Timeout() bool   { return e.err.Timeout() }
func (e *permanentError) Temporary() bool { return false }

func (hc *halfConn) setErrorLocked(err error) error {
	if e, ok := err.(net.Error); ok {
		hc.err = &permanentError{err: e}
	} else {
		hc.err = err
	}
	return hc.err
}

// prepareCipherSpec sets the encryption and MAC states
// that a subsequent changeCipherSpec will use.
func (hc *halfConn) prepareCipherSpec(version uint16, cipher any, mac hash.Hash) {
	hc.version = version
	hc.nextCipher = cipher
	hc.nextMac = mac
}

func (hc *halfConn) prepareCipherSpecTLS13(version uint16, cipher any) {
	hc.version = version
	hc.cipher = cipher
}

// changeCipherSpec changes the encryption and MAC states
// to the ones previously passed to prepareCipherSpec.
func (hc *halfConn) changeCipherSpec() error {
	if hc.nextCipher == nil || hc.version == VersionTLS13 {
		return alertInternalError
	}
	hc.cipher = hc.nextCipher
	hc.mac = hc.nextMac
	hc.nextCipher = nil
	hc.nextMac = nil
	for i := range hc.seq {
		hc.seq[i] = 0
	}
	return nil
}

func (hc *halfConn) SetTrafficSecret(suite *cipherSuiteTLS13, secret []byte) {
	hc.trafficSecret = secret
	key, iv := suite.trafficKey(secret)
	hc.cipher = suite.aead(key, iv)
	for i := range hc.seq {
		hc.seq[i] = 0
	}
}

// incSeq increments the sequence number.
func (hc *halfConn) incSeq() {
	for i := 7; i >= 0; i-- {
		hc.seq[i]++
		if hc.seq[i] != 0 {
			return
		}
	}

	// Not allowed to let sequence number wrap.
	// Instead, must renegotiate before it does.
	// Not likely enough to bother.
	panic("TLS: sequence number wraparound")
}

// explicitNonceLen returns the number of bytes of explicit nonce or IV included
// In each record. Explicit nonces are present only In CBC modes after TLS 1.0
// and In certain AEAD modes In TLS 1.2.
func (hc *halfConn) explicitNonceLen() int {
	if hc.cipher == nil {
		return 0
	}

	switch c := hc.cipher.(type) {
	case cipher.Stream:
		return 0
	case aead:
		return c.explicitNonceLen()
	case cbcMode:
		// TLS 1.1 introduced a per-record explicit IV to fix the BEAST attack.
		if hc.version >= VersionTLS11 {
			return c.BlockSize()
		}
		return 0
	default:
		panic("unknown cipher type")
	}
}

// extractPadding returns, In constant time, the length of the padding to remove
// from the end of payload. It also returns a byte which is equal to 255 if the
// padding was valid and 0 otherwise. See RFC 2246, Section 6.2.3.2.
func extractPadding(payload []byte) (toRemove int, good byte) {
	if len(payload) < 1 {
		return 0, 0
	}

	paddingLen := payload[len(payload)-1]
	t := uint(len(payload)-1) - uint(paddingLen)
	// if len(payload) >= (paddingLen - 1) then the MSB of t is zero
	good = byte(int32(^t) >> 31)

	// The maximum possible padding length plus the actual length field
	toCheck := 256
	// The length of the padded data is public, so we can use an if here
	if toCheck > len(payload) {
		toCheck = len(payload)
	}

	for i := 0; i < toCheck; i++ {
		t := uint(paddingLen) - uint(i)
		// if i <= paddingLen then the MSB of t is zero
		mask := byte(int32(^t) >> 31)
		b := payload[len(payload)-1-i]
		good &^= mask&paddingLen ^ mask&b
	}

	// We AND together the bits of good and replicate the result across
	// all the bits.
	good &= good << 4
	good &= good << 2
	good &= good << 1
	good = uint8(int8(good) >> 7)

	// Zero the padding length on error. This ensures any unchecked bytes
	// are included In the MAC. Otherwise, an attacker that could
	// distinguish MAC failures from padding failures could mount an attack
	// similar to POODLE In SSL 3.0: given a good ciphertext that uses a
	// full block's worth of padding, replace the final block with another
	// block. If the MAC check passed but the padding check failed, the
	// last byte of that block decrypted to the block size.
	//
	// See also macAndPaddingGood logic below.
	paddingLen &= good

	toRemove = int(paddingLen) + 1
	return
}

func roundUp(a, b int) int {
	return a + (b-a%b)%b
}

// cbcMode is an interface for block ciphers using cipher block chaining.
type cbcMode interface {
	cipher.BlockMode
	SetIV([]byte)
}

// decrypt authenticates and decrypts the record if protection is active at
// this stage. The returned plaintext might overlap with the input.
func (hc *halfConn) decrypt(record []byte) ([]byte, recordType, error) {
	var plaintext []byte
	typ := recordType(record[0])
	payload := record[recordHeaderLen:]

	// In TLS 1.3, change_cipher_spec messages are to be ignored without being
	// decrypted. See RFC 8446, Appendix D.4.
	if hc.version == VersionTLS13 && typ == recordTypeChangeCipherSpec {
		return payload, typ, nil
	}

	paddingGood := byte(255)
	paddingLen := 0

	explicitNonceLen := hc.explicitNonceLen()

	if hc.cipher != nil {
		switch c := hc.cipher.(type) {
		case cipher.Stream:
			c.XORKeyStream(payload, payload)
		case aead:
			if len(payload) < explicitNonceLen {
				return nil, 0, alertBadRecordMAC
			}
			nonce := payload[:explicitNonceLen]
			if len(nonce) == 0 {
				nonce = hc.seq[:]
			}
			payload = payload[explicitNonceLen:]

			var additionalData []byte
			if hc.version == VersionTLS13 {
				additionalData = record[:recordHeaderLen]
			} else {
				additionalData = append(hc.scratchBuf[:0], hc.seq[:]...)
				additionalData = append(additionalData, record[:3]...)
				n := len(payload) - c.Overhead()
				additionalData = append(additionalData, byte(n>>8), byte(n))
			}

			var err error
			plaintext, err = c.Open(payload[:0], nonce, payload, additionalData)
			if err != nil {
				return nil, 0, alertBadRecordMAC
			}
		case cbcMode:
			blockSize := c.BlockSize()
			minPayload := explicitNonceLen + roundUp(hc.mac.Size()+1, blockSize)
			if len(payload)%blockSize != 0 || len(payload) < minPayload {
				return nil, 0, alertBadRecordMAC
			}

			if explicitNonceLen > 0 {
				c.SetIV(payload[:explicitNonceLen])
				payload = payload[explicitNonceLen:]
			}
			c.CryptBlocks(payload, payload)

			// In a limited attempt to protect against CBC padding oracles like
			// Lucky13, the data past paddingLen (which is secret) is passed to
			// the MAC function as extra data, to be fed into the HMAC after
			// computing the digest. This makes the MAC roughly constant time as
			// long as the digest computation is constant time and does not
			// affect the subsequent write, modulo cache effects.
			paddingLen, paddingGood = extractPadding(payload)
		default:
			panic("unknown cipher type")
		}

		if hc.version == VersionTLS13 {
			if typ != recordTypeApplicationData {
				return nil, 0, alertUnexpectedMessage
			}
			if len(plaintext) > maxPlaintext+1 {
				return nil, 0, alertRecordOverflow
			}
			// Remove padding and find the ContentType scanning from the end.
			for i := len(plaintext) - 1; i >= 0; i-- {
				if plaintext[i] != 0 {
					typ = recordType(plaintext[i])
					plaintext = plaintext[:i]
					break
				}
				if i == 0 {
					return nil, 0, alertUnexpectedMessage
				}
			}
		}
	} else {
		plaintext = payload
	}

	if hc.mac != nil {
		macSize := hc.mac.Size()
		if len(payload) < macSize {
			return nil, 0, alertBadRecordMAC
		}

		n := len(payload) - macSize - paddingLen
		n = subtle.ConstantTimeSelect(int(uint32(n)>>31), 0, n) // if n < 0 { n = 0 }
		record[3] = byte(n >> 8)
		record[4] = byte(n)
		remoteMAC := payload[n : n+macSize]
		localMAC := tls10MAC(hc.mac, hc.scratchBuf[:0], hc.seq[:], record[:recordHeaderLen], payload[:n], payload[n+macSize:])

		// This is equivalent to checking the MACs and paddingGood
		// separately, but In constant-time to prevent distinguishing
		// padding failures from MAC failures. Depending on what value
		// of paddingLen was returned on bad padding, distinguishing
		// bad MAC from bad padding can lead to an attack.
		//
		// See also the logic at the end of extractPadding.
		macAndPaddingGood := subtle.ConstantTimeCompare(localMAC, remoteMAC) & int(paddingGood)
		if macAndPaddingGood != 1 {
			return nil, 0, alertBadRecordMAC
		}

		plaintext = payload[:n]
	}

	hc.incSeq()
	return plaintext, typ, nil
}

// sliceForAppend extends the input slice by n bytes. head is the full extended
// slice, while tail is the appended part. If the original slice has sufficient
// capacity no allocation is performed.
func sliceForAppend(in []byte, n int) (head, tail []byte) {
	if total := len(in) + n; cap(in) >= total {
		head = in[:total]
	} else {
		head = make([]byte, total)
		copy(head, in)
	}
	tail = head[len(in):]
	return
}

// encrypt encrypts payload, adding the appropriate nonce and/or MAC, and
// appends it to record, which must already contain the record header.
func (hc *halfConn) encrypt(record, payload []byte, rand io.Reader) ([]byte, error) {
	if hc.cipher == nil {
		return append(record, payload...), nil
	}

	var explicitNonce []byte
	if explicitNonceLen := hc.explicitNonceLen(); explicitNonceLen > 0 {
		record, explicitNonce = sliceForAppend(record, explicitNonceLen)
		if _, isCBC := hc.cipher.(cbcMode); !isCBC && explicitNonceLen < 16 {
			copy(explicitNonce, hc.seq[:])
		} else {
			if _, err := io.ReadFull(rand, explicitNonce); err != nil {
				return nil, err
			}
		}
	}

	var dst []byte
	switch c := hc.cipher.(type) {
	case cipher.Stream:
		mac := tls10MAC(hc.mac, hc.scratchBuf[:0], hc.seq[:], record[:recordHeaderLen], payload, nil)
		record, dst = sliceForAppend(record, len(payload)+len(mac))
		c.XORKeyStream(dst[:len(payload)], payload)
		c.XORKeyStream(dst[len(payload):], mac)
	case aead:
		nonce := explicitNonce
		if len(nonce) == 0 {
			nonce = hc.seq[:]
		}

		if hc.version == VersionTLS13 {
			record = append(record, payload...)

			// Encrypt the actual ContentType and replace the plaintext one.
			record = append(record, record[0])
			record[0] = byte(recordTypeApplicationData)

			n := len(payload) + 1 + c.Overhead()
			record[3] = byte(n >> 8)
			record[4] = byte(n)

			record = c.Seal(record[:recordHeaderLen],
				nonce, record[recordHeaderLen:], record[:recordHeaderLen])
		} else {
			additionalData := append(hc.scratchBuf[:0], hc.seq[:]...)
			additionalData = append(additionalData, record[:recordHeaderLen]...)
			record = c.Seal(record, nonce, payload, additionalData)
		}
	case cbcMode:
		mac := tls10MAC(hc.mac, hc.scratchBuf[:0], hc.seq[:], record[:recordHeaderLen], payload, nil)
		blockSize := c.BlockSize()
		plaintextLen := len(payload) + len(mac)
		paddingLen := blockSize - plaintextLen%blockSize
		record, dst = sliceForAppend(record, plaintextLen+paddingLen)
		copy(dst, payload)
		copy(dst[len(payload):], mac)
		for i := plaintextLen; i < len(dst); i++ {
			dst[i] = byte(paddingLen - 1)
		}
		if len(explicitNonce) > 0 {
			c.SetIV(explicitNonce)
		}
		c.CryptBlocks(dst, dst)
	default:
		panic("unknown cipher type")
	}

	// Update length to include nonce, MAC and any block padding needed.
	n := len(record) - recordHeaderLen
	record[3] = byte(n >> 8)
	record[4] = byte(n)
	hc.incSeq()

	return record, nil
}

// RecordHeaderError is returned when a TLS record header is invalid.
type RecordHeaderError struct {
	// Msg contains a human readable string that describes the error.
	Msg string
	// RecordHeader contains the five bytes of TLS record header that
	// triggered the error.
	RecordHeader [5]byte
	// Conn provides the underlying net.Conn In the case that a client
	// sent an initial handshake that didn't look like TLS.
	// It is nil if there's already been a handshake or a TLS alert has
	// been written to the connection.
	Conn net.Conn
}

func (e RecordHeaderError) Error() string { return "tls: " + e.Msg }

func (tpc *TLSPassThroughConnection) newRecordHeaderError(conn net.Conn, msg string) (err RecordHeaderError) {
	err.Msg = msg
	err.Conn = conn
	copy(err.RecordHeader[:], tpc.rawInput.Bytes())
	return err
}

func (tpc *TLSPassThroughConnection) readRecord() (bool, error) {
	return tpc.readRecordOrCCS()
}

// readRecordOrCCS reads one or more TLS records from the connection and
// updates the record layer state. Some invariants:
//   - tpc.In must be locked
//   - tpc.input must be empty
//
// During the handshake one and only one of the following will happen:
//   - tpc.hand grows
//   - tpc.In.changeCipherSpec is called
//   - an error is returned
//
// After the handshake one and only one of the following will happen:
//   - tpc.hand grows
//   - tpc.input is set
//   - an error is returned
func (tpc *TLSPassThroughConnection) readRecordOrCCS() (bool, error) {
	if tpc.In.err != nil {
		return false, tpc.In.err
	}

	// This function modifies tpc.rawInput, which owns the tpc.input memory.
	if tpc.input.Len() != 0 {
		return false, tpc.In.setErrorLocked(errors.New("tls: internal error: attempted to read record with pending application data"))
	}
	tpc.input.Reset(nil)

	// Read header, payload.
	if err := tpc.readFromUntil(tpc.conn, recordHeaderLen); err != nil {
		// RFC 8446, Section 6.1 suggests that EOF without an alertCloseNotify
		// is an error, but popular web sites seem to do this, so we accept it
		// if and only if at the record boundary.
		if err == io.ErrUnexpectedEOF && tpc.rawInput.Len() == 0 {
			err = io.EOF
		}
		if e, ok := err.(net.Error); !ok || !e.Temporary() {
			tpc.In.setErrorLocked(err)
		}
		return false, err
	}
	hdr := tpc.rawInput.Bytes()[:recordHeaderLen]
	typ := recordType(hdr[0])

	vers := uint16(hdr[1])<<8 | uint16(hdr[2])
	expectedVers := tpc.Vers
	if expectedVers == VersionTLS13 {
		// All TLS 1.3 records are expected to have 0x0303 (1.2) after
		// the initial hello (RFC 8446 Section 5.1).
		expectedVers = VersionTLS12
	}
	n := int(hdr[3])<<8 | int(hdr[4])
	if tpc.haveVers && vers != expectedVers {
		tpc.sendAlert(alertProtocolVersion)
		msg := fmt.Sprintf("received record with version %x when expecting version %x", vers, expectedVers)
		return false, tpc.In.setErrorLocked(tpc.newRecordHeaderError(nil, msg))
	}
	if tpc.Vers == VersionTLS13 && n > maxCiphertextTLS13 || n > maxCiphertext {
		tpc.sendAlert(alertRecordOverflow)
		msg := fmt.Sprintf("oversized record received with length %d", n)
		return false, tpc.In.setErrorLocked(tpc.newRecordHeaderError(nil, msg))
	}
	if err := tpc.readFromUntil(tpc.conn, recordHeaderLen+n); err != nil {
		if e, ok := err.(net.Error); !ok || !e.Temporary() {
			tpc.In.setErrorLocked(err)
		}
		return false, err
	}

	// Process message.
	record := tpc.rawInput.Next(recordHeaderLen + n)
	data, typ, err := tpc.In.decrypt(record)
	if err != nil {
		return false, tpc.In.setErrorLocked(tpc.sendAlert(err.(alert)))
	}
	if len(data) > maxPlaintext {
		return false, tpc.In.setErrorLocked(tpc.sendAlert(alertRecordOverflow))
	}

	// Application Data messages are always protected.
	if tpc.In.cipher == nil && typ == recordTypeApplicationData {
		return false, tpc.In.setErrorLocked(tpc.sendAlert(alertUnexpectedMessage))
	}

	if typ != recordTypeAlert && typ != recordTypeChangeCipherSpec && len(data) > 0 {
		// This is a state-advancing message: reset the retry count.
		tpc.retryCount = 0
	}

	switch typ {
	default:
		return false, tpc.In.setErrorLocked(tpc.sendAlert(alertUnexpectedMessage))

	case recordTypeAlert:
		if len(data) != 2 {
			return false, tpc.In.setErrorLocked(tpc.sendAlert(alertUnexpectedMessage))
		}
		if alert(data[1]) == alertCloseNotify {
			return false, tpc.In.setErrorLocked(io.EOF)
		}
		if tpc.Vers == VersionTLS13 {
			return false, tpc.In.setErrorLocked(&net.OpError{Op: "remote error", Err: alert(data[1])})
		}
		switch data[0] {
		case alertLevelWarning:
			// Drop the record on the floor and retry.
			return tpc.retryReadRecord(false)
		case alertLevelError:
			return false, tpc.In.setErrorLocked(&net.OpError{Op: "remote error", Err: alert(data[1])})
		default:
			return false, tpc.In.setErrorLocked(tpc.sendAlert(alertUnexpectedMessage))
		}

	case recordTypeChangeCipherSpec:
		if !tpc.isHandshakeComplete.Load() {
			tpc.isHandshakeComplete.Store(true)
		}
		if len(data) != 1 || data[0] != 1 {
			return true, tpc.In.setErrorLocked(tpc.sendAlert(alertDecodeError))
		}
		return true, nil

	case recordTypeApplicationData:
		if !tpc.isHandshakeComplete.Load() {
			tpc.isHandshakeComplete.Store(true)
		}
		// Some OpenSSL servers send empty records In order to randomize the
		// CBC IV. Ignore a limited number of empty records.
		if len(data) == 0 {
			return tpc.retryReadRecord(false)
		}
		// Note that data is owned by tpc.rawInput, following the Next call above,
		// to avoid copying the plaintext. This is safe because tpc.rawInput is
		// not read from or written to until tpc.input is drained.
		tpc.input.Reset(data)
	case recordTypeHandshake:
		if len(data) == 0 {
			return false, tpc.In.setErrorLocked(tpc.sendAlert(alertUnexpectedMessage))
		}
		tpc.hand.Write(data)
	}

	return false, nil
}

// retryReadRecord recurs into readRecordOrCCS to drop a non-advancing record, like
// a warning alert, empty application_data, or a change_cipher_spec In TLS 1.3.
func (tpc *TLSPassThroughConnection) retryReadRecord(readChangeCipherSpec bool) (bool, error) {
	tpc.retryCount++
	if tpc.retryCount > maxUselessRecords {
		tpc.sendAlert(alertUnexpectedMessage)
		return readChangeCipherSpec, tpc.In.setErrorLocked(errors.New("tls: too many ignored records"))
	}
	return tpc.readRecordOrCCS()
}

// atLeastReader reads from R, stopping with EOF once at least N bytes have been
// read. It is different from an io.LimitedReader In that it doesn't cut short
// the last Read call, and In that it considers an early EOF an error.
type atLeastReader struct {
	R io.Reader
	N int64
}

func (r *atLeastReader) Read(p []byte) (int, error) {
	if r.N <= 0 {
		return 0, io.EOF
	}
	n, err := r.R.Read(p)
	r.N -= int64(n) // won't underflow unless len(p) >= n > 9223372036854775809
	if r.N > 0 && err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	if r.N <= 0 && err == nil {
		return n, io.EOF
	}
	return n, err
}

// readFromUntil reads from r into tpc.rawInput until tpc.rawInput contains
// at least n bytes or else returns an error.
func (tpc *TLSPassThroughConnection) readFromUntil(r io.Reader, n int) error {
	if tpc.rawInput.Len() >= n {
		return nil
	}
	needs := n - tpc.rawInput.Len()
	// There might be extra input waiting on the wire. Make a best effort
	// attempt to fetch it so that it can be used In (*Conn).Read to
	// "predict" closeNotify alerts.
	tpc.rawInput.Grow(needs + bytes.MinRead)
	_, err := tpc.rawInput.ReadFrom(&atLeastReader{r, int64(needs)})
	return err
}

// sendAlertLocked sends a TLS alert message.
func (tpc *TLSPassThroughConnection) sendAlertLocked(err alert) error {
	switch err {
	case alertNoRenegotiation, alertCloseNotify:
		tpc.tmp[0] = alertLevelWarning
	default:
		tpc.tmp[0] = alertLevelError
	}
	tpc.tmp[1] = byte(err)

	_, writeErr := tpc.writeRecordLocked(recordTypeAlert, tpc.tmp[0:2])
	if err == alertCloseNotify {
		// closeNotify is a special case In that it isn't an error.
		return writeErr
	}

	return tpc.Out.setErrorLocked(&net.OpError{Op: "local error", Err: err})
}

// sendAlert sends a TLS alert message.
func (tpc *TLSPassThroughConnection) sendAlert(err alert) error {
	tpc.Out.Lock()
	defer tpc.Out.Unlock()
	return tpc.sendAlertLocked(err)
}

const (
	// tcpMSSEstimate is a conservative estimate of the TCP maximum segment
	// size (MSS). A constant is used, rather than querying the kernel for
	// the actual MSS, to avoid complexity. The value here is the IPv6
	// minimum MTU (1280 bytes) minus the overhead of an IPv6 header (40
	// bytes) and a TCP header with timestamps (32 bytes).
	tcpMSSEstimate = 1208

	// recordSizeBoostThreshold is the number of bytes of application data
	// sent after which the TLS record size will be increased to the
	// maximum.
	recordSizeBoostThreshold = 128 * 1024
)

// maxPayloadSizeForWrite returns the maximum TLS payload size to use for the
// next application data record. There is the following trade-off:
//
//   - For latency-sensitive applications, such as web browsing, each TLS
//     record should fit In one TCP segment.
//   - For throughput-sensitive applications, such as large file transfers,
//     larger TLS records better amortize framing and encryption overheads.
//
// A simple heuristic that works well In practice is to use small records for
// the first 1MB of data, then use larger records for subsequent data, and
// reset back to smaller records after the connection becomes idle. See "High
// Performance Web Networking", Chapter 4, or:
// https://www.igvita.com/2013/10/24/optimizing-tls-record-size-and-buffering-latency/
//
// In the interests of simplicity and determinism, this code does not attempt
// to reset the record size once the connection is idle, however.
func (tpc *TLSPassThroughConnection) maxPayloadSizeForWrite(typ recordType) int {
	if tpc.bytesSent >= recordSizeBoostThreshold {
		return maxPlaintext
	}

	// Subtract TLS overheads to get the maximum payload size.
	payloadBytes := tcpMSSEstimate - recordHeaderLen - tpc.Out.explicitNonceLen()
	if tpc.Out.cipher != nil {
		switch ciph := tpc.Out.cipher.(type) {
		case cipher.Stream:
			payloadBytes -= tpc.Out.mac.Size()
		case cipher.AEAD:
			payloadBytes -= ciph.Overhead()
		case cbcMode:
			blockSize := ciph.BlockSize()
			// The payload must fit In a multiple of blockSize, with
			// room for at least one padding byte.
			payloadBytes = (payloadBytes & ^(blockSize - 1)) - 1
			// The MAC is appended before padding so affects the
			// payload size directly.
			payloadBytes -= tpc.Out.mac.Size()
		default:
			panic("unknown cipher type")
		}
	}
	if tpc.Vers == VersionTLS13 {
		payloadBytes-- // encrypted ContentType
	}

	// Allow packet growth In arithmetic progression up to max.
	pkt := tpc.packetsSent
	tpc.packetsSent++
	if pkt > 1000 {
		return maxPlaintext // avoid overflow In multiply below
	}

	n := payloadBytes * int(pkt+1)
	if n > maxPlaintext {
		n = maxPlaintext
	}
	return n
}

func (tpc *TLSPassThroughConnection) write(data []byte) (int, error) {
	if tpc.buffering {
		tpc.sendBuf = append(tpc.sendBuf, data...)
		return len(data), nil
	}

	n, err := tpc.conn.Write(data)

	tpc.bytesSent += int64(n)
	return n, err
}

func (tpc *TLSPassThroughConnection) flush() (int, error) {
	if len(tpc.sendBuf) == 0 {
		return 0, nil
	}

	n, err := tpc.conn.Write(tpc.sendBuf)
	tpc.bytesSent += int64(n)
	tpc.sendBuf = nil
	tpc.buffering = false
	return n, err
}

// outBufPool pools the record-sized scratch buffers used by writeRecordLocked.
var outBufPool = sync.Pool{
	New: func() any {
		return new([]byte)
	},
}

// writeRecordLocked writes a TLS record with the given type and payload to the
// connection and updates the record layer state.
func (tpc *TLSPassThroughConnection) writeRecordLocked(typ recordType, data []byte) (int, error) {
	outBufPtr := outBufPool.Get().(*[]byte)
	outBuf := *outBufPtr
	defer func() {
		// You might be tempted to simplify this by just passing &outBuf to Put,
		// but that would make the local copy of the outBuf slice header escape
		// to the heap, causing an allocation. Instead, we keep around the
		// pointer to the slice header returned by Get, which is already on the
		// heap, and overwrite and return that.
		*outBufPtr = outBuf
		outBufPool.Put(outBufPtr)
	}()

	var n int
	for len(data) > 0 {
		m := len(data)
		if maxPayload := tpc.maxPayloadSizeForWrite(typ); m > maxPayload {
			m = maxPayload
		}

		_, outBuf = sliceForAppend(outBuf[:0], recordHeaderLen)
		outBuf[0] = byte(typ)
		vers := tpc.Vers
		if vers == 0 {
			// Some TLS servers fail if the record version is
			// greater than TLS 1.0 for the initial ClientHello.
			vers = VersionTLS10
		} else if vers == VersionTLS13 {
			// TLS 1.3 froze the record layer version to 1.2.
			// See RFC 8446, Section 5.1.
			vers = VersionTLS12
		}
		outBuf[1] = byte(vers >> 8)
		outBuf[2] = byte(vers)
		outBuf[3] = byte(m >> 8)
		outBuf[4] = byte(m)

		var err error
		outBuf, err = tpc.Out.encrypt(outBuf, data[:m], rand.Reader)
		if err != nil {
			return n, err
		}
		if _, err := tpc.write(outBuf); err != nil {
			return n, err
		}
		n += m
		data = data[m:]
	}

	if typ == recordTypeChangeCipherSpec && tpc.Vers != VersionTLS13 {
		if err := tpc.Out.changeCipherSpec(); err != nil {
			return n, tpc.sendAlertLocked(err.(alert))
		}
	}

	return n, nil
}

// writeHandshakeRecord writes a handshake message to the connection and updates
// the record layer state. If transcript is non-nil the marshalled message is
// written to it.
func (tpc *TLSPassThroughConnection) writeHandshakeRecord(msg handshakeMessage) (int, error) {
	tpc.Out.Lock()
	defer tpc.Out.Unlock()

	data, err := msg.marshal()
	if err != nil {
		return 0, err
	}

	return tpc.writeRecordLocked(recordTypeHandshake, data)
}

// writeChangeCipherRecord writes a ChangeCipherSpec message to the connection and
// updates the record layer state.
func (tpc *TLSPassThroughConnection) writeChangeCipherRecord() error {
	tpc.Out.Lock()
	defer tpc.Out.Unlock()
	_, err := tpc.writeRecordLocked(recordTypeChangeCipherSpec, []byte{1})
	return err
}

// readHandshakeBytes reads handshake data until c.hand contains at least n bytes.
func (tpc *TLSPassThroughConnection) readHandshakeBytes(n int) error {
	for tpc.hand.Len() < n {
		if _, err := tpc.readRecord(); err != nil {
			return err
		}
	}
	return nil
}

// readHandshake reads the next handshake message from
// the record layer. If transcript is non-nil, the message
// is written to the passed transcriptHash.
func (tpc *TLSPassThroughConnection) readHandshake() (any, error) {
	if err := tpc.readHandshakeBytes(4); err != nil {
		return nil, err
	}
	data := tpc.hand.Bytes()
	n := int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	if n > maxHandshake {
		tpc.sendAlertLocked(alertInternalError)
		return nil, tpc.In.setErrorLocked(fmt.Errorf("tls: handshake message of length %d bytes exceeds maximum of %d bytes", n, maxHandshake))
	}
	if err := tpc.readHandshakeBytes(4 + n); err != nil {
		return nil, err
	}
	data = tpc.hand.Next(4 + n)
	return tpc.unmarshalHandshakeMessage(data)
}

func (tpc *TLSPassThroughConnection) unmarshalHandshakeMessage(data []byte) (handshakeMessage, error) {
	var m handshakeMessage
	switch data[0] {
	case typeHelloRequest:
		m = new(helloRequestMsg)
	case typeClientHello:
		m = new(clientHelloMsg)
	case typeServerHello:
		m = new(serverHelloMsg)
	case typeNewSessionTicket:
		if tpc.Vers == VersionTLS13 {
			m = new(newSessionTicketMsgTLS13)
		} else {
			m = new(newSessionTicketMsg)
		}
	case typeCertificate:
		if tpc.Vers == VersionTLS13 {
			m = new(certificateMsgTLS13)
		} else {
			m = new(certificateMsg)
		}
	case typeCertificateRequest:
		if tpc.Vers == VersionTLS13 {
			m = new(certificateRequestMsgTLS13)
		} else {
			m = &certificateRequestMsg{
				hasSignatureAlgorithm: tpc.Vers >= VersionTLS12,
			}
		}
	case typeCertificateStatus:
		m = new(certificateStatusMsg)
	case typeServerKeyExchange:
		m = new(serverKeyExchangeMsg)
	case typeServerHelloDone:
		m = new(serverHelloDoneMsg)
	case typeClientKeyExchange:
		m = new(clientKeyExchangeMsg)
	case typeCertificateVerify:
		m = &certificateVerifyMsg{
			hasSignatureAlgorithm: tpc.Vers >= VersionTLS12,
		}
	case typeFinished:
		m = new(finishedMsg)
	case typeEncryptedExtensions:
		m = new(encryptedExtensionsMsg)
	case typeEndOfEarlyData:
		m = new(endOfEarlyDataMsg)
	case typeKeyUpdate:
		m = new(keyUpdateMsg)
	default:
		return nil, tpc.In.setErrorLocked(tpc.sendAlert(alertUnexpectedMessage))
	}

	// The handshake message unmarshalers
	// expect to be able to keep references to data,
	// so pass In a fresh copy that won't be overwritten.
	data = append([]byte(nil), data...)

	if !m.unmarshal(data) {
		return nil, tpc.In.setErrorLocked(tpc.sendAlert(alertUnexpectedMessage))
	}

	return m, nil
}

// Write writes data to the connection.
//
// As Write calls [Conn.Handshake], In order to prevent indefinite blocking a deadline
// must be set for both [Conn.Read] and Write before Write is called when the handshake
// has not yet completed. See [Conn.SetDeadline], [Conn.SetReadDeadline], and
// [Conn.SetWriteDeadline].
func (tpc *TLSPassThroughConnection) Write(b []byte) (int, error) {
	// interlock with Close below
	for {
		x := tpc.activeCall.Load()
		if x&1 != 0 {
			return 0, net.ErrClosed
		}
		if tpc.activeCall.CompareAndSwap(x, x+2) {
			break
		}
	}
	defer tpc.activeCall.Add(-2)

	tpc.Out.Lock()
	defer tpc.Out.Unlock()

	if err := tpc.Out.err; err != nil {
		return 0, err
	}

	if !tpc.isHandshakeComplete.Load() {
		return 0, alertInternalError
	}

	// TLS 1.0 is susceptible to a chosen-plaintext
	// attack when using block mode ciphers due to predictable IVs.
	// This can be prevented by splitting each Application Data
	// record into two records, effectively randomizing the IV.
	//
	// https://www.openssl.org/~bodo/tls-cbc.txt
	// https://bugzilla.mozilla.org/show_bug.cgi?id=665814
	// https://www.imperialviolet.org/2012/01/15/beastfollowup.html

	var m int
	if len(b) > 1 && tpc.Vers == VersionTLS10 {
		if _, ok := tpc.Out.cipher.(cipher.BlockMode); ok {
			n, err := tpc.writeRecordLocked(recordTypeApplicationData, b[:1])
			if err != nil {
				return n, tpc.Out.setErrorLocked(err)
			}
			m, b = 1, b[1:]
		}
	}
	n, err := tpc.writeRecordLocked(recordTypeApplicationData, b)
	return n + m, tpc.Out.setErrorLocked(err)
}

func (tpc *TLSPassThroughConnection) handleKeyUpdate(keyUpdate *keyUpdateMsg) error {
	cipherSuite := CipherSuiteTLS13ByID(tpc.CipherSuite)
	if cipherSuite == nil {
		return tpc.In.setErrorLocked(tpc.sendAlert(alertInternalError))
	}

	newSecret := cipherSuite.nextTrafficSecret(tpc.In.trafficSecret)
	tpc.In.SetTrafficSecret(cipherSuite, newSecret)

	if keyUpdate.updateRequested {
		tpc.Out.Lock()
		defer tpc.Out.Unlock()

		msg := &keyUpdateMsg{}
		msgBytes, err := msg.marshal()
		if err != nil {
			return err
		}
		_, err = tpc.writeRecordLocked(recordTypeHandshake, msgBytes)
		if err != nil {
			// Surface the error at the next write.
			tpc.Out.setErrorLocked(err)
			return nil
		}

		newSecret := cipherSuite.nextTrafficSecret(tpc.Out.trafficSecret)
		tpc.Out.SetTrafficSecret(cipherSuite, newSecret)
	}

	return nil
}

// Read reads data from the connection.
//
// As Read calls [Conn.Handshake], In order to prevent indefinite blocking a deadline
// must be set for both Read and [Conn.Write] before Read is called when the handshake
// has not yet completed. See [Conn.SetDeadline], [Conn.SetReadDeadline], and
// [Conn.SetWriteDeadline].
func (tpc *TLSPassThroughConnection) Read(b []byte) (int, error) {
	if len(b) == 0 {
		// Put this after Handshake, In case people were calling
		// Read(nil) for the side effect of the Handshake.
		return 0, nil
	}

	tpc.In.Lock()
	defer tpc.In.Unlock()

	for tpc.input.Len() == 0 {
		if _, err := tpc.readRecord(); err != nil {
			return 0, err
		}
	}

	n, _ := tpc.input.Read(b)

	// If a close-notify alert is waiting, read it so that we can return (n,
	// EOF) instead of (n, nil), to signal to the HTTP response reading
	// goroutine that the connection is now closed. This eliminates a race
	// where the HTTP response reading goroutine would otherwise not observe
	// the EOF until its next read, by which time a client goroutine might
	// have already tried to reuse the HTTP connection for a new request.
	// See https://golang.org/cl/76400046 and https://golang.org/issue/3514
	if n != 0 && tpc.input.Len() == 0 && tpc.rawInput.Len() > 0 &&
		recordType(tpc.rawInput.Bytes()[0]) == recordTypeAlert {
		if _, err := tpc.readRecord(); err != nil {
			return n, err // will be io.EOF on closeNotify
		}
	}

	return n, nil
}

// Close closes the connection.
func (tpc *TLSPassThroughConnection) Close() error {
	// Interlock with Conn.Write above.
	var x int32
	for {
		x = tpc.activeCall.Load()
		if x&1 != 0 {
			return net.ErrClosed
		}
		if tpc.activeCall.CompareAndSwap(x, x|1) {
			break
		}
	}
	if x != 0 {
		// io.Writer and io.Closer should not be used concurrently.
		// If Close is called while a Write is currently In-flight,
		// interpret that as a sign that this Close is really just
		// being used to break the Write and/or clean up resources and
		// avoid sending the alertCloseNotify, which may block
		// waiting on handshakeMutex or the c.Out mutex.
		return tpc.conn.Close()
	}

	if err := tpc.conn.Close(); err != nil {
		return err
	}
	return nil
}
