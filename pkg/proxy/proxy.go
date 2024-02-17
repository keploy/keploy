package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/proxy/integrations/grpcparser"
	postgresparser "go.keploy.io/server/pkg/proxy/integrations/postgresParser"
	"go.keploy.io/server/utils"

	"github.com/cloudflare/cfssl/csr"
	cfsslLog "github.com/cloudflare/cfssl/log"

	"github.com/cloudflare/cfssl/helpers"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"

	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	genericparser "go.keploy.io/server/pkg/proxy/integrations/genericParser"
	"go.keploy.io/server/pkg/proxy/integrations/httpparser"
	"go.keploy.io/server/pkg/proxy/integrations/mongoparser"
	"go.keploy.io/server/pkg/proxy/integrations/mysqlparser"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

type DependencyHandler interface {
	OutgoingType(buffer []byte) bool
	ProcessOutgoing(buffer []byte, conn net.Conn, dst net.Conn, ctx context.Context)
}

var ParsersMap = make(map[string]DependencyHandler)

type ProxySet struct {
	IP4               uint32
	IP6               [4]uint32
	Port              uint32
	DnsPort           uint32
	hook              *hooks.Hook
	logger            *zap.Logger
	FilterPid         bool
	clientConnections []net.Conn
	connMutex         *sync.Mutex
	Listener          net.Listener
	UdpDnsServer      *dns.Server
	TcpDnsServer      *dns.Server
	DnsServerTimeout  time.Duration
	dockerAppCmd      bool
	PassThroughPorts  []uint
	MongoPassword     string // password to mock the mongo connection and pass the authentication requests
}

type CustomConn struct {
	net.Conn
	r      io.Reader
	logger *zap.Logger
	mu     sync.Mutex
}

func (c *CustomConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(p) == 0 {
		c.logger.Debug("the length is 0 for the reading from customConn")
	}
	return c.r.Read(p)
}

type Conn struct {
	net.Conn
	r bufio.Reader
}

func (c *Conn) Read(b []byte) (n int, err error) {
	return c.r.Read(b)
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}

func getCaPaths() ([]string, error) {
	var caPaths = []string{}
	for _, dir := range caStorePath {
		if directoryExists(dir) {
			caPaths = append(caPaths, dir)
		}
	}
	if len(caPaths) == 0 {
		return nil, fmt.Errorf("no valid CA store path found")
	}
	return caPaths, nil
}

func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

func updateCaStore() error {
	commandRun := false
	for _, cmd := range caStoreUpdateCmd {
		if commandExists(cmd) {
			commandRun = true
			_, err := exec.Command(cmd).CombinedOutput()
			if err != nil {
				return err
			}
		}
	}
	if !commandRun {
		return fmt.Errorf("no valid CA store update command found")
	}
	return nil
}

//go:embed asset/ca.crt
var caCrt []byte

//go:embed asset/ca.key
var caPKey []byte

//go:embed asset
var caFolder embed.FS

// isJavaInstalled checks if java is installed on the system
func isJavaInstalled() bool {
	_, err := exec.LookPath("java")
	return err == nil
}

// to extract ca certificate to temp
func ExtractCertToTemp() (string, error) {
	tempFile, err := os.CreateTemp("", "ca.crt")
	if err != nil {
		return "", err
	}
	defer tempFile.Close()
	// Write to the file
	_, err = tempFile.Write(caCrt)
	if err != nil {
		return "", err
	}

	// Close the file
	err = tempFile.Close()
	if err != nil {
		return "", err
	}
	return tempFile.Name(), nil
}

// JavaCAExists checks if the CA is already installed in the specified Java keystore
func JavaCAExists(alias, storepass, cacertsPath string) bool {
	cmd := exec.Command("keytool", "-list", "-keystore", cacertsPath, "-storepass", storepass, "-alias", alias)

	err := cmd.Run()

	return err == nil
}

// get jdk path from application pid using proc file system in case of running application via IDE's
func getJavaHomeFromPID(pid string) (string, error) {
	cmdlinePath := fmt.Sprintf("/proc/%s/cmdline", pid)
	file, err := os.Open(cmdlinePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanWords) // cmdline arguments are separated by NULL bytes

	if scanner.Scan() {
		javaExecPath := filepath.Dir(filepath.Dir(scanner.Text()))
		index := strings.Index(javaExecPath, "/bin/java")

		if index != -1 {
			path := javaExecPath[:index+len("/bin/java")]
			if strings.HasSuffix(path, "/bin/java") {
				jdkPath := strings.TrimSuffix(strings.TrimSpace(path), "/bin/java")
				return jdkPath, nil
			}

		}
	}
	return "", fmt.Errorf("failed to find JAVA_HOME from PID")
}

