mod ebpf;
mod ipc;
mod proxy;
mod tls;

use clap::Parser;
use std::process::exit;
use tracing::{error, info};

#[derive(Parser, Debug)]
#[command(author, version, about, long_about = None)]
struct Args {
    #[arg(long)]
    proxy_port: u16,

    #[arg(long)]
    uds_path: String,

    /// Path to the CA certificate PEM file for TLS MITM.
    /// If not provided, TLS interception is disabled and connections are forwarded as-is.
    #[arg(long)]
    ca_cert: Option<String>,

    /// Path to the pinned eBPF redirect_proxy_map (e.g. /sys/fs/bpf/keploy_redirect_proxy_map).
    /// When provided, Rust reads the eBPF map directly instead of asking Go via IPC.
    #[arg(long)]
    ebpf_map_pin: Option<String>,
}

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt::init();

    let args = Args::parse();
    info!(
        "Rust Proxy starting up... port: {}, uds_path: {}, ca_cert: {:?}, ebpf_map_pin: {:?}",
        args.proxy_port, args.uds_path, args.ca_cert, args.ebpf_map_pin
    );

    // Initialize IPC Client
    let ipc_client = match ipc::IpcClient::connect(&args.uds_path).await {
        Ok(client) => client,
        Err(e) => {
            error!("Failed to connect to Go IPC server at {}: {}", args.uds_path, e);
            exit(1);
        }
    };

    info!("Connected to Go IPC server.");

    // Open pinned eBPF map if provided
    let ebpf_map = args.ebpf_map_pin.as_deref().and_then(|path| {
        match ebpf::BpfMapHandle::open_pinned(path) {
            Ok(handle) => {
                info!("Opened pinned eBPF map at {}", path);
                Some(std::sync::Arc::new(handle))
            }
            Err(e) => {
                error!("Failed to open pinned eBPF map at {}: {}. Falling back to IPC.", path, e);
                None
            }
        }
    });

    // Start Proxy
    if let Err(e) = proxy::start_proxy(
        args.proxy_port,
        ipc_client,
        args.ca_cert.as_deref(),
        ebpf_map,
    ).await {
        error!("Proxy exited with error: {}", e);
        exit(1);
    }
}
