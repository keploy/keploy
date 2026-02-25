use std::sync::Arc;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;
use tokio::sync::{mpsc, Mutex};
use serde::{Deserialize, Serialize};
use tracing::{error, info};

#[derive(Debug, Serialize)]
struct GetDestReq {
    source_port: u16,
    conn_id: String,
}

#[derive(Debug, Deserialize)]
pub struct GetDestRes {
    pub success: bool,
    pub ip: String,
    pub port: u16,
}

#[derive(Debug, Deserialize)]
pub struct StartIngressCmd {
    pub orig_port: u16,
    pub new_port: u16,
}

#[derive(Debug, Serialize)]
struct CloseReq {
    conn_id: String,
}

const MSG_TYPE_GET_DEST: u8 = 1;
const MSG_TYPE_GET_DEST_RES: u8 = 2;
const MSG_TYPE_DATA: u8 = 3;
const MSG_TYPE_CLOSE: u8 = 4;
const MSG_TYPE_START_INGRESS: u8 = 5;
const MSG_TYPE_INGRESS_DATA: u8 = 6;
const MSG_TYPE_INGRESS_CLOSE: u8 = 7;

struct BackgroundMsg {
    msg_type: u8,
    payload: Vec<u8>,
}

#[derive(Clone)]
pub struct IpcClient {
    ctrl_stream: Arc<Mutex<UnixStream>>,
    tx: mpsc::UnboundedSender<BackgroundMsg>,
    cmd_rx: Arc<Mutex<mpsc::UnboundedReceiver<StartIngressCmd>>>,
}

// Stream type identifiers for the handshake protocol.
// Each UDS connection sends this byte immediately after connecting
// so Go knows how to handle the stream.
const STREAM_TYPE_CTRL: u8 = 1;
const STREAM_TYPE_DATA: u8 = 2;
const STREAM_TYPE_CMD: u8 = 3;

impl IpcClient {
    pub async fn connect(path: &str) -> std::io::Result<Self> {
        // Connect and identify each stream with a handshake byte
        let mut ctrl_stream = UnixStream::connect(path).await?;
        ctrl_stream.write_u8(STREAM_TYPE_CTRL).await?;

        let mut data_stream = UnixStream::connect(path).await?;
        data_stream.write_u8(STREAM_TYPE_DATA).await?;

        let mut cmd_stream = UnixStream::connect(path).await?;
        cmd_stream.write_u8(STREAM_TYPE_CMD).await?;
        
        // Background task for streaming data/close messages without blocking
        let (tx, mut rx) = mpsc::unbounded_channel::<BackgroundMsg>();
        tokio::spawn(async move {
            while let Some(msg) = rx.recv().await {
                let length = (1 + msg.payload.len()) as u32;
                if let Err(e) = data_stream.write_u32_le(length).await {
                    error!("IPC Data write length failed: {}", e);
                    break;
                }
                if let Err(e) = data_stream.write_u8(msg.msg_type).await {
                    error!("IPC Data write type failed: {}", e);
                    break;
                }
                if let Err(e) = data_stream.write_all(&msg.payload).await {
                    error!("IPC Data write payload failed: {}", e);
                    break;
                }
            }
        });

        // Background task for receiving commands from Go (StartIngress, etc.)
        let (cmd_tx, cmd_rx) = mpsc::unbounded_channel::<StartIngressCmd>();
        tokio::spawn(async move {
            loop {
                // Read length prefix (4 bytes LE)
                let length = match cmd_stream.read_u32_le().await {
                    Ok(l) => l,
                    Err(e) => {
                        error!("IPC Cmd read length failed: {}", e);
                        break;
                    }
                };
                // Read message type (1 byte)
                let msg_type = match cmd_stream.read_u8().await {
                    Ok(t) => t,
                    Err(e) => {
                        error!("IPC Cmd read type failed: {}", e);
                        break;
                    }
                };
                // Read payload
                let payload_len = length as usize - 1;
                let mut payload = vec![0u8; payload_len];
                if payload_len > 0 {
                    if let Err(e) = cmd_stream.read_exact(&mut payload).await {
                        error!("IPC Cmd read payload failed: {}", e);
                        break;
                    }
                }

                match msg_type {
                    MSG_TYPE_START_INGRESS => {
                        match serde_json::from_slice::<StartIngressCmd>(&payload) {
                            Ok(cmd) => {
                                info!("Received StartIngress command: orig_port={}, new_port={}", cmd.orig_port, cmd.new_port);
                                let _ = cmd_tx.send(cmd);
                            }
                            Err(e) => {
                                error!("Failed to parse StartIngress command: {}", e);
                            }
                        }
                    }
                    _ => {
                        error!("Unknown command message type: {}", msg_type);
                    }
                }
            }
        });

        Ok(Self {
            ctrl_stream: Arc::new(Mutex::new(ctrl_stream)),
            tx,
            cmd_rx: Arc::new(Mutex::new(cmd_rx)),
        })
    }

