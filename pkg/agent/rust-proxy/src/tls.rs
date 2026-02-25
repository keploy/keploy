use std::collections::HashMap;
use std::io::{BufReader, Cursor};
use std::sync::{Arc, Mutex};
use std::time::Instant;

use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{ClientConfig, DigitallySignedStruct, Error as RustlsError, ServerConfig, SignatureScheme};
use tokio::net::TcpStream;
use tokio_rustls::{TlsAcceptor, TlsConnector};
use tracing::{debug, info};

use crate::ipc::IpcClient;

/// Check if the first bytes of a buffer look like a TLS ClientHello.
pub fn is_tls_client_hello(buf: &[u8]) -> bool {
    if buf.len() < 5 {
        return false;
    }
    // ContentType = 0x16 (Handshake), ProtocolVersion = 0x03 0x0{0-3}
    buf[0] == 0x16 && buf[1] == 0x03 && buf[2] <= 0x03
}

/// Load the CA certificate chain from a PEM file path.
/// Returns the first certificate found (the CA cert used for chain building).
pub fn load_ca_cert(path: &str) -> std::io::Result<CertificateDer<'static>> {
    let file = std::fs::File::open(path)?;
    let mut reader = BufReader::new(file);
    let certs = rustls_pemfile::certs(&mut reader).collect::<Result<Vec<_>, _>>()?;
    if certs.is_empty() {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            "No certificates found in CA file",
        ));
    }
    Ok(certs.into_iter().next().unwrap())
}

/// Parse PEM certificate + key bytes (returned from Go IPC) into a rustls ServerConfig.
/// The cert chain includes: [per-host cert, CA cert].
pub fn build_server_config(
    cert_pem: &[u8],
    key_pem: &[u8],
    ca_cert: &CertificateDer<'static>,
) -> Result<Arc<ServerConfig>, Box<dyn std::error::Error + Send + Sync>> {
    // Parse the per-host cert
    let mut cert_reader = Cursor::new(cert_pem);
    let certs: Vec<CertificateDer<'static>> =
        rustls_pemfile::certs(&mut cert_reader).collect::<Result<Vec<_>, _>>()?;
    if certs.is_empty() {
        return Err("No certificates found in cert PEM".into());
    }

    // Build chain: [per-host, CA]
    let mut chain = certs;
    chain.push(ca_cert.clone());

    // Parse the private key
    let mut key_reader = Cursor::new(key_pem);
    let key = rustls_pemfile::private_key(&mut key_reader)?
        .ok_or("No private key found in key PEM")?;

    let config = ServerConfig::builder()
        .with_no_client_auth()
        .with_single_cert(chain, key)?;

    Ok(Arc::new(config))
}

/// Build a TLS client config that accepts any server certificate (like Go's InsecureSkipVerify).
pub fn build_insecure_client_config() -> Arc<ClientConfig> {
    let config = ClientConfig::builder()
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(InsecureServerCertVerifier))
        .with_no_client_auth();
    Arc::new(config)
}

/// A certificate verifier that accepts any server certificate.
#[derive(Debug)]
struct InsecureServerCertVerifier;

