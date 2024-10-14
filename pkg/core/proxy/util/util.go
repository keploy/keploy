//go:build linux

// Package util provides utility functions for the proxy package.
package util

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"go.keploy.io/server/v2/pkg/models"
	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/utils"

	"go.uber.org/zap"

	// "math/rand"
	"net"
	"strconv"
	"strings"
)

var Emoji = "\U0001F430" + " Keploy:"

// idCounter is used to generate random ID for each request
var idCounter int64 = -1

func GetNextID() int64 {
	return atomic.AddInt64(&idCounter, 1)
}

// Conn is helpful for multiple reads from the same connection
type Conn struct {
	net.Conn
	Reader io.Reader
	Logger *zap.Logger
	mu     sync.Mutex
}

func (c *Conn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(p) == 0 {
		c.Logger.Debug("the length is 0 for the reading from customConn")
	}
	return c.Reader.Read(p)
}

type Peer string

// Peer constants
const (
	Client      Peer = "client"
	Destination Peer = "destination"
)

// ReadBuffConn is used to read the buffer from the connection
func ReadBuffConn(ctx context.Context, logger *zap.Logger, conn net.Conn, bufferChannel chan []byte, errChannel chan error) {
	//TODO: where to close the errChannel
	for {
		select {
		case <-ctx.Done():
			// errChannel <- ctx.Err()
			return
		default:
			if conn == nil {
				logger.Debug("the conn is nil")
			}
			buffer, err := ReadBytes(ctx, logger, conn)
			if err != nil {
				if ctx.Err() != nil { // to avoid sending buffer to closed channel if the context is cancelled
					return
				}
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read the packet message in proxy")
				}
				errChannel <- err
				return
			}
			if ctx.Err() != nil { // to avoid sending buffer to closed channel if the context is cancelled
				return
			}
			bufferChannel <- buffer
		}
	}
}

func ReadInitialBuf(ctx context.Context, logger *zap.Logger, conn net.Conn) ([]byte, error) {
	readErr := errors.New("failed to read the initial request buffer")

	initialBuf, err := ReadBytes(ctx, logger, conn)
	if err != nil && err != io.EOF {
		utils.LogError(logger, err, "failed to read the request message in proxy")
		return nil, readErr
	}

	if err == io.EOF && len(initialBuf) == 0 {
		logger.Debug("received EOF, closing conn", zap.Error(err))
		return nil, readErr
	}

	logger.Debug("received initial buffer", zap.Any("size", len(initialBuf)), zap.Any("initial buffer", initialBuf))
	if err != nil {
		utils.LogError(logger, err, "failed to read the request message in proxy")
		return nil, readErr
	}
	return initialBuf, nil
}

// ReadBytes function is utilized to read the complete message from the reader until the end of the file (EOF).
// It returns the content as a byte array.
func ReadBytes(ctx context.Context, logger *zap.Logger, reader io.Reader) ([]byte, error) {
	var buffer []byte
	const maxEmptyReads = 5
	emptyReads := 0

	// Channel to communicate read results
	readResult := make(chan struct {
		n   int
		err error
		buf []byte
	})

	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		err := g.Wait()
		if err != nil {
			utils.LogError(logger, err, "failed to read the request message in proxy")
		}
		close(readResult)
	}()

	for {
		// Start a goroutine to perform the read operation
		g.Go(func() error {
			defer Recover(logger, nil, nil)
			buf := make([]byte, 1024)
			n, err := reader.Read(buf)
			if ctx.Err() != nil {
				return nil
			}
			readResult <- struct {
				n   int
				err error
				buf []byte
			}{n, err, buf}
			return nil
		})

		// Use a select statement to wait for either the read result or context cancellation
		select {
		case <-ctx.Done():
			return buffer, ctx.Err()
		case result := <-readResult:
			if result.n > 0 {
				buffer = append(buffer, result.buf[:result.n]...)
				emptyReads = 0 // Reset the counter because we got some data
			}

			if result.err != nil {
				if result.err == io.EOF {
					emptyReads++
					if emptyReads >= maxEmptyReads {
						return buffer, result.err // Multiple EOFs in a row, probably a true EOF
					}
					time.Sleep(time.Millisecond * 100) // Sleep before trying again
					continue
				}
				return buffer, result.err
			}
			if result.n < len(result.buf) {
				return buffer, nil
			}
		}
	}
}