// getJavaHome returns the JAVA_HOME path
func getJavaHome() (string, error) {
	cmd := exec.Command("java", "-XshowSettings:properties", "-version")
	var out bytes.Buffer
	cmd.Stderr = &out // The output we need is printed to STDERR

	if err := cmd.Run(); err != nil {
		return "", err
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

// InstallJavaCA installs the CA in the Java keystore
func InstallJavaCA(logger *zap.Logger, caPath string, pid uint32, isJavaServe bool) {
	// check if java is installed
	if isJavaInstalled() {
		var javaHome string
		var err error
		logger.Debug("", zap.Any("isJavaServe", isJavaServe))
		if pid != 0 && isJavaServe { // in case of unit tests, we know the pid beforehand
			logger.Debug("checking java path from proc file system", zap.Any("pid", pid))
			javaHome, err = getJavaHomeFromPID(strconv.Itoa(int(pid)))
		} else {
			logger.Debug("checking java path from default java home")
			javaHome, err = getJavaHome()
		}

		if err != nil {
			logger.Error("Java detected but failed to find JAVA_HOME", zap.Error(err))
			return
		}

		// Assuming modern Java structure (without /jre/)
		cacertsPath := fmt.Sprintf("%s/lib/security/cacerts", javaHome)
		// You can modify these as per your requirements
		storePass := "changeit"
		alias := "keployCA"

		logger.Debug("", zap.Any("java_home", javaHome), zap.Any("caCertsPath", cacertsPath), zap.Any("caPath", caPath))

		if JavaCAExists(alias, storePass, cacertsPath) {
			logger.Info("Java detected and CA already exists", zap.String("path", cacertsPath))
			return
		}

		cmd := exec.Command("keytool", "-import", "-trustcacerts", "-keystore", cacertsPath, "-storepass", storePass, "-noprompt", "-alias", alias, "-file", caPath)

		cmdOutput, err := cmd.CombinedOutput()

		if err != nil {
			logger.Error("Java detected but failed to import CA", zap.Error(err), zap.String("output", string(cmdOutput)))
			return
		}

		logger.Info("Java detected and successfully imported CA", zap.String("path", cacertsPath), zap.String("output", string(cmdOutput)))
		logger.Info("Successfully imported CA", zap.Any("", cmdOutput))
	} else {
		logger.Debug("Java is not installed on the system")
	}
}

func containsJava(input string) bool {
	// Convert the input string and the search term "java" to lowercase for a case-insensitive comparison.
	inputLower := strings.ToLower(input)
	searchTerm := "java"
	searchTermLower := strings.ToLower(searchTerm)

	// Use strings.Contains to check if the lowercase input contains the lowercase search term.
	return strings.Contains(inputLower, searchTermLower)
}

func Register(parserName string, parser DependencyHandler) {
	ParsersMap[parserName] = parser
}

// BootProxy starts proxy server on the idle local port, Default:16789
func BootProxy(logger *zap.Logger, opt Option, appCmd, appContainer string, pid uint32, lang string, passThroughPorts []uint, h *hooks.Hook, ctx context.Context, delay uint64) *ProxySet {
	//Register all the parsers in the map.
	Register("grpc", grpcparser.NewGrpcParser(logger, h))
	Register("postgres", postgresparser.NewPostgresParser(logger, h))
	Register("mongo", mongoparser.NewMongoParser(logger, h, opt.MongoPassword))
	Register("http", httpparser.NewHttpParser(logger, h))
	Register("mysql", mysqlparser.NewMySqlParser(logger, h, delay))
	// assign default values if not provided
	caPaths, err := getCaPaths()
	if err != nil {
		logger.Error("Failed to find the CA store path", zap.Error(err))
	}

	for _, path := range caPaths {
		caPath := filepath.Join(path, "ca.crt")

		fs, err := os.Create(caPath)
		if err != nil {
			logger.Error("failed to create path for ca certificate", zap.Error(err), zap.Any("root store path", path))
			return nil
		}

		_, err = fs.Write(caCrt)
		if err != nil {
			logger.Error("failed to write custom ca certificate", zap.Error(err), zap.Any("root store path", path))
			return nil
		}

		//check if serve command is used by java application
		isJavaServe := containsJava(lang)

		// install CA in the java keystore if java is installed
		InstallJavaCA(logger, caPath, pid, isJavaServe)

	}

	// Update the trusted CAs store
	err = updateCaStore()
	if err != nil {
		logger.Error("Failed to update the CA store", zap.Error(err))
	}

	tempCertPath, err := ExtractCertToTemp()
	if err != nil {
		logger.Error(Emoji+"Failed to extract certificate to tmp folder: %v", zap.Any("failed to extract certificate", err))
	}

	// for node
	err = os.Setenv("NODE_EXTRA_CA_CERTS", tempCertPath)
	if err != nil {
		logger.Error(Emoji+"Failed to set environment variable NODE_EXTRA_CA_CERTS: %v", zap.Any("failed to certificate path in environment", err))
	}

	// for python
	err = os.Setenv("REQUESTS_CA_BUNDLE", tempCertPath)
	if err != nil {
		logger.Error(Emoji+"Failed to set environment variable REQUESTS_CA_BUNDLE: %v", zap.Any("failed to certificate path in environment", err))
	}

	if opt.Port == 0 {
		opt.Port = 16789
	}
	maxAttempts := 1000
	attemptsDone := 0

	if !isPortAvailable(opt.Port) {
		for i := 1024; i <= 65535 && attemptsDone < maxAttempts; i++ {
			if isPortAvailable(uint32(i)) {
				opt.Port = uint32(i)
				logger.Info("Found an available port to start proxy server", zap.Uint32("port", opt.Port))
				break
			}
			attemptsDone++
		}
	}

	if maxAttempts <= attemptsDone {
		logger.Error("Failed to find an available port to start proxy server")
		return nil
	}
	//IPv4
	localIp4, err := util.GetLocalIPv4()
	if err != nil {
		log.Fatalln(Emoji+"Failed to get the local Ip4 address", err)
	}

	proxyAddr4, ok := util.ConvertToIPV4(localIp4)
	if !ok {
		log.Fatalf(Emoji + "Failed to convert local Ip to IPV4")
	}

	//IPv6
	proxyAddr6 := [4]uint32{0000, 0000, 0000, 0001}

	//check if the user application is running inside docker container
	dCmd, _ := util.IsDockerRelatedCommand(appCmd)
	//check if the user application is running docker container using IDE
	dIDE := (appCmd == "" && len(appContainer) != 0)

	var proxySet = ProxySet{
		Port:              opt.Port,
		IP4:               proxyAddr4,
		IP6:               proxyAddr6,
		logger:            logger,
		clientConnections: []net.Conn{},
		connMutex:         &sync.Mutex{},
		dockerAppCmd:      (dCmd || dIDE),
		PassThroughPorts:  passThroughPorts,
		hook:              h,
		MongoPassword:     opt.MongoPassword,
	}

	//setting the proxy port field in hook
	proxySet.hook.SetProxyPort(opt.Port)

	if isPortAvailable(opt.Port) {
		go func() {
			defer h.Recover(pkg.GenerateRandomID())
			defer utils.HandlePanic()
			proxySet.startProxy(ctx)
		}()

		//TODO: Need to make it configurable
		proxySet.DnsPort = 26789

		// Start the TCP DNS server
		proxySet.logger.Debug("Running Tcp Dns Server for handling Dns queries over TCP")
		go func() {
			defer h.Recover(pkg.GenerateRandomID())
			defer utils.HandlePanic()
			proxySet.startTcpDnsServer()
		}()

		// Resolve DNS queries only in case of test mode.
		if models.GetMode() == models.MODE_TEST {
			proxySet.logger.Debug("Running Udp Dns Server in Test mode...")
			proxySet.logger.Info("Keploy has hijacked the DNS resolution mechanism, your application may misbehave in keploy test mode if you have provided wrong domain name in your application code.")
			go func() {
				defer h.Recover(pkg.GenerateRandomID())
				defer utils.HandlePanic()
				proxySet.startUdpDnsServer()
			}()
		}
	} else {
		// TODO: Release eBPF resources if failed abruptly
		log.Fatalf(Emoji+"Failed to start Proxy at [Port:%v]: %v", opt.Port, err)
	}

	proxySet.logger.Debug(fmt.Sprintf("Proxy IPv4:Port %v:%v", proxySet.IP4, proxySet.Port))
	proxySet.logger.Debug(fmt.Sprintf("Proxy IPV6:Port Addr %v:%v", proxySet.IP6, proxySet.Port))
	proxySet.logger.Info(fmt.Sprintf("Proxy started at port:%v", proxySet.Port))

	return &proxySet
}

// isPortAvailable function checks whether a local port is occupied and returns a boolean value indicating its availability.
func isPortAvailable(port uint32) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%v", port))
	if err != nil {
		return false
	}
	defer ln.Close()
	return true
}

var caStorePath = []string{
	"/usr/local/share/ca-certificates/",
	"/etc/pki/ca-trust/source/anchors/",
	"/etc/ca-certificates/trust-source/anchors/",
	"/etc/pki/trust/anchors/",
	"/etc/pki/ca-trust/source/anchors/",
	"/usr/local/share/certs/",
	"/etc/ssl/certs/",
}

var caStoreUpdateCmd = []string{
	"update-ca-certificates",
	"update-ca-trust",
	"trust extract-compat",
	"update-ca-trust extract",
	"certctl rehash",
}

type certKeyPair struct {
	cert tls.Certificate
	host string
}

var (
	caPrivKey      interface{}
	caCertParsed   *x509.Certificate
	destinationUrl string
)

func certForClient(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	// Generate a new server certificate and private key for the given hostname
	destinationUrl = clientHello.ServerName

	cfsslLog.Level = cfsslLog.LevelError

	serverReq := &csr.CertificateRequest{
		//Make the name accordng to the ip of the request
		CN: clientHello.ServerName,
		Hosts: []string{
			clientHello.ServerName,
		},
		KeyRequest: csr.NewKeyRequest(),
	}

	serverCsr, serverKey, err := csr.ParseRequest(serverReq)
	if err != nil {
		return nil, fmt.Errorf(Emoji+"failed to create server CSR: %v", err)
	}
	cryptoSigner, ok := caPrivKey.(crypto.Signer)
	if !ok {
		log.Printf(Emoji, "Error in typecasting the caPrivKey")
	}
	signerd, err := local.NewSigner(cryptoSigner, caCertParsed, signer.DefaultSigAlgo(cryptoSigner), nil)
	if err != nil {
		return nil, fmt.Errorf(Emoji+"failed to create signer: %v", err)
	}

	serverCert, err := signerd.Sign(signer.SignRequest{
		Hosts:   serverReq.Hosts,
		Request: string(serverCsr),
		Profile: "web",
	})
	if err != nil {
		return nil, fmt.Errorf(Emoji+"failed to sign server certificate: %v", err)
	}

	// Load the server certificate and private key
	serverTlsCert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, fmt.Errorf(Emoji+"failed to load server certificate and key: %v", err)
	}

	return &serverTlsCert, nil
}

