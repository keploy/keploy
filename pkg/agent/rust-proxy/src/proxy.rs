use std::net::SocketAddr;
use std::sync::Arc;
use std::collections::HashMap;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::Mutex;
use tracing::{debug, error, info, warn};
use socket2::Socket;

use crate::ebpf::BpfMapHandle;
use crate::ipc::IpcClient;
use crate::tls::{self, CertCache};

/// Apply low-latency TCP tuning to a TcpStream:
/// - TCP_NODELAY: disable Nagle's algorithm
/// - TCP_QUICKACK: disable delayed ACKs (biggest P99 win on loopback)
/// - 2 MB socket buffers: reduce TCP back-pressure on bursty traffic
fn tune_tcp_stream(stream: &TcpStream) {
    let _ = stream.set_nodelay(true);

    // For buffer sizes and TCP_QUICKACK, access the raw fd via socket2
    let std_socket = unsafe {
        use std::os::unix::io::{AsRawFd, FromRawFd};
        Socket::from_raw_fd(stream.as_raw_fd())
    };
    let _ = std_socket.set_recv_buffer_size(2 * 1024 * 1024);
    let _ = std_socket.set_send_buffer_size(2 * 1024 * 1024);

    // TCP_QUICKACK: disable delayed ACKs for lower per-packet latency.
    // This is a one-shot flag that the kernel resets, but setting it at
    // connection start helps the critical first few packets.
    unsafe {
        use std::os::unix::io::AsRawFd;
        let val: libc::c_int = 1;
        libc::setsockopt(
            std_socket.as_raw_fd(),
            libc::IPPROTO_TCP,
            libc::TCP_QUICKACK,
            &val as *const _ as *const libc::c_void,
            std::mem::size_of::<libc::c_int>() as libc::socklen_t,
        );
    }

    // Don't let socket2 close the fd — it's owned by the TcpStream
    std::mem::forget(std_socket);
}

/// Shared state for tracking active ingress listeners 
type IngressMap = Arc<Mutex<HashMap<u16, tokio::task::JoinHandle<()>>>>;

pub async fn start_proxy(
    proxy_port: u16,
    ipc_client: IpcClient,
    ca_cert_path: Option<&str>,
    ebpf_map: Option<Arc<BpfMapHandle>>,
) -> std::io::Result<()> {
    let addr = format!("0.0.0.0:{}", proxy_port);
    let listener = TcpListener::bind(&addr).await?;
    info!("Rust Proxy listening on {}", addr);

    // Install the ring crypto provider for rustls before any TLS usage.
    rustls::crypto::ring::default_provider()
        .install_default()
        .expect("Failed to install rustls ring CryptoProvider");

    // Load TLS infrastructure if CA cert is provided
    let tls_ctx: Option<Arc<TlsContext>> = if let Some(path) = ca_cert_path {
        match tls::load_ca_cert(path) {
            Ok(ca_cert) => {
                info!("Loaded CA certificate from {}", path);
                let cert_cache = CertCache::new(ca_cert);
                let client_config = tls::build_insecure_client_config();
                Some(Arc::new(TlsContext {
                    cert_cache,
                    client_config,
                }))
            }
            Err(e) => {
                error!("Failed to load CA certificate from {}: {}. TLS MITM disabled.", path, e);
                None
            }
        }
    } else {
        info!("No CA cert provided, TLS MITM disabled");
        None
    };

    // Pre-warm cert cache for common hostnames
    if let Some(ref ctx) = tls_ctx {
        let ctx_clone = ctx.clone();
        let ipc_clone = ipc_client.clone();
        tokio::spawn(async move {
            for name in &["localhost", "127.0.0.1"] {
                info!("Pre-warming TLS cert for {}", name);
                match ctx_clone.cert_cache.get_or_fetch(name, 0, &ipc_clone).await {
                    Ok(_) => info!("Pre-warmed TLS cert for {}", name),
                    Err(e) => warn!("Failed to pre-warm cert for {}: {}", name, e),
                }
            }
        });
    }

    let ingress_map: IngressMap = Arc::new(Mutex::new(HashMap::new()));

    // Spawn ingress command listener
    let ipc_for_ingress = ipc_client.clone();
    let ingress_map_clone = ingress_map.clone();
    tokio::spawn(async move {
        listen_for_ingress_commands(ipc_for_ingress, ingress_map_clone).await;
    });

    // Main egress (outgoing) proxy accept loop
    loop {
        let (socket, peer_addr) = listener.accept().await?;
        let ipc = ipc_client.clone();
        let tls_ctx_clone = tls_ctx.clone();
        let ebpf = ebpf_map.clone();

        tokio::spawn(async move {
            if let Err(e) = handle_connection(socket, peer_addr, ipc, tls_ctx_clone, ebpf).await {
                error!("Connection error: {}", e);
            }
        });
    }
}

