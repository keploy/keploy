use std::net::SocketAddr;
use std::sync::Arc;
use std::collections::HashMap;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::Mutex;
use tracing::{debug, error, info, warn};
use socket2::Socket;

use crate::ipc::IpcClient;

/// Apply low-latency TCP tuning to a TcpStream, matching Go proxy's tuneTCPConn:
/// - TCP_NODELAY: disable Nagle's algorithm
/// - 2 MB socket buffers: reduce TCP back-pressure on bursty traffic
fn tune_tcp_stream(stream: &TcpStream) {
    // Use set_nodelay directly on tokio's TcpStream
    let _ = stream.set_nodelay(true);

    // For buffer sizes, we need the raw socket via socket2
    let std_socket = unsafe {
        use std::os::unix::io::{AsRawFd, FromRawFd};
        Socket::from_raw_fd(stream.as_raw_fd())
    };
    let _ = std_socket.set_recv_buffer_size(2 * 1024 * 1024);
    let _ = std_socket.set_send_buffer_size(2 * 1024 * 1024);
    // Don't let socket2 close the fd — it's owned by the TcpStream
    std::mem::forget(std_socket);
}

/// Shared state for tracking active ingress listeners 
type IngressMap = Arc<Mutex<HashMap<u16, tokio::task::JoinHandle<()>>>>;

pub async fn start_proxy(proxy_port: u16, ipc_client: IpcClient) -> std::io::Result<()> {
    let addr = format!("0.0.0.0:{}", proxy_port);
    let listener = TcpListener::bind(&addr).await?;
    info!("Rust Proxy listening on {}", addr);

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

        tokio::spawn(async move {
            if let Err(e) = handle_connection(socket, peer_addr, ipc).await {
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
        let mut buf = vec![0; 8192];
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
        let mut buf = vec![0; 8192];
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

async fn handle_connection(
    mut client_socket: TcpStream,
    peer_addr: SocketAddr,
    ipc: IpcClient,
) -> std::io::Result<()> {
    let source_port = peer_addr.port();
    let conn_id = format!("{}:{}", peer_addr.ip(), source_port);
    
    info!("New connection from {} (source_port={})", peer_addr, source_port);

    // Apply TCP tuning to client socket
    tune_tcp_stream(&client_socket);

    // 1. Get Destination Info from Go Control Plane
    let dest_res = match ipc.get_dest(source_port, &conn_id).await {
        Ok(res) => res,
        Err(e) => {
            error!("Failed to get destination for port {}: {}", source_port, e);
            return Ok(());
        }
    };

    if !dest_res.success {
        warn!("Go returned failure for port {}. Dropping.", source_port);
        return Ok(());
    }

    let target_addr = format!("{}:{}", dest_res.ip, dest_res.port);
    info!("Resolved dest for port {}: {}", source_port, target_addr);

    // 2. Dial Target
    let mut server_socket = match TcpStream::connect(&target_addr).await {
        Ok(s) => s,
        Err(e) => {
            error!("Failed to connect to target {}: {}", target_addr, e);
            return Ok(());
        }
    };

    // Apply TCP tuning to server socket
    tune_tcp_stream(&server_socket);
    info!("Connected to target {}, starting bidirectional forwarding", target_addr);

    // 3. Setup bidirectional forwarding with teeing
    let (mut client_read, mut client_write) = client_socket.split();
    let (mut server_read, mut server_write) = server_socket.split();

    let conn_id_req = conn_id.clone();
    let conn_id_res = conn_id.clone();
    let ipc_req = ipc.clone();
    let ipc_res = ipc.clone();

    // Client -> Server (Request)
    let client_to_server = async move {
        let mut buf = vec![0; 8192];
        loop {
            let n = match client_read.read(&mut buf).await {
                Ok(n) if n > 0 => n,
                _ => break,
            };

            // Write to Server
            if let Err(e) = server_write.write_all(&buf[..n]).await {
                error!("Failed to write to server: {}", e);
                break;
            }

            // Tee to Go IPC
            ipc_req.send_data(&conn_id_req, true, &buf[..n]);
        }
    };

    // Server -> Client (Response)
    let server_to_client = async move {
        let mut buf = vec![0; 8192];
        loop {
            let n = match server_read.read(&mut buf).await {
                Ok(n) if n > 0 => n,
                _ => break,
            };

            // Write to Client
            if let Err(e) = client_write.write_all(&buf[..n]).await {
                error!("Failed to write to client: {}", e);
                break;
            }

            // Tee to Go IPC
            ipc_res.send_data(&conn_id_res, false, &buf[..n]);
        }
    };

    // Run both forwarding loops concurrently
    tokio::select! {
        _ = client_to_server => debug!("Client -> Server finished"),
        _ = server_to_client => debug!("Server -> Client finished"),
    }

    // 4. Notify Go that connection is closed
    debug!("Connection closed: {}", conn_id);
    ipc.send_close(&conn_id);

    Ok(())
}

