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
}

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt::init();

    let args = Args::parse();
    info!(
        "Rust Proxy starting up... port: {}, uds_path: {}, ca_cert: {:?}",
        args.proxy_port, args.uds_path, args.ca_cert
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

    // Start Proxy
    if let Err(e) = proxy::start_proxy(args.proxy_port, ipc_client, args.ca_cert.as_deref()).await {
        error!("Proxy exited with error: {}", e);
        exit(1);
    }
}