/// Listens for StartIngress commands from Go and spawns ingress forwarders
async fn listen_for_ingress_commands(ipc: IpcClient, ingress_map: IngressMap) {
    loop {
        let cmd = match ipc.recv_ingress_cmd().await {
            Some(c) => c,
            None => {
                info!("Ingress command channel closed, stopping listener");
                break;
            }
        };

        let orig_port = cmd.orig_port;
        let new_port = cmd.new_port;

        // Check if already listening on this port
        {
            let map = ingress_map.lock().await;
            if map.contains_key(&orig_port) {
                debug!("Ingress forwarder already active for port {}", orig_port);
                continue;
            }
        }

        let ipc_clone = ipc.clone();
        let ingress_map_clone = ingress_map.clone();

        let handle = tokio::spawn(async move {
            if let Err(e) = run_ingress_forwarder(orig_port, new_port, ipc_clone).await {
                error!("Ingress forwarder for port {} failed: {}", orig_port, e);
            }
            // Remove from map when done
            let mut map = ingress_map_clone.lock().await;
            map.remove(&orig_port);
        });

        let mut map = ingress_map.lock().await;
        map.insert(orig_port, handle);
    }
}

/// Runs a TCP forwarder on orig_port, forwarding to 127.0.0.1:new_port.
/// Tees all data to Go IPC for test case capture.
async fn run_ingress_forwarder(orig_port: u16, new_port: u16, ipc: IpcClient) -> std::io::Result<()> {
    let addr = format!("0.0.0.0:{}", orig_port);
    let listener = TcpListener::bind(&addr).await?;
    info!("Ingress forwarder listening on {} → forwarding to 127.0.0.1:{}", addr, new_port);

    loop {
        let (client_socket, peer_addr) = listener.accept().await?;
        let ipc_clone = ipc.clone();
        let new_port = new_port;

        let orig_port = orig_port;
        tokio::spawn(async move {
            if let Err(e) = handle_ingress_connection(client_socket, peer_addr, new_port, orig_port, ipc_clone).await {
                error!("Ingress connection error: {}", e);
            }
        });
    }
}

/// Handles a single ingress connection: external client → Rust proxy → app
async fn handle_ingress_connection(
    client_socket: TcpStream,
    peer_addr: SocketAddr,
    new_port: u16,
    orig_port: u16,
    ipc: IpcClient,
) -> std::io::Result<()> {
    let conn_id = format!("ingress-{}:{}", peer_addr.ip(), peer_addr.port());
    debug!("Accepted ingress connection from {} (conn_id={})", peer_addr, conn_id);

    // Apply TCP tuning
    tune_tcp_stream(&client_socket);

    // Connect to the app's actual port
    let upstream_addr = format!("127.0.0.1:{}", new_port);
    let upstream_socket = match TcpStream::connect(&upstream_addr).await {
        Ok(s) => s,
        Err(e) => {
            error!("Failed to connect to upstream app at {}: {}", upstream_addr, e);
            return Ok(());
        }
    };
    tune_tcp_stream(&upstream_socket);

    debug!("Connected to upstream app at {}, starting ingress forwarding for {}", upstream_addr, conn_id);

    // Bidirectional forwarding with teeing
    let (mut client_read, mut client_write) = client_socket.into_split();
    let (mut upstream_read, mut upstream_write) = upstream_socket.into_split();

    let conn_id_req = conn_id.clone();
    let conn_id_res = conn_id.clone();
    let ipc_req = ipc.clone();
    let ipc_res = ipc.clone();
    let orig_port_req = orig_port;
    let orig_port_res = orig_port;

    // External Client → App (Request)
    let client_to_upstream = async move {
        let mut buf = vec![0u8; 65536];
        loop {
            let n = match client_read.read(&mut buf).await {
                Ok(n) if n > 0 => n,
                _ => break,
            };

            // Forward to app
            if let Err(e) = upstream_write.write_all(&buf[..n]).await {
                error!("Failed to write to upstream app: {}", e);
                break;
            }

            // Tee request data to Go IPC
            ipc_req.send_ingress_data(&conn_id_req, true, orig_port_req, &buf[..n]);
        }
    };

    // App → External Client (Response)
    let upstream_to_client = async move {
        let mut buf = vec![0u8; 65536];
        loop {
            let n = match upstream_read.read(&mut buf).await {
                Ok(n) if n > 0 => n,
                _ => break,
            };

            // Forward to external client
            if let Err(e) = client_write.write_all(&buf[..n]).await {
                error!("Failed to write to external client: {}", e);
                break;
            }

            // Tee response data to Go IPC
            ipc_res.send_ingress_data(&conn_id_res, false, orig_port_res, &buf[..n]);
        }
    };

    // Run both forwarding loops concurrently
    tokio::select! {
        _ = client_to_upstream => debug!("Ingress Client → App finished for {}", conn_id),
        _ = upstream_to_client => debug!("Ingress App → Client finished for {}", conn_id),
    }

    debug!("Ingress connection closed: {}", conn_id);
    ipc.send_ingress_close(&conn_id);

    Ok(())
}