// ReadRequiredBytes ReadBytes function is utilized to read the required number of bytes from the reader.
// It returns the content as a byte array.
func ReadRequiredBytes(ctx context.Context, logger *zap.Logger, reader io.Reader, numBytes int) ([]byte, error) {
	var buffer []byte
	const maxEmptyReads = 5
	emptyReads := 0

	// Channel to communicate read results
	readResult := make(chan struct {
		n   int
		err error
		buf []byte
	})

	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		err := g.Wait()
		if err != nil {
			utils.LogError(logger, err, "failed to read the request message in proxy")
		}
		close(readResult)
	}()

	for numBytes > 0 {
		// Start a goroutine to perform the read operation
		g.Go(func() error {
			defer Recover(logger, nil, nil)
			buf := make([]byte, numBytes)
			n, err := reader.Read(buf)
			if ctx.Err() != nil {
				return nil
			}
			readResult <- struct {
				n   int
				err error
				buf []byte
			}{n, err, buf}
			return nil
		})

		// Use a select statement to wait for either the read result or context cancellation with timeout
		select {
		case <-ctx.Done():
			return buffer, ctx.Err()
		// case <-time.After(5 * time.Second):
		// 	logger.Error("timeout occurred while reading the packet")
		// 	return buffer, context.DeadlineExceeded
		case result := <-readResult:
			if result.n > 0 {
				buffer = append(buffer, result.buf[:result.n]...)
				numBytes -= result.n
				emptyReads = 0 // Reset the counter because we got some data
			}

			if result.err != nil {
				if result.err == io.EOF {
					emptyReads++
					if emptyReads >= maxEmptyReads {
						return buffer, result.err // Multiple EOFs in a row, probably a true EOF
					}
					time.Sleep(time.Millisecond * 100) // Sleep before trying again
					continue
				}
				return buffer, result.err
			}

			if result.n == numBytes {
				return buffer, nil
			}
		}
	}

	return buffer, nil
}

// ReadFromPeer function is used to read the buffer from the peer connection. The peer can be either the client or the destination.
func ReadFromPeer(ctx context.Context, logger *zap.Logger, conn net.Conn, buffChan chan []byte, errChan chan error, peer Peer) error {
	//get the error group from the context
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context while reading from peer")
	}

	var client, dest net.Conn

	if peer == Client {
		client = conn
	} else {
		dest = conn
	}

	g.Go(func() error {
		defer Recover(logger, client, dest)
		defer close(buffChan)
		ReadBuffConn(ctx, logger, conn, buffChan, errChan)
		return nil
	})

	return nil
}

// PassThrough function is used to pass the network traffic to the destination connection.
// It also closes the destination connection if the function returns an error.
func PassThrough(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, requestBuffer [][]byte) ([]byte, error) {
	logger.Debug("passing through the network traffic to the destination server", zap.Any("Destination Addr", dstCfg.Addr))
	// making destConn
	var destConn net.Conn
	var err error
	if dstCfg.TLSCfg != nil {
		logger.Debug("trying to establish a TLS connection with the destination server", zap.Any("Destination Addr", dstCfg.Addr))

		destConn, err = tls.Dial("tcp", dstCfg.Addr, dstCfg.TLSCfg)
		if err != nil {
			utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Any("server address", dstCfg.Addr))
			return nil, err
		}
		logger.Debug("TLS connection established with the destination server", zap.Any("Destination Addr", destConn.RemoteAddr().String()))
	} else {
		logger.Debug("trying to establish a connection with the destination server", zap.Any("Destination Addr", dstCfg.Addr))
		destConn, err = net.Dial("tcp", dstCfg.Addr)
		if err != nil {
			utils.LogError(logger, err, "failed to dial the destination server")
			return nil, err
		}
		logger.Debug("connection established with the destination server", zap.Any("Destination Addr", destConn.RemoteAddr().String()))
	}

	logger.Debug("trying to forward requests to target", zap.Any("Destination Addr", destConn.RemoteAddr().String()))
	for _, v := range requestBuffer {
		_, err := destConn.Write(v)
		if err != nil {
			utils.LogError(logger, err, "failed to write request message to the destination server", zap.Any("Destination Addr", destConn.RemoteAddr().String()))
			return nil, err
		}
	}

	// channels for writing messages from proxy to destination or client
	destBufferChannel := make(chan []byte)
	errChannel := make(chan error, 1)

	go func() {
		defer Recover(logger, clientConn, nil)
		defer close(destBufferChannel)
		defer close(errChannel)
		defer func(destConn net.Conn) {
			err := destConn.Close()
			if err != nil {
				utils.LogError(logger, err, "failed to close the destination connection")
			}
		}(destConn)

		ReadBuffConn(ctx, logger, destConn, destBufferChannel, errChannel)
	}()

	select {
	case buffer := <-destBufferChannel:
		// Write the response message to the client
		_, err := clientConn.Write(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			utils.LogError(logger, err, "failed to write response to the client")
			return nil, err
		}

		logger.Debug("the iteration for the generic response ends with responses:"+strconv.Itoa(len(buffer)), zap.Any("buffer", buffer))
	case err := <-errChannel:
		if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
			return nil, err
		}
		return nil, nil

	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return nil, nil
}