// startProxy function initiates a proxy on the specified port to handle redirected outgoing network calls.
func (ps *ProxySet) startProxy(ctx context.Context) {

	port := ps.Port

	proxyAddress4 := util.ToIP4AddressStr(ps.IP4)
	ps.logger.Debug("", zap.Any("ProxyAddress4", proxyAddress4))

	proxyAddress6 := util.ToIPv6AddressStr(ps.IP6)
	ps.logger.Debug("", zap.Any("ProxyAddress6", proxyAddress6))

	listener, err := net.Listen("tcp", fmt.Sprintf(":%v", port))
	if err != nil {
		ps.logger.Error(fmt.Sprintf("failed to start proxy on port:%v", port), zap.Error(err))
		return
	}
	ps.Listener = listener

	ps.logger.Debug(fmt.Sprintf("Proxy server is listening on %s", fmt.Sprintf(":%v", listener.Addr())))
	ps.logger.Debug("Proxy will accept both ipv4 and ipv6 connections", zap.Any("Ipv4", proxyAddress4), zap.Any("Ipv6", proxyAddress6))

	// TODO: integerate method For TLS connections
	// config := &tls.Config{
	// 	GetCertificate: certForClient,
	// }
	// listener = tls.NewListener(listener, config)

	// retry := 0
	for {
		conn, err := listener.Accept()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				break
			}

			ps.logger.Error("failed to accept connection to the proxy", zap.Error(err))
			break
		}
		ps.connMutex.Lock()
		ps.clientConnections = append(ps.clientConnections, conn)
		ps.connMutex.Unlock()
		go func() {
			defer ps.hook.Recover(pkg.GenerateRandomID())
			defer utils.HandlePanic()
			ps.handleConnection(conn, port, ctx)
		}()
	}
}