impl ServerCertVerifier for InsecureServerCertVerifier {
    fn verify_server_cert(
        &self,
        _end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, RustlsError> {
        Ok(ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, RustlsError> {
        Ok(HandshakeSignatureValid::assertion())
    }

    fn verify_tls13_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, RustlsError> {
        Ok(HandshakeSignatureValid::assertion())
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        vec![
            SignatureScheme::RSA_PKCS1_SHA256,
            SignatureScheme::RSA_PKCS1_SHA384,
            SignatureScheme::RSA_PKCS1_SHA512,
            SignatureScheme::ECDSA_NISTP256_SHA256,
            SignatureScheme::ECDSA_NISTP384_SHA384,
            SignatureScheme::ECDSA_NISTP521_SHA512,
            SignatureScheme::RSA_PSS_SHA256,
            SignatureScheme::RSA_PSS_SHA384,
            SignatureScheme::RSA_PSS_SHA512,
            SignatureScheme::ED25519,
        ]
    }
}

/// Extract the SNI (Server Name Indication) from a raw TLS ClientHello message.
/// Returns None if the buffer doesn't contain a valid ClientHello with SNI.
pub fn extract_sni(buf: &[u8]) -> Option<String> {
    // Minimum: 5 (record header) + 4 (handshake header) + 2 (version) + 32 (random)
    //        + 1 (session_id len) = 44
    if buf.len() < 44 {
        return None;
    }

    // Record layer:  ContentType(1) Version(2) Length(2)
    // Handshake:     Type(1) Length(3) ClientVersion(2) Random(32) SessionIDLen(1)
    let handshake_start = 5;
    if buf[handshake_start] != 0x01 {
        // Not a ClientHello
        return None;
    }

    let mut pos = handshake_start + 4; // skip handshake type + length

    // Client version (2 bytes)
    pos += 2;
    // Random (32 bytes)
    pos += 32;

    // Session ID
    if pos >= buf.len() {
        return None;
    }
    let session_id_len = buf[pos] as usize;
    pos += 1 + session_id_len;

    // Cipher Suites
    if pos + 2 > buf.len() {
        return None;
    }
    let cipher_suites_len = u16::from_be_bytes([buf[pos], buf[pos + 1]]) as usize;
    pos += 2 + cipher_suites_len;

    // Compression Methods
    if pos >= buf.len() {
        return None;
    }
    let comp_methods_len = buf[pos] as usize;
    pos += 1 + comp_methods_len;

    // Extensions length
    if pos + 2 > buf.len() {
        return None;
    }
    let extensions_len = u16::from_be_bytes([buf[pos], buf[pos + 1]]) as usize;
    pos += 2;
    let extensions_end = pos + extensions_len;

    // Walk extensions looking for SNI (type 0x0000)
    while pos + 4 <= extensions_end && pos + 4 <= buf.len() {
        let ext_type = u16::from_be_bytes([buf[pos], buf[pos + 1]]);
        let ext_len = u16::from_be_bytes([buf[pos + 2], buf[pos + 3]]) as usize;
        pos += 4;

        if ext_type == 0x0000 {
            // SNI extension
            // ServerNameList: length(2) + ServerName entries
            if pos + 2 > buf.len() {
                return None;
            }
            let _list_len = u16::from_be_bytes([buf[pos], buf[pos + 1]]) as usize;
            pos += 2;

            // ServerName: type(1) length(2) name(...)
            if pos + 3 > buf.len() {
                return None;
            }
            let name_type = buf[pos];
            let name_len = u16::from_be_bytes([buf[pos + 1], buf[pos + 2]]) as usize;
            pos += 3;

            if name_type == 0x00 && pos + name_len <= buf.len() {
                return String::from_utf8(buf[pos..pos + name_len].to_vec()).ok();
            }
            return None;
        }

        pos += ext_len;
    }

    None
}

/// Cached cert entry with TTL.
struct CertCacheEntry {
    config: Arc<ServerConfig>,
    created: Instant,
}

/// In-process cert cache keyed by hostname, 10 min TTL.
pub struct CertCache {
    map: Mutex<HashMap<String, CertCacheEntry>>,
    ca_cert: CertificateDer<'static>,
}

const CERT_CACHE_TTL_SECS: u64 = 600; // 10 minutes

impl CertCache {
    pub fn new(ca_cert: CertificateDer<'static>) -> Self {
        Self {
            map: Mutex::new(HashMap::new()),
            ca_cert,
        }
    }

    /// Get a cached ServerConfig for the given hostname, or request one from Go.
    pub async fn get_or_fetch(
        &self,
        server_name: &str,
        source_port: u16,
        ipc: &IpcClient,
    ) -> Result<Arc<ServerConfig>, Box<dyn std::error::Error + Send + Sync>> {
        // Fast path: check cache
        {
            let map = self.map.lock().unwrap();
            if let Some(entry) = map.get(server_name) {
                if entry.created.elapsed().as_secs() < CERT_CACHE_TTL_SECS {
                    debug!("TLS cert cache hit for {}", server_name);
                    return Ok(entry.config.clone());
                }
            }
        }

        // Slow path: request cert from Go via IPC
        info!("Requesting TLS cert from Go for {}", server_name);
        let cert_res = ipc.get_cert(server_name, source_port).await?;
        if !cert_res.success {
            return Err(format!("Go failed to generate cert for {}", server_name).into());
        }

        let config =
            build_server_config(cert_res.cert_pem.as_bytes(), cert_res.key_pem.as_bytes(), &self.ca_cert)?;

        // Store in cache
        {
            let mut map = self.map.lock().unwrap();
            map.insert(
                server_name.to_string(),
                CertCacheEntry {
                    config: config.clone(),
                    created: Instant::now(),
                },
            );
        }

        debug!("TLS cert cached for {}", server_name);
        Ok(config)
    }
}

/// A stream that prepends buffered bytes before reading from the inner stream.
/// Implements AsyncRead + AsyncWrite by delegating writes directly to the inner stream.
pub struct PrefixedStream {
    prefix: Vec<u8>,
    prefix_pos: usize,
    inner: TcpStream,
}

impl PrefixedStream {
    pub fn new(prefix: Vec<u8>, inner: TcpStream) -> Self {
        Self {
            prefix,
            prefix_pos: 0,
            inner,
        }
    }
}

impl tokio::io::AsyncRead for PrefixedStream {
    fn poll_read(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &mut tokio::io::ReadBuf<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        let this = self.get_mut();

        // First, yield any remaining prefix bytes
        if this.prefix_pos < this.prefix.len() {
            let remaining = &this.prefix[this.prefix_pos..];
            let to_copy = remaining.len().min(buf.remaining());
            buf.put_slice(&remaining[..to_copy]);
            this.prefix_pos += to_copy;
            return std::task::Poll::Ready(Ok(()));
        }

        // Then delegate to the inner stream
        std::pin::Pin::new(&mut this.inner).poll_read(cx, buf)
    }
}

impl tokio::io::AsyncWrite for PrefixedStream {
    fn poll_write(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &[u8],
    ) -> std::task::Poll<std::io::Result<usize>> {
        std::pin::Pin::new(&mut self.get_mut().inner).poll_write(cx, buf)
    }

    fn poll_flush(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        std::pin::Pin::new(&mut self.get_mut().inner).poll_flush(cx)
    }

    fn poll_shutdown(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        std::pin::Pin::new(&mut self.get_mut().inner).poll_shutdown(cx)
    }
}

/// Perform TLS MITM on the client side: accept the TLS connection from the app.
/// `client_hello_bytes` are the bytes already read from the socket.
/// Returns the TLS server stream wrapping a PrefixedStream and the extracted SNI.
pub async fn accept_tls_client(
    client_socket: TcpStream,
    client_hello_bytes: &[u8],
    source_port: u16,
    cert_cache: &CertCache,
    ipc: &IpcClient,
) -> Result<
    (tokio_rustls::server::TlsStream<PrefixedStream>, String),
    Box<dyn std::error::Error + Send + Sync>,
> {
    let sni = extract_sni(client_hello_bytes).unwrap_or_default();
    let server_name = if sni.is_empty() { "localhost" } else { &sni };

    info!(
        "TLS ClientHello detected, SNI={}, source_port={}",
        server_name, source_port
    );

    let server_config = cert_cache.get_or_fetch(server_name, source_port, ipc).await?;
    let acceptor = TlsAcceptor::from(server_config);

    let prefixed = PrefixedStream::new(client_hello_bytes.to_vec(), client_socket);
    let tls_stream = acceptor.accept(prefixed).await?;

    Ok((tls_stream, server_name.to_string()))
}

/// Upgrade the server connection (proxy → real destination) to TLS client mode.
pub async fn connect_tls_server(
    server_socket: TcpStream,
    server_name: &str,
    client_config: Arc<ClientConfig>,
) -> Result<tokio_rustls::client::TlsStream<TcpStream>, Box<dyn std::error::Error + Send + Sync>> {
    let connector = TlsConnector::from(client_config);

    let sni = ServerName::try_from(server_name.to_string())
        .unwrap_or_else(|_| ServerName::try_from("localhost".to_string()).unwrap());

    let tls_stream = connector.connect(sni, server_socket).await?;
    Ok(tls_stream)
}