/// Shared TLS MITM context
struct TlsContext {
    cert_cache: CertCache,
    client_config: Arc<rustls::ClientConfig>,
}

async fn handle_connection(
    client_socket: TcpStream,
    peer_addr: SocketAddr,
    ipc: IpcClient,
    tls_ctx: Option<Arc<TlsContext>>,
    ebpf_map: Option<Arc<BpfMapHandle>>,
) -> std::io::Result<()> {
    let source_port = peer_addr.port();
    let conn_id = format!("{}:{}", peer_addr.ip(), source_port);

    debug!("New connection from {} (source_port={})", peer_addr, source_port);

    tune_tcp_stream(&client_socket);

    // 1. Resolve destination — direct eBPF map lookup (μs) or IPC fallback (ms)
    let (dest_ip, dest_port) = if let Some(ref map) = ebpf_map {
        match map.lookup_and_delete(source_port) {
            Some(info) => {
                let (ip, port) = crate::ebpf::format_dest(&info);
                debug!("eBPF direct lookup for port {}: {}:{}", source_port, ip, port);
                ipc.send_notify_conn(&conn_id, &ip, port);
                (ip, port)
            }
            None => {
                warn!("eBPF map lookup failed for port {}. Dropping.", source_port);
                return Ok(());
            }
        }
    } else {
        match ipc.get_dest(source_port, &conn_id).await {
            Ok(res) if res.success => (res.ip, res.port),
            Ok(_) => {
                warn!("Go returned failure for port {}. Dropping.", source_port);
                return Ok(());
            }
            Err(e) => {
                error!("Failed to get destination for port {}: {}", source_port, e);
                return Ok(());
            }
        }
    };

    let target_addr = format!("{}:{}", dest_ip, dest_port);
    debug!("Resolved dest for port {}: {}", source_port, target_addr);

    // 2. Dial Target
    let server_socket = match TcpStream::connect(&target_addr).await {
        Ok(s) => s,
        Err(e) => {
            error!("Failed to connect to target {}: {}", target_addr, e);
            return Ok(());
        }
    };
    tune_tcp_stream(&server_socket);
    debug!("Connected to target {}, starting relay", target_addr);

    // 3. Bidirectional forwarding with inline TLS detection.
    //
    // We must use a select! loop (not parallel tasks) because TLS ClientHello
    // can appear mid-protocol for server-speaks-first protocols like MySQL:
    //   Server greeting → Client SSL Request → Client TLS ClientHello
    // The select! loop checks every client→server chunk for TLS so we can
    // intercept the upgrade regardless of when it appears.

    let (mut client_read, mut client_write) = client_socket.into_split();
    let (mut server_read, mut server_write) = server_socket.into_split();

    let mut client_buf = vec![0u8; 65536];
    let mut server_buf = vec![0u8; 65536];
    let mut tls_detected = false;
    let mut pending_client_data: Option<Vec<u8>> = None;

    loop {
        tokio::select! {
            result = client_read.read(&mut client_buf) => {
                let n = match result {
                    Ok(0) | Err(_) => break,
                    Ok(n) => n,
                };

                // Check for TLS ClientHello if we have a TLS context
                if tls_ctx.is_some() && tls::is_tls_client_hello(&client_buf[..n]) {
                    info!("TLS ClientHello detected on conn {}", conn_id);
                    tls_detected = true;
                    pending_client_data = Some(client_buf[..n].to_vec());
                    break;
                }

                // Normal forwarding: write to server + tee to Go
                if server_write.write_all(&client_buf[..n]).await.is_err() {
                    break;
                }
                ipc.send_data(&conn_id, true, &client_buf[..n]);
            }
            result = server_read.read(&mut server_buf) => {
                let n = match result {
                    Ok(0) | Err(_) => break,
                    Ok(n) => n,
                };

                // Forward to client + tee to Go
                if client_write.write_all(&server_buf[..n]).await.is_err() {
                    break;
                }
                ipc.send_data(&conn_id, false, &server_buf[..n]);
            }
        }
    }

    if tls_detected {
        let tls_ctx = tls_ctx.unwrap(); // Safe: we checked is_some() above
        let client_hello = pending_client_data.unwrap();

        // Reunite the split halves back into full TcpStreams
        let client_socket = client_read.reunite(client_write).map_err(|e| {
            std::io::Error::new(std::io::ErrorKind::Other, format!("Failed to reunite client socket: {}", e))
        })?;
        let server_socket = server_read.reunite(server_write).map_err(|e| {
            std::io::Error::new(std::io::ErrorKind::Other, format!("Failed to reunite server socket: {}", e))
        })?;

        // Perform TLS MITM
        return handle_tls_connection(
            client_socket,
            server_socket,
            &client_hello,
            source_port,
            &conn_id,
            &tls_ctx,
            &ipc,
        )
        .await;
    }

    // Normal close
    debug!("Connection closed: {}", conn_id);
    ipc.send_close(&conn_id);

    Ok(())
}