func (ps *ProxySet) startTcpDnsServer() {
	addr := fmt.Sprintf(":%v", ps.DnsPort)

	handler := ps
	server := &dns.Server{
		Addr:      addr,
		Net:       "tcp",
		Handler:   handler,
		ReusePort: true,
	}

	ps.TcpDnsServer = server

	ps.logger.Info(fmt.Sprintf("starting TCP DNS server at addr %v", server.Addr))
	err := server.ListenAndServe()
	if err != nil {
		ps.logger.Error("failed to start tcp dns server", zap.Any("addr", server.Addr), zap.Error(err))
	}
}

func (ps *ProxySet) startUdpDnsServer() {

	addr := fmt.Sprintf(":%v", ps.DnsPort)

	handler := ps
	server := &dns.Server{
		Addr:      addr,
		Net:       "udp",
		Handler:   handler,
		ReusePort: true,
		// DisableBackground: true,
	}

	ps.UdpDnsServer = server

	ps.logger.Info(fmt.Sprintf("starting UDP DNS server at addr %v", server.Addr))
	err := server.ListenAndServe()
	if err != nil {
		ps.logger.Error("failed to start dns server", zap.Any("addr", server.Addr), zap.Error(err))
	}
}

// For DNS caching
var cache = struct {
	sync.RWMutex
	m map[string][]dns.RR
}{m: make(map[string][]dns.RR)}

