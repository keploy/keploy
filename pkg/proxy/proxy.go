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
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/helpers"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"

	"github.com/miekg/dns"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	genericparser "go.keploy.io/server/pkg/proxy/integrations/genericParser"
	"go.keploy.io/server/pkg/proxy/integrations/httpparser"
	"go.keploy.io/server/pkg/proxy/integrations/mongoparser"
	postgresparser "go.keploy.io/server/pkg/proxy/integrations/postgresParser"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"

	"time"
)

var Emoji = "\U0001F430" + " Keploy:"

type ProxySet struct {
	IP4              uint32
	IP6              [4]uint32
	Port             uint32
	hook             *hooks.Hook
	logger           *zap.Logger
	FilterPid        bool
	Listener         net.Listener
	DnsServer        *dns.Server
	DnsServerTimeout time.Duration
	dockerAppCmd     bool
}

type CustomConn struct {
	net.Conn
	r io.Reader
}

func (c *CustomConn) Read(p []byte) (int, error) {
	if len(p) == 0{
		fmt.Println("the length is 0 for the reading")
	}
	return c.r.Read(p)
}

type Conn struct {
	net.Conn
	r bufio.Reader
	// buffer       []byte
	// ReadComplete bool
	// pointer      int
}

func (c *Conn) Read(b []byte) (n int, err error) {
	return c.r.Read(b)

	// buffer, err := readBytes(c.conn)
	// if err != nil {
	// 	return 0, err
	// }
	// b = buffer
	// if len(buffer) == 0 && len(c.buffer) != 0 {
	// 	b = c.buffer
	// } else {
	// 	c.buffer = buffer
	// }
	// if c.ReadComplete {
	// 	b = c.buffer[c.pointer:(c.pointer + len(b))]

	// 	return 257, nil
	// }
	// n, err = c.Conn.Read(b)
	// if n > 0 {
	// 	c.buffer = append(c.buffer, b...)
	// }
	// if err != nil {
	// 	return n, err
	// }

	// return n, nil
}
func (ps *ProxySet) SetHook(hook *hooks.Hook) {
	ps.hook = hook
}

func getDistroInfo() string {
	osRelease, err := ioutil.ReadFile("/etc/os-release")
	if err != nil {
		fmt.Println(Emoji+"Error reading /etc/os-release:", err)
		return ""
	}
	lines := strings.Split(string(osRelease), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Split(strings.Trim(line[len("PRETTY_NAME="):], `"`), " ")[0]

		}
	}
	return ""
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