/// Handle a connection that has been identified as TLS.
/// Performs MITM: accept TLS from client, connect TLS to server, forward plaintext.
async fn handle_tls_connection(
    client_socket: TcpStream,
    server_socket: TcpStream,
    client_hello: &[u8],
    source_port: u16,
    conn_id: &str,
    tls_ctx: &TlsContext,
    ipc: &IpcClient,
) -> std::io::Result<()> {
    // 1. Accept TLS from client (act as TLS server)
    let (mut tls_client, server_name) = match tls::accept_tls_client(
        client_socket,
        client_hello,
        source_port,
        &tls_ctx.cert_cache,
        ipc,
    )
    .await
    {
        Ok(result) => result,
        Err(e) => {
            error!("TLS client handshake failed for {}: {}", conn_id, e);
            ipc.send_close(conn_id);
            return Ok(());
        }
    };

    info!("TLS MITM client handshake complete for {} (SNI={})", conn_id, server_name);

    // 2. Connect TLS to server (act as TLS client)
    let mut tls_server = match tls::connect_tls_server(
        server_socket,
        &server_name,
        tls_ctx.client_config.clone(),
    )
    .await
    {
        Ok(s) => s,
        Err(e) => {
            error!("TLS server handshake failed for {}: {}", conn_id, e);
            ipc.send_close(conn_id);
            return Ok(());
        }
    };

    info!("TLS MITM server handshake complete for {}", conn_id);

    // 3. Bidirectional forwarding on decrypted streams, teeing plaintext to Go
    let (mut tc_read, mut tc_write) = tokio::io::split(&mut tls_client);
    let (mut ts_read, mut ts_write) = tokio::io::split(&mut tls_server);

    let conn_id_req = conn_id.to_string();
    let conn_id_res = conn_id.to_string();
    let ipc_req = ipc.clone();
    let ipc_res = ipc.clone();

    let client_to_server = async move {
        let mut buf = vec![0u8; 65536];
        loop {
            let n = match tc_read.read(&mut buf).await {
                Ok(0) | Err(_) => break,
                Ok(n) => n,
            };
            if let Err(e) = ts_write.write_all(&buf[..n]).await {
                error!("TLS: Failed to write to server: {}", e);
                break;
            }
            // Tee PLAINTEXT to Go
            ipc_req.send_data(&conn_id_req, true, &buf[..n]);
        }
    };

    let server_to_client = async move {
        let mut buf = vec![0u8; 65536];
        loop {
            let n = match ts_read.read(&mut buf).await {
                Ok(0) | Err(_) => break,
                Ok(n) => n,
            };
            if let Err(e) = tc_write.write_all(&buf[..n]).await {
                error!("TLS: Failed to write to client: {}", e);
                break;
            }
            // Tee PLAINTEXT to Go
            ipc_res.send_data(&conn_id_res, false, &buf[..n]);
        }
    };

    tokio::select! {
        _ = client_to_server => debug!("TLS Client -> Server finished for {}", conn_id),
        _ = server_to_client => debug!("TLS Server -> Client finished for {}", conn_id),
    }

    debug!("TLS connection closed: {}", conn_id);
    ipc.send_close(conn_id);

    Ok(())
}