func generateCacheKey(name string, qtype uint16) string {
	return fmt.Sprintf("%s-%s", name, dns.TypeToString[qtype])
}

func (ps *ProxySet) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {

	ps.logger.Debug("", zap.Any("Source socket info", w.RemoteAddr().String()))
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true
	ps.logger.Debug("Got some Dns queries")
	for _, question := range r.Question {
		ps.logger.Debug("", zap.Any("Record Type", question.Qtype), zap.Any("Received Query", question.Name))

		key := generateCacheKey(question.Name, question.Qtype)

		// Check if the answer is cached
		cache.RLock()
		answers, found := cache.m[key]
		cache.RUnlock()

		if !found {
			// If not found in cache, resolve the DNS query only in case of record mode
			if models.GetMode() == models.MODE_RECORD {
				answers = resolveDNSQuery(question.Name, ps.logger, ps.DnsServerTimeout)
			}

			if answers == nil || len(answers) == 0 {
				// If the resolution failed, return a default A record with Proxy IP
				if question.Qtype == dns.TypeA {
					answers = []dns.RR{&dns.A{
						Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
						A:   net.ParseIP(util.ToIP4AddressStr(ps.IP4)),
					}}
					ps.logger.Debug("failed to resolve dns query hence sending proxy ip4", zap.Any("proxy Ip", util.ToIP4AddressStr(ps.IP4)))
				} else if question.Qtype == dns.TypeAAAA {
					if ps.dockerAppCmd {
						ps.logger.Debug("failed to resolve dns query (in docker case) hence sending empty record")
					} else {
						answers = []dns.RR{&dns.AAAA{
							Hdr:  dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 3600},
							AAAA: net.ParseIP(util.ToIPv6AddressStr(ps.IP6)),
						}}
						ps.logger.Debug("failed to resolve dns query hence sending proxy ip6", zap.Any("proxy Ip", util.ToIPv6AddressStr(ps.IP6)))
					}
				}

				ps.logger.Debug(fmt.Sprintf("Answers[when resolution failed for query:%v]:\n%v\n", question.Qtype, answers))
			}

			// Cache the answer
			cache.Lock()
			cache.m[key] = answers
			cache.Unlock()
			ps.logger.Debug(fmt.Sprintf("Answers[after caching it]:\n%v\n", answers))
		}

		ps.logger.Debug(fmt.Sprintf("Answers[before appending to msg]:\n%v\n", answers))
		msg.Answer = append(msg.Answer, answers...)
		ps.logger.Debug(fmt.Sprintf("Answers[After appending to msg]:\n%v\n", msg.Answer))
	}

	ps.logger.Debug(fmt.Sprintf("dns msg sending back:\n%v\n", msg))
	ps.logger.Debug(fmt.Sprintf("dns msg RCODE sending back:\n%v\n", msg.Rcode))
	ps.logger.Debug("Writing dns info back to the client...")
	err := w.WriteMsg(msg)
	if err != nil {
		ps.logger.Error("failed to write dns info back to the client", zap.Error(err))
	}
}