// ToIP4AddressStr converts the integer IP4 Address to the octet format
func ToIP4AddressStr(ip uint32) string {
	// convert the IP address to a 32-bit binary number
	ipBinary := fmt.Sprintf("%032b", ip)

	// divide the binary number into four 8-bit segments
	firstByte, _ := strconv.ParseUint(ipBinary[0:8], 2, 64)
	secondByte, _ := strconv.ParseUint(ipBinary[8:16], 2, 64)
	thirdByte, _ := strconv.ParseUint(ipBinary[16:24], 2, 64)
	fourthByte, _ := strconv.ParseUint(ipBinary[24:32], 2, 64)

	// concatenate the four decimal segments with a dot separator to form the dot-decimal string
	return fmt.Sprintf("%d.%d.%d.%d", firstByte, secondByte, thirdByte, fourthByte)
}

func ToIPv6AddressStr(ip [4]uint32) string {
	// construct a byte slice
	ipBytes := make([]byte, 16) // IPv6 address is 128 bits or 16 bytes long
	for i := 0; i < 4; i++ {
		// for each uint32, extract its four bytes and put them into the byte slice
		ipBytes[i*4] = byte(ip[i] >> 24)
		ipBytes[i*4+1] = byte(ip[i] >> 16)
		ipBytes[i*4+2] = byte(ip[i] >> 8)
		ipBytes[i*4+3] = byte(ip[i])
	}
	// net.IP is a byte slice, so it can be directly used to construct an IPv6 address
	ipv6Addr := net.IP(ipBytes)
	return ipv6Addr.String()
}

func GetLocalIPv4() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
				return ipNet.IP, nil
			}
		}
	}

	return nil, fmt.Errorf("no valid IP address found")
}

func ToIPV4(ip net.IP) (uint32, bool) {
	ipv4 := ip.To4()
	if ipv4 == nil {
		return 0, false // Return 0 or handle the error accordingly
	}

	return uint32(ipv4[0])<<24 | uint32(ipv4[1])<<16 | uint32(ipv4[2])<<8 | uint32(ipv4[3]), true
}

func GetLocalIPv6() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() == nil && ipNet.IP.To16() != nil {
				return ipNet.IP, nil
			}
		}
	}

	return nil, fmt.Errorf("no valid IPv6 address found")
}

func IPv6ToUint32Array(ip net.IP) ([4]uint32, error) {
	ip = ip.To16()
	if ip == nil {
		return [4]uint32{}, errors.New("invalid IPv6 address")
	}

	return [4]uint32{
		binary.BigEndian.Uint32(ip[0:4]),
		binary.BigEndian.Uint32(ip[4:8]),
		binary.BigEndian.Uint32(ip[8:12]),
		binary.BigEndian.Uint32(ip[12:16]),
	}, nil
}

func IPToDotDecimal(ip net.IP) string {
	ipStr := ip.String()
	if ip.To4() != nil {
		ipStr = ip.To4().String()
	}
	return ipStr
}

func IsDirectoryExist(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}

func IsJava(input string) bool {
	// Convert the input string and the search term "java" to lowercase for a case-insensitive comparison.
	inputLower := strings.ToLower(input)
	searchTerm := "java"
	searchTermLower := strings.ToLower(searchTerm)

	// Use strings.Contains to check if the lowercase input contains the lowercase search term.
	return strings.Contains(inputLower, searchTermLower)
}

// IsJavaInstalled checks if java is installed on the system
func IsJavaInstalled() bool {
	_, err := exec.LookPath("java")
	return err == nil
}

// GetJavaHome returns the JAVA_HOME path
func GetJavaHome(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "java", "-XshowSettings:properties", "-version")
	var out bytes.Buffer
	cmd.Stderr = &out // The output we need is printed to STDERR

	err := cmd.Run()
	if err != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
			return "", err
		}
	}

	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "java.home") {
			parts := strings.Split(line, "=")
			if len(parts) > 1 {
				return strings.TrimSpace(parts[1]), nil
			}
		}
	}

	return "", fmt.Errorf("java.home not found in command output")
}

// Recover recovers from a panic in any parser and logs the stack trace to Sentry.
// It also closes the client and destination connection.
func Recover(logger *zap.Logger, client, dest net.Conn) {
	if logger == nil {
		fmt.Println(Emoji + "Failed to recover from panic. Logger is nil.")
		return
	}

	sentry.Flush(2 * time.Second)
	if r := recover(); r != nil {
		logger.Error("Recovered from panic in parser, closing active connections")
		if client != nil {
			err := client.Close()
			if err != nil {
				// Use string matching as a last resort to check for the specific error
				if !strings.Contains(err.Error(), "use of closed network connection") {
					// Log other errors
					utils.LogError(logger, err, "failed to close the client connection")
				}
			}
		}

		if dest != nil {
			err := dest.Close()
			if err != nil {
				// Use string matching as a last resort to check for the specific error
				if !strings.Contains(err.Error(), "use of closed network connection") {
					// Log other errors
					utils.LogError(logger, err, "failed to close the destination connection")
				}
			}
		}
		utils.HandleRecovery(logger, r, "Recovered from panic")
		sentry.Flush(time.Second * 2)
	}
}
