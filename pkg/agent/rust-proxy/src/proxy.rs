use std::net::SocketAddr;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
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

pub async fn start_proxy(proxy_port: u16, ipc_client: IpcClient) -> std::io::Result<()> {
    let addr = format!("0.0.0.0:{}", proxy_port);
    let listener = TcpListener::bind(&addr).await?;
    info!("Rust Proxy listening on {}", addr);

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