func resolveDNSQuery(domain string, logger *zap.Logger, timeout time.Duration) []dns.RR {
	// Remove the last dot from the domain name if it exists
	domain = strings.TrimSuffix(domain, ".")

	// Use the default system resolver
	resolver := net.DefaultResolver

	// Perform the lookup with the context
	ips, err := resolver.LookupIPAddr(context.Background(), domain)
	if err != nil {
		logger.Debug(fmt.Sprintf("failed to resolve the dns query for:%v", domain), zap.Error(err))
		return nil
	}

	// Convert the resolved IPs to dns.RR
	var answers []dns.RR
	for _, ip := range ips {
		if ipv4 := ip.IP.To4(); ipv4 != nil {
			answers = append(answers, &dns.A{
				Hdr: dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
				A:   ipv4,
			})
		} else {
			answers = append(answers, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 3600},
				AAAA: ip.IP,
			})
		}
	}

	if len(answers) > 0 {
		logger.Debug("net.LookupIP resolved the ip address...")
	}

	return answers
}

func isTLSHandshake(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 && (data[2] == 0x00 || data[2] == 0x01 || data[2] == 0x02 || data[2] == 0x03)
}

func (ps *ProxySet) handleTLSConnection(conn net.Conn) (net.Conn, error) {
	//Load the CA certificate and private key

	var err error
	caPrivKey, err = helpers.ParsePrivateKeyPEM(caPKey)
	if err != nil {
		ps.logger.Error(Emoji+"Failed to parse CA private key: ", zap.Error(err))
		return nil, err
	}
	caCertParsed, err = helpers.ParseCertificatePEM(caCrt)
	if err != nil {
		ps.logger.Error(Emoji+"Failed to parse CA certificate: ", zap.Error(err))
		return nil, err
	}

	// Create a TLS configuration
	config := &tls.Config{
		GetCertificate: certForClient,
	}

	// Wrap the TCP connection with TLS
	tlsConn := tls.Server(conn, config)
	// Perform the handshake
	err = tlsConn.Handshake()

	if err != nil {
		ps.logger.Error(Emoji+"failed to complete TLS handshake with the client with error: ", zap.Error(err))
		return nil, err
	}
	// Use the tlsConn for further communication
	// For example, you can read and write data using tlsConn.Read() and tlsConn.Write()

	// Here, we simply close the connection
	return tlsConn, nil
}