    /// Receive the next StartIngress command from Go
    pub async fn recv_ingress_cmd(&self) -> Option<StartIngressCmd> {
        let mut rx = self.cmd_rx.lock().await;
        rx.recv().await
    }

    pub async fn get_dest(&self, source_port: u16, conn_id: &str) -> std::io::Result<GetDestRes> {
        let req = GetDestReq { 
            source_port,
            conn_id: conn_id.to_string(),
        };
        let payload = serde_json::to_vec(&req)?;
        
        // Lock ctrl stream for request-response cycle
        let mut stream = self.ctrl_stream.lock().await;
        
        let length = (1 + payload.len()) as u32;
        stream.write_u32_le(length).await?;
        stream.write_u8(MSG_TYPE_GET_DEST).await?;
        stream.write_all(&payload).await?;
        stream.flush().await?;
        
        // Wait for response
        let res_len = stream.read_u32_le().await?;
        let msg_type = stream.read_u8().await?;
        if msg_type != MSG_TYPE_GET_DEST_RES {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                format!("Expected GET_DEST_RES (2), got {}", msg_type),
            ));
        }
        
        let payload_len = res_len as usize - 1;
        let mut res_payload = vec![0u8; payload_len];
        if payload_len > 0 {
            stream.read_exact(&mut res_payload).await?;
        }
        
        let res: GetDestRes = serde_json::from_slice(&res_payload)?;
        Ok(res)
    }

    pub fn send_data(&self, conn_id: &str, is_request: bool, data: &[u8]) {
        // Build binary payload directly
        // [conn_id_len u8] [conn_id bytes] [direction u8: 0=req, 1=res] [data bytes]
        let mut payload = Vec::with_capacity(1 + conn_id.len() + 1 + data.len());
        payload.push(conn_id.len() as u8);
        payload.extend_from_slice(conn_id.as_bytes());
        payload.push(if is_request { 0 } else { 1 });
        payload.extend_from_slice(data);
        
        let _ = self.tx.send(BackgroundMsg {
            msg_type: MSG_TYPE_DATA,
            payload,
        });
    }

    pub fn send_close(&self, conn_id: &str) {
        let req = CloseReq {
            conn_id: conn_id.to_string(),
        };
        if let Ok(payload) = serde_json::to_vec(&req) {
            let _ = self.tx.send(BackgroundMsg {
                msg_type: MSG_TYPE_CLOSE,
                payload,
            });
        }
    }

    /// Send ingress (incoming) data to Go for test case capture.
    /// direction: 0 = request (external client → app), 1 = response (app → external client)
    pub fn send_ingress_data(&self, conn_id: &str, is_request: bool, orig_port: u16, data: &[u8]) {
        let mut payload = Vec::with_capacity(1 + conn_id.len() + 1 + 2 + data.len());
        payload.push(conn_id.len() as u8);
        payload.extend_from_slice(conn_id.as_bytes());
        payload.push(if is_request { 0 } else { 1 });
        payload.extend_from_slice(&orig_port.to_le_bytes());
        payload.extend_from_slice(data);

        let _ = self.tx.send(BackgroundMsg {
            msg_type: MSG_TYPE_INGRESS_DATA,
            payload,
        });
    }

    pub fn send_ingress_close(&self, conn_id: &str) {
        let req = CloseReq {
            conn_id: conn_id.to_string(),
        };
        if let Ok(payload) = serde_json::to_vec(&req) {
            let _ = self.tx.send(BackgroundMsg {
                msg_type: MSG_TYPE_INGRESS_CLOSE,
                payload,
            });
        }
    }
}