// JavaCAExists checks if the CA is already installed in the Java keystore
func JavaCAExists(alias string) bool {
	cmd := exec.Command("keytool", "-list", "-alias", alias, "-cacerts", "-storepass", "changeit")

	err := cmd.Run()
	return err == nil
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
func InstallJavaCA(logger *zap.Logger, caPath string) {
	// check if java is installed
	if isJavaInstalled() {
		javaHome, err := getJavaHome()
		if err != nil {
			logger.Error(Emoji+"Java detected but failed to find JAVA_HOME", zap.Error(err))
			return
		}
		// Assuming modern Java structure (without /jre/)
		cacertsPath := fmt.Sprintf("%s/lib/security/cacerts", javaHome)
		// You can modify these as per your requirements
		storePass := "changeit"
		alias := "keployCA"
		if JavaCAExists(alias) {
			logger.Info(Emoji+"Java detected and CA already exists", zap.String("path", cacertsPath))
			return
		}

		cmd := exec.Command("keytool", "-import", "-trustcacerts", "-cacerts", "-storepass", storePass, "-noprompt", "-alias", alias, "-file", caPath)

		cmdOutput, err := cmd.CombinedOutput()
		if err != nil {
			logger.Error(Emoji+"Java detected but failed to import CA", zap.Error(err), zap.String("output", string(cmdOutput)))
			return
		}

		logger.Info(Emoji+"Java detected and successfully imported CA", zap.String("path", cacertsPath), zap.String("output", string(cmdOutput)))
		fmt.Printf("Successfully imported CA:\n%s\n", cmdOutput)

	}
}

// BootProxies starts proxy servers on the idle local port, Default:16789
func BootProxies(logger *zap.Logger, opt Option, appCmd, appContainer string) *ProxySet {

	// assign default values if not provided
	distro := getDistroInfo()

	caPath := filepath.Join(caStorePath[distro], "ca.crt")

	fs, err := os.Create(caPath)
	if err != nil {
		logger.Error(Emoji+"failed to create custom ca certificate", zap.Error(err), zap.Any("root store path", caStorePath[distro]))
		return nil
	}

	_, err = fs.Write(caCrt)
	if err != nil {
		logger.Error(Emoji+"failed to write custom ca certificate", zap.Error(err), zap.Any("root store path", caStorePath[distro]))
		return nil
	}

	// install CA in the java keystore if java is installed
	InstallJavaCA(logger, caPath)

	// Update the trusted CAs store
	cmd := exec.Command("/usr/bin/sudo", caStoreUpdateCmd[distro])
	// log.Printf("This is the command2: %v", cmd)
	err = cmd.Run()
	if err != nil {
		log.Fatalf(Emoji+"Failed to update system trusted store: %v", err)
	}

	if opt.Port == 0 {
		opt.Port = 16789
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
	// localIp6, err := util.GetLocalIPv6()
	// if err != nil {
	// 	log.Fatalln(Emoji+"Failed to get the local Ip6 address", err)
	// }

	// proxyAddr6, err := util.ConvertIPv6ToUint32Array(localIp6)
	// if err != nil {
	// 	log.Fatalf(Emoji + "Failed to convert local Ip to IPV6")
	// }
	proxyAddr6 := [4]uint32{0000, 0000, 0000, 0001}

	//check if the user application is running inside docker container
	dCmd, _ := util.IsDockerRelatedCommand(appCmd)
	//check if the user application is running docker container using IDE
	dIDE := (appCmd == "" && len(appContainer) != 0)

	var proxySet = ProxySet{
		Port:         opt.Port,
		IP4:          proxyAddr4,
		IP6:          proxyAddr6,
		logger:       logger,
		dockerAppCmd: (dCmd || dIDE),
	}

	if isPortAvailable(opt.Port) {
		go proxySet.startProxy()
		// Resolve DNS queries only in case of test mode.
		if models.GetMode() == models.MODE_TEST {
			proxySet.logger.Debug(Emoji + "Running Dns Server in Test mode...")
			proxySet.logger.Info(Emoji + "Keploy has hijacked the DNS resolution mechanism, your application may misbehave in keploy test mode if you have provided wrong domain name in your application code.")
			go proxySet.startDnsServer()
		}
	} else {
		// TODO: Release eBPF resources if failed abruptly
		log.Fatalf(Emoji+"Failed to start Proxy at [Port:%v]: %v", opt.Port, err)
	}

	// randomID := generateRandomID()
	// go func() {

	// 	pcapFileName := fmt.Sprintf("capture_%s.pcap", randomID)

	// 	// Create a new PCAP file
	// 	pcapFile, err := os.Create(pcapFileName)
	// 	if err != nil {
	// 		log.Fatal("Error creating PCAP file:", err)
	// 	}
	// 	defer pcapFile.Close()

	// 	// Create a new PCAP writer
	// 	pcapWriter := pcapgo.NewWriter(pcapFile)
	// 	pcapWriter.WriteFileHeader(65536, layers.LinkTypeEthernet) // Adjust parameters as needed

	// 	// Create a packet source to capture packets from an interface
	// 	iface := "any" // Replace with your network interface name
	// 	handle, err := pcap.OpenLive(iface, 1600, true, pcap.BlockForever)
	// 	if err != nil {
	// 		log.Fatal("Error opening interface:", err)
	// 	}
	// 	defer handle.Close()

	// 	// Set up a packet filter for port 16789
	// 	filter := "port 16789"
	// 	err = handle.SetBPFFilter(filter)
	// 	if err != nil {
	// 		log.Fatal("Error setting BPF filter:", err)
	// 	}

	// 	// Start capturing packets
	// 	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	// 	for packet := range packetSource.Packets() {
	// 		// Convert packet data to bytes
	// 		packetData := packet.Data()

	// 		// Write packet data to the PCAP file
	// 		err = pcapWriter.WritePacket(packet.Metadata().CaptureInfo, packetData)
	// 		if err != nil {
	// 			log.Println("Error writing packet data:", err)
	// 		}
	// 	}

	// 	log.Println("Packet capture complete")
	// }()

	proxySet.logger.Debug(Emoji + fmt.Sprintf("Proxy IPv4:Port %v:%v", proxySet.IP4, proxySet.Port))
	proxySet.logger.Debug(Emoji + fmt.Sprintf("Proxy IPV6:Port Addr %v:%v", proxySet.IP6, proxySet.Port))
	proxySet.logger.Info(Emoji + fmt.Sprintf("Proxy started at port:%v", proxySet.Port))

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

// const (
// 	caCertPath       = "pkg/proxy/ca.crt" // Your CA certificate file path
// 	caPrivateKeyPath = "pkg/proxy/ca.key" // Your CA private key file path
// )

var caStorePath = map[string]string{
	"Ubuntu":   "/usr/local/share/ca-certificates/",
	"Pop!_OS":  "/usr/local/share/ca-certificates/",
	"Debian":   "/usr/local/share/ca-certificates/",
	"CentOS":   "/etc/pki/ca-trust/source/anchors/",
	"Red Hat":  "/etc/pki/ca-trust/source/anchors/",
	"Fedora":   "/etc/pki/ca-trust/source/anchors/",
	"Arch ":    "/etc/ca-certificates/trust-source/anchors/",
	"openSUSE": "/etc/pki/trust/anchors/",
	"SUSE ":    "/etc/pki/trust/anchors/",
	"Oracle ":  "/etc/pki/ca-trust/source/anchors/",
	"Alpine ":  "/usr/local/share/ca-certificates/",
	"Gentoo ":  "/usr/local/share/ca-certificates/",
	"FreeBSD":  "/usr/local/share/certs/",
	"OpenBSD":  "/etc/ssl/certs/",
	"macOS":    "/usr/local/share/ca-certificates/",
}

var caStoreUpdateCmd = map[string]string{
	"Ubuntu":   "update-ca-certificates",
	"Pop!_OS":  "update-ca-certificates",
	"Debian":   "update-ca-certificates",
	"CentOS":   "update-ca-trust",
	"Red Hat":  "update-ca-trust",
	"Fedora":   "update-ca-trust",
	"Arch ":    "trust extract-compat",
	"openSUSE": "update-ca-certificates",
	"SUSE ":    "update-ca-certificates",
	"Oracle ":  "update-ca-trust",
	"Alpine ":  "update-ca-certificates",
	"Gentoo ":  "update-ca-certificates",
	"FreeBSD":  "trust extract-compat",
	"OpenBSD":  "trust extract-compat",
	"macOS":    "security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain",
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
	// log.Printf("This is the server name: %s", clientHello.ServerName)
	destinationUrl = clientHello.ServerName
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
func (ps *ProxySet) startProxy() {

	port := ps.Port

	proxyAddress4 := util.ToIP4AddressStr(ps.IP4)
	ps.logger.Debug(Emoji, zap.Any("ProxyAddress4", proxyAddress4))

	proxyAddress6 := util.ToIPv6AddressStr(ps.IP6)
	ps.logger.Debug(Emoji, zap.Any("ProxyAddress6", proxyAddress6))

	listener, err := net.Listen("tcp", fmt.Sprintf(":%v", port))
	if err != nil {
		ps.logger.Error(Emoji+fmt.Sprintf("failed to start proxy on port:%v", port), zap.Error(err))
		return
	}
	ps.Listener = listener

	ps.logger.Debug(Emoji + fmt.Sprintf("Proxy server is listening on %s", fmt.Sprintf(":%v", listener.Addr())))
	ps.logger.Debug(Emoji+"Proxy will accept both ipv4 and ipv6 connections", zap.Any("Ipv4", proxyAddress4), zap.Any("Ipv6", proxyAddress6))

	// TODO: integerate method For TLS connections
	// config := &tls.Config{
	// 	GetCertificate: certForClient,
	// }
	// listener = tls.NewListener(listener, config)

	// retry := 0
	for {
		conn, err := listener.Accept()
		if err != nil {
			ps.logger.Error(Emoji+"failed to accept connection to the proxy", zap.Error(err))
			// retry++
			// if retry < 5 {
			// 	continue
			// }
			break
		}

		go ps.handleConnection(conn, port)
	}
}

func readableProxyAddress(ps *ProxySet) string {

	if ps != nil {
		port := ps.Port
		proxyAddress := util.ToIP4AddressStr(ps.IP4)
		return fmt.Sprintf(proxyAddress+":%v", port)
	}
	return ""
}

func (ps *ProxySet) startDnsServer() {

	proxyAddress4 := readableProxyAddress(ps)
	ps.logger.Debug(Emoji, zap.Any("ProxyAddress in dns server", proxyAddress4))

	//TODO: Need to make it configurable
	ps.DnsServerTimeout = 1 * time.Second

	handler := ps
	server := &dns.Server{
		Addr:      proxyAddress4,
		Net:       "udp",
		Handler:   handler,
		UDPSize:   65535,
		ReusePort: true,
		// DisableBackground: true,
	}

	ps.DnsServer = server

	ps.logger.Info(Emoji + fmt.Sprintf("starting DNS server at addr:%v", server.Addr))
	err := server.ListenAndServe()
	if err != nil {
		ps.logger.Error(Emoji+"failed to start dns server", zap.Any("addr", server.Addr), zap.Error(err))
	}

	ps.logger.Info(Emoji + fmt.Sprintf("DNS server started at port:%v", ps.Port))

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

	ps.logger.Debug(Emoji, zap.Any("Source socket info", w.RemoteAddr().String()))
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true
	ps.logger.Debug(Emoji + "Got some Dns queries")
	for _, question := range r.Question {
		ps.logger.Debug(Emoji, zap.Any("Record Type", question.Qtype), zap.Any("Received Query", question.Name))

		key := generateCacheKey(question.Name, question.Qtype)

		// Check if the answer is cached
		cache.RLock()
		answers, found := cache.m[key]
		cache.RUnlock()

		if !found {
			// If not found in cache, resolve the DNS query
			// answers = resolveDNSQuery(question.Name, ps.logger, ps.DnsServerTimeout)

			if answers == nil || len(answers) == 0 {
				// If the resolution failed, return a default A record with Proxy IP
				if question.Qtype == dns.TypeA {
					answers = []dns.RR{&dns.A{
						Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
						A:   net.ParseIP(util.ToIP4AddressStr(ps.IP4)),
					}}
					ps.logger.Debug(Emoji+"failed to resolve dns query hence sending proxy ip4", zap.Any("proxy Ip", util.ToIP4AddressStr(ps.IP4)))
				} else if question.Qtype == dns.TypeAAAA {
					if ps.dockerAppCmd {
						// answers = []dns.RR{&dns.AAAA{
						// 	Hdr:  dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 3600},
						// 	AAAA: net.ParseIP(""),
						// }}
						ps.logger.Debug(Emoji + "failed to resolve dns query (in docker case) hence sending empty record")
					} else {
						answers = []dns.RR{&dns.AAAA{
							Hdr:  dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 3600},
							AAAA: net.ParseIP(util.ToIPv6AddressStr(ps.IP6)),
						}}
						ps.logger.Debug(Emoji+"failed to resolve dns query hence sending proxy ip6", zap.Any("proxy Ip", util.ToIPv6AddressStr(ps.IP6)))
					}
				}

				fmt.Printf("Answers[when resolution failed for query:%v]:\n%v\n", question.Qtype, answers)
			}

			// Cache the answer
			cache.Lock()
			cache.m[key] = answers
			cache.Unlock()
			fmt.Printf("Answers[after caching it]:\n%v\n", answers)
		}

		fmt.Printf("Answers[before appending to msg]:\n%v\n", answers)
		msg.Answer = append(msg.Answer, answers...)
		fmt.Printf("Answers[After appending to msg]:\n%v\n", msg.Answer)
	}

	fmt.Printf(Emoji+"dns msg sending back:\n%v\n", msg)
	fmt.Printf(Emoji+"dns msg RCODE sending back:\n%v\n", msg.Rcode)
	ps.logger.Debug(Emoji + "Writing dns info back to the client...")
	err := w.WriteMsg(msg)
	if err != nil {
		ps.logger.Error(Emoji+"failed to write dns info back to the client", zap.Error(err))
	}
}

func resolveDNSQuery(domain string, logger *zap.Logger, timeout time.Duration) []dns.RR {
	// Remove the last dot from the domain name if it exists
	domain = strings.TrimSuffix(domain, ".")

	// Create a context with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Use the default system resolver
	resolver := net.DefaultResolver

	// Perform the lookup with the context
	ips, err := resolver.LookupIPAddr(ctx, domain)
	if err != nil {
		logger.Debug(Emoji+fmt.Sprintf("failed to resolve the dns query for:%v", domain), zap.Error(err))
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
		logger.Debug(Emoji + "net.LookupIP resolved the ip address...")
	}

	// for i, ans := range answers {
	// 	println("Answer-", i, ":", ans)
	// }

	return answers
}

func isTLSHandshake(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 && (data[2] == 0x00 || data[2] == 0x01 || data[2] == 0x02 || data[2] == 0x03)
}

func handleTLSConnection(conn net.Conn) (net.Conn, error) {
	// fmt.Println(Emoji, "Handling TLS connection from", conn.RemoteAddr().String())
	//Load the CA certificate and private key

	var err error
	caPrivKey, err = helpers.ParsePrivateKeyPEM(caPKey)
	if err != nil {
		log.Fatalf(Emoji, "Failed to parse CA private key: %v", err)
	}
	caCertParsed, err = helpers.ParseCertificatePEM(caCrt)
	if err != nil {
		log.Fatalf(Emoji, "Failed to parse CA certificate: %v", err)
	}

	// Create a TLS configuration
	config := &tls.Config{
		GetCertificate: certForClient,
	}

	// Wrap the TCP connection with TLS
	tlsConn := tls.Server(conn, config)

	// req := make([]byte, 1024)
	// fmt.Println("before the parsed req: ", string(req))

	// _, err = tlsConn.Read(req)
	if err != nil {
		log.Panic(Emoji+"failed reading the request message with error: ", err)
	}
	// fmt.Println("after the parsed req: ", string(req))
	// Perform the TLS handshake
	// err = tlsConn.Handshake()
	// if err != nil {
	// 	log.Println("Error performing TLS handshake:", err)
	// 	return
	// }

	// Use the tlsConn for further communication
	// For example, you can read and write data using tlsConn.Read() and tlsConn.Write()

	// Here, we simply close the connection
	// tlsConn.Close()
	return tlsConn, nil
}

// handleConnection function executes the actual outgoing network call and captures/forwards the request and response messages.
func (ps *ProxySet) handleConnection(conn net.Conn, port uint32) {

	//checking how much time proxy takes to execute the flow.
	start := time.Now()

	ps.logger.Debug(Emoji, zap.Any("PID in proxy:", os.Getpid()))
	ps.logger.Debug(Emoji, zap.Any("Filtering in Proxy", ps.FilterPid))

	remoteAddr := conn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	// ps.hook.PrintRedirectProxyMap()

	ps.logger.Debug(Emoji+"Inside handleConnection of proxyServer", zap.Any("source port", sourcePort), zap.Any("Time", time.Now().Unix()))

	//TODO:  fix this bug, getting source port same as proxy port.
	if uint16(sourcePort) == uint16(ps.Port) {
		ps.logger.Debug(Emoji+"Inside handleConnection: Got source port == proxy port", zap.Int("Source port", sourcePort), zap.Int("Proxy port", int(ps.Port)))
		return
	}

	destInfo, err := ps.hook.GetDestinationInfo(uint16(sourcePort))
	if err != nil {
		ps.logger.Error(Emoji+"failed to fetch the destination info", zap.Any("Source port", sourcePort), zap.Any("err:", err))
		return
	}

	if destInfo.IpVersion == 4 {
		ps.logger.Debug(Emoji, zap.Any("DestIp4", destInfo.DestIp4), zap.Any("DestPort", destInfo.DestPort), zap.Any("KernelPid", destInfo.KernelPid))
	} else if destInfo.IpVersion == 6 {
		ps.logger.Debug(Emoji, zap.Any("DestIp6", destInfo.DestIp6), zap.Any("DestPort", destInfo.DestPort), zap.Any("KernelPid", destInfo.KernelPid))
	}

	// releases the occupied source port when done fetching the destination info
	ps.hook.CleanProxyEntry(uint16(sourcePort))

	reader := bufio.NewReader(conn)
	initialData := make([]byte, 5)
	testBuffer, err := reader.Peek(len(initialData))
	if err != nil {
		ps.logger.Error(Emoji+"failed to peek the request message in proxy", zap.Error(err), zap.Any("proxy port", port))
		return
	}
	isTLS := isTLSHandshake(testBuffer)
	multiReader := io.MultiReader(reader, conn)
	conn = &CustomConn{
		Conn: conn,
		r:    multiReader,
	}
	if isTLS {
		conn, err = handleTLSConnection(conn)
		if err != nil {
			ps.logger.Error(Emoji+"failed to handle TLS connection", zap.Error(err))
			return
		}
	}
	connEstablishedAt := time.Now()
	rand.Seed(time.Now().UnixNano())
	clientConnId := rand.Intn(101)
	buffer, err := util.ReadBytes(conn)
	ps.logger.Debug(Emoji + fmt.Sprintf("the clientConnId: %v", clientConnId))
	readRequestDelay := time.Since(connEstablishedAt)
	if err != nil {
		ps.logger.Error(Emoji+"failed to read the request message in proxy", zap.Error(err), zap.Any("proxy port", port))
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
	destConnId := 0
	// if models.GetMode() != models.MODE_TEST {
		destConnId = rand.Intn(101)
		if isTLS {
			ps.logger.Debug(Emoji, zap.Any("isTLS", isTLS))
			config := &tls.Config{
				InsecureSkipVerify: false,
				ServerName:         destinationUrl,
			}
			dst, err = tls.Dial("tcp", fmt.Sprintf("%v:%v", destinationUrl, destInfo.DestPort), config)
			if err != nil && models.GetMode() != models.MODE_TEST {
				ps.logger.Error(Emoji+"failed to dial the connection to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
				conn.Close()
				return
			}
		} else {
			dst, err = net.Dial("tcp", actualAddress)
			if err != nil && models.GetMode() != models.MODE_TEST {
				ps.logger.Error(Emoji+"failed to dial the connection to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
				conn.Close()
				return
				// }
			}
		}
	// }

	switch {
	case httpparser.IsOutgoingHTTP(buffer):
		// capture the otutgoing http text messages]
		// if models.GetMode() == models.MODE_RECORD {
		// deps = append(deps, httpparser.CaptureHTTPMessage(buffer, conn, dst, ps.logger))
		// ps.hook.AppendDeps(httpparser.CaptureHTTPMessage(buffer, conn, dst, ps.logger))
		// }
		// var deps []*models.Mock = ps.hook.GetDeps()
		// fmt.Println("before http egress call, deps array: ", deps)
		httpparser.ProcessOutgoingHttp(buffer, conn, dst, ps.hook, ps.logger)
		// fmt.Println("after http egress call, deps array: ", deps)

		// ps.hook.SetDeps(deps)
	case mongoparser.IsOutgoingMongo(buffer):
		// var deps []*models.Mock = ps.hook.GetDeps()
		// fmt.Println("before mongo egress call, deps array: ", deps)
		ps.logger.Debug("into mongo parsing mode")
		mongoparser.ProcessOutgoingMongo(clientConnId, destConnId, buffer, conn, dst, ps.hook, connEstablishedAt, readRequestDelay, ps.logger)
		// fmt.Println("after mongo egress call, deps array: ", deps)

		// ps.hook.SetDeps(deps)

		// deps := mongoparser.CaptureMongoMessage(buffer, conn, dst, ps.logger)
		// for _, v := range deps {
		// 	ps.hook.AppendDeps(v)
		// }
	case postgresparser.IsOutgoingPSQL(buffer):
		fmt.Println("into psql desp mode, before passing")
		postgresparser.ProcessOutgoingPSQL(buffer, conn, dst, ps.hook, ps.logger)

	default:
		ps.logger.Debug(Emoji + "the external dependecy call is not supported")
		genericparser.ProcessGeneric(buffer, conn, dst, ps.hook, ps.logger)
		// fmt.Println("into default desp mode, before passing")
		// err = callNext(buffer, conn, dst, ps.logger)
		// if err != nil {
		// 	ps.logger.Error(Emoji+"failed to call next", zap.Error(err))
		// 	conn.Close()
		// 	return
		// }
		// fmt.Println("into default desp mode, after passing")

	}

	// Closing the user client connection
	conn.Close()
	duration := time.Since(start)
	ps.logger.Debug(Emoji+"time taken by proxy to execute the flow", zap.Any("Duration(ms)", duration.Milliseconds()))
}

func callNext(requestBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger) error {

	logger.Debug(Emoji+"trying to forward requests to target", zap.Any("Destination Addr", destConn.RemoteAddr().String()))

	defer destConn.Close()

	// channels for writing messages from proxy to destination or client
	destinationWriteChannel := make(chan []byte)
	clientWriteChannel := make(chan []byte)

	_, err := destConn.Write(requestBuffer)
	if err != nil {
		logger.Error(Emoji+"failed to write request message to the destination server", zap.Error(err), zap.Any("Destination Addr", destConn.RemoteAddr().String()))
		return err
	}

	for {
		fmt.Println("inside connection")
		// go routine to read from client
		go func() {
			buffer, err := util.ReadBytes(clientConn)
			if err != nil {
				logger.Error(Emoji+"failed to read the request from client in proxy", zap.Error(err), zap.Any("Client Addr", clientConn.RemoteAddr().String()))
				return
			}
			destinationWriteChannel <- buffer
		}()

		// go routine to read from destination
		go func() {
			buffer, err := util.ReadBytes(destConn)
			if err != nil {
				logger.Error(Emoji+"failed to read the response from destination in proxy", zap.Error(err), zap.Any("Destination Addr", destConn.RemoteAddr().String()))
				return
			}

			clientWriteChannel <- buffer
		}()

		select {
		case requestBuffer := <-destinationWriteChannel:
			// Write the request message to the actual destination server
			_, err := destConn.Write(requestBuffer)
			if err != nil {
				logger.Error(Emoji+"failed to write request message to the destination server", zap.Error(err), zap.Any("Destination Addr", destConn.RemoteAddr().String()))
				return err
			}
		case responseBuffer := <-clientWriteChannel:
			// Write the response message to the client
			_, err := clientConn.Write(responseBuffer)
			if err != nil {
				logger.Error(Emoji+"failed to write response to the client", zap.Error(err), zap.Any("Client Addr", clientConn.RemoteAddr().String()))
				return err
			}
		}
	}

}

func (ps *ProxySet) StopProxyServer() {
	err := ps.Listener.Close()
	if err != nil {
		ps.logger.Error(Emoji+"failed to stop proxy server", zap.Error(err))
	}

	// stop dns server only in case of test mode.
	if ps.DnsServer != nil {
		err = ps.DnsServer.Shutdown()
		if err != nil {
			ps.logger.Error(Emoji+"failed to stop dns server", zap.Error(err))
		}
		ps.logger.Info(Emoji + "Dns server stopped")
	}
	ps.logger.Info(Emoji + "proxy stopped...")
}

func generateRandomID() string {
	rand.Seed(time.Now().UnixNano())
	id := rand.Intn(100000) // Adjust the range as needed
	return fmt.Sprintf("%d", id)
}