// handleConnection function executes the actual outgoing network call and captures/forwards the request and response messages.
func (ps *ProxySet) handleConnection(conn net.Conn, port uint32, ctx context.Context) {

	//checking how much time proxy takes to execute the flow.
	start := time.Now()

	ps.logger.Debug("", zap.Any("PID in proxy:", os.Getpid()))
	ps.logger.Debug("", zap.Any("Filtering in Proxy", ps.FilterPid))

	remoteAddr := conn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	ps.logger.Debug("Inside handleConnection of proxyServer", zap.Any("source port", sourcePort), zap.Any("Time", time.Now().Unix()))

	//TODO:  fix this bug, getting source port same as proxy port.
	if uint16(sourcePort) == uint16(ps.Port) {
		ps.logger.Debug("Inside handleConnection: Got source port == proxy port", zap.Int("Source port", sourcePort), zap.Int("Proxy port", int(ps.Port)))
		return
	}

	destInfo, err := ps.hook.GetDestinationInfo(uint16(sourcePort))
	if err != nil {
		ps.logger.Error("failed to fetch the destination info", zap.Any("Source port", sourcePort), zap.Any("err:", err))
		return
	}

	if destInfo.IpVersion == 4 {
		ps.logger.Debug("", zap.Any("DestIp4", destInfo.DestIp4), zap.Any("DestPort", destInfo.DestPort), zap.Any("KernelPid", destInfo.KernelPid))
	} else if destInfo.IpVersion == 6 {
		ps.logger.Debug("", zap.Any("DestIp6", destInfo.DestIp6), zap.Any("DestPort", destInfo.DestPort), zap.Any("KernelPid", destInfo.KernelPid))
	}

	// releases the occupied source port when done fetching the destination info
	ps.hook.CleanProxyEntry(uint16(sourcePort))
	//checking for the destination port of mysql
	if destInfo.DestPort == 3306 {
		var dst net.Conn
		var actualAddress = ""
		if destInfo.IpVersion == 4 {
			actualAddress = fmt.Sprintf("%v:%v", util.ToIP4AddressStr(destInfo.DestIp4), destInfo.DestPort)
		} else if destInfo.IpVersion == 6 {
			actualAddress = fmt.Sprintf("[%v]:%v", util.ToIPv6AddressStr(destInfo.DestIp6), destInfo.DestPort)
		}
		if models.GetMode() != models.MODE_TEST {
			dst, err = net.Dial("tcp", actualAddress)
			if err != nil {
				ps.logger.Error(Emoji+"failed to dial the connection to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
				conn.Close()
				return
				// }
			}
		}
		ParsersMap["mysql"].ProcessOutgoing([]byte{}, conn, dst, ctx)

	} else {
		clientConnId := util.GetNextID()
		reader := bufio.NewReader(conn)
		initialData := make([]byte, 5)
		testBuffer, err := reader.Peek(len(initialData))
		if err != nil {
			if err == io.EOF && len(testBuffer) == 0 {
				ps.logger.Debug("received EOF, closing connection", zap.Error(err), zap.Any("connectionID", clientConnId))
				conn.Close()
				return
			}
			ps.logger.Error("failed to peek the request message in proxy", zap.Error(err), zap.Any("proxy port", port))
			return
		}

		isTLS := isTLSHandshake(testBuffer)
		multiReader := io.MultiReader(reader, conn)
		conn = &CustomConn{
			Conn:   conn,
			r:      multiReader,
			logger: ps.logger,
		}
		if isTLS {
			conn, err = ps.handleTLSConnection(conn)
			if err != nil {
				ps.logger.Error("failed to handle TLS connection", zap.Error(err))
				return
			}
		}
		// attempt to read the conn until buffer is either filled or connection is closed
		var buffer []byte
		buffer, err = util.ReadBytes(conn)
		if err != nil && err != io.EOF {
			ps.logger.Error("failed to read the request message in proxy", zap.Error(err), zap.Any("proxy port", port))
			return
		}

		if err == io.EOF && len(buffer) == 0 {
			ps.logger.Debug("received EOF, closing connection", zap.Error(err), zap.Any("connectionID", clientConnId))
			return
		}

		ps.logger.Debug("received buffer", zap.Any("size", len(buffer)), zap.Any("buffer", buffer), zap.Any("connectionID", clientConnId))
		ps.logger.Debug(fmt.Sprintf("the clientConnId: %v", clientConnId))
		if err != nil {
			ps.logger.Error("failed to read the request message in proxy", zap.Error(err), zap.Any("proxy port", port))
			return
		}

		// dst stores the connection with actual destination for the outgoing network call
		var dst net.Conn
		var actualAddress = ""
		if destInfo.IpVersion == 4 {
			actualAddress = fmt.Sprintf("%v:%v", util.ToIP4AddressStr(destInfo.DestIp4), destInfo.DestPort)
		} else if destInfo.IpVersion == 6 {
			actualAddress = fmt.Sprintf("[%v]:%v", util.ToIPv6AddressStr(destInfo.DestIp6), destInfo.DestPort)
		}

		//Dialing for tls connection
		destConnId := util.GetNextID()
		logger := ps.logger.With(zap.Any("Client IP Address", conn.RemoteAddr().String()), zap.Any("Client ConnectionID", clientConnId), zap.Any("Destination IP Address", actualAddress), zap.Any("Destination ConnectionID", destConnId))
		if isTLS {
			logger.Debug("", zap.Any("isTLS", isTLS))
			config := &tls.Config{
				InsecureSkipVerify: true,
				ServerName:         destinationUrl,
			}
			dst, err = tls.Dial("tcp", fmt.Sprintf("%v:%v", destinationUrl, destInfo.DestPort), config)
			if err != nil && models.GetMode() != models.MODE_TEST {
				logger.Error("failed to dial the connection to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
				conn.Close()
				return
			}
		} else {
			dst, err = net.Dial("tcp", actualAddress)
			if err != nil && models.GetMode() != models.MODE_TEST {
				logger.Error("failed to dial the connection to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
				conn.Close()
				return
			}
		}

		for _, port := range ps.PassThroughPorts {
			if port == uint(destInfo.DestPort) {
				err = ps.callNext(buffer, conn, dst, logger)
				if err != nil {
					logger.Error("failed to pass through the outgoing call", zap.Error(err), zap.Any("for port", port))
					return
				}
			}
		}
		genericCheck := true
		//Checking for all the parsers.
		for _, parser := range ParsersMap {
			if parser.OutgoingType(buffer) {
				parser.ProcessOutgoing(buffer, conn, dst, ctx)
				genericCheck = false
			}
		}
		if genericCheck {
			logger.Debug("The external dependency is not supported. Hence using generic parser")
			genericparser.ProcessGeneric(buffer, conn, dst, ps.hook, logger, ctx)
		}
	}

	// Closing the user client connection
	conn.Close()
	duration := time.Since(start)
	ps.logger.Debug("time taken by proxy to execute the flow", zap.Any("Duration(ms)", duration.Milliseconds()))
}

func (ps *ProxySet) callNext(requestBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger) error {
	logger.Debug("trying to forward requests to target", zap.Any("Destination Addr", destConn.RemoteAddr().String()))

	defer destConn.Close()
	defer clientConn.Close()
	// channels for writing messages from proxy to destination or client
	destinationWriteChannel := make(chan []byte)
	clientWriteChannel := make(chan []byte)

	if requestBuffer != nil {
		_, err := destConn.Write(requestBuffer)
		if err != nil {
			logger.Error("failed to write request message to the destination server", zap.Error(err), zap.Any("Destination Addr", destConn.RemoteAddr().String()))
			return err
		}
	}

	for {
		// go routine to read from client
		go func() {
			defer ps.hook.Recover(pkg.GenerateRandomID())
			defer utils.HandlePanic()
			if clientConn != nil {
				buffer, err := util.ReadBytes(clientConn)
				if err != nil {
					if err == io.EOF {
						logger.Debug("received EOF, closing connection", zap.Error(err), zap.Any("connectionID", clientConn.RemoteAddr().String()))
						return
					}
					logger.Error("failed to read the request from client in proxy", zap.Error(err), zap.Any("Client Addr", clientConn.RemoteAddr().String()))
					return
				}
				destinationWriteChannel <- buffer
			}
		}()

		// go routine to read from destination
		go func() {
			defer ps.hook.Recover(pkg.GenerateRandomID())
			defer utils.HandlePanic()
			if destConn != nil {
				buffer, err := util.ReadBytes(destConn)
				if err != nil {
					if err == io.EOF {
						logger.Debug("received EOF, closing connection", zap.Error(err), zap.Any("connectionID", destConn.RemoteAddr().String()))
						return
					}
					logger.Error("failed to read the response from destination in proxy", zap.Error(err), zap.Any("Destination Addr", destConn.RemoteAddr().String()))
					return
				}
				clientWriteChannel <- buffer
			}
		}()

		select {
		case requestBuffer := <-destinationWriteChannel:
			// Write the request message to the actual destination server
			_, err := destConn.Write(requestBuffer)
			if err != nil {
				logger.Error("failed to write request message to the destination server", zap.Error(err), zap.Any("Destination Addr", destConn.RemoteAddr().String()))
				return err
			}
		case responseBuffer := <-clientWriteChannel:
			// Write the response message to the client
			_, err := clientConn.Write(responseBuffer)
			if err != nil {
				logger.Error("failed to write response to the client", zap.Error(err), zap.Any("Client Addr", clientConn.RemoteAddr().String()))
				return err
			}
		}
	}

}

func (ps *ProxySet) StopProxyServer() {
	ps.connMutex.Lock()
	for _, clientConn := range ps.clientConnections {
		clientConn.Close()
	}
	ps.connMutex.Unlock()

	if ps.Listener != nil {
		err := ps.Listener.Close()
		if err != nil {
			ps.logger.Error("failed to stop proxy server", zap.Error(err))
		}
	}

	// stop udp dns server & tcp dns server
	if ps.UdpDnsServer != nil {
		err := ps.UdpDnsServer.Shutdown()
		if err != nil {
			ps.logger.Error("failed to stop udp dns server", zap.Error(err))
		}
		ps.logger.Info("Udp Dns server stopped")
	}

	// stop tcp dns server & tcp dns server
	if ps.TcpDnsServer != nil {
		err := ps.TcpDnsServer.Shutdown()
		if err != nil {
			ps.logger.Error("failed to stop tcp dns server", zap.Error(err))
		}
		ps.logger.Info("Tcp Dns server stopped")
	}

	ps.logger.Info("proxy stopped...")
}
