//! OpenSandbox - Linux sandbox with HTTP API and gRPC support.
//!
//! Usage:
//!   opensandbox serve [--port 8080] [--grpc-port 50051]   # Start HTTP + gRPC servers
//!   opensandbox --run -- <command> [args]                  # CLI mode (original)

#[cfg(not(target_os = "linux"))]
compile_error!("This program only works on Linux.");

#[cfg(target_os = "linux")]
mod grpc_server;
#[cfg(target_os = "linux")]
mod http_server;
#[cfg(target_os = "linux")]
mod sandbox;
#[cfg(target_os = "linux")]
mod state;

#[cfg(target_os = "linux")]
use clap::{Parser, Subcommand};
#[cfg(target_os = "linux")]
use std::collections::HashMap;

#[cfg(target_os = "linux")]
#[derive(Parser, Debug)]
#[command(name = "opensandbox")]
#[command(about = "Linux sandbox with HTTP API and gRPC support")]
struct Args {
    #[command(subcommand)]
    command: Option<Commands>,

    /// Run command directly (legacy mode)
    #[arg(long)]
    run: bool,

    /// CPU time limit in milliseconds
    #[arg(long, default_value = "1000")]
    time: u64,

    /// Memory limit in KB
    #[arg(long, default_value = "262144")]
    mem: u64,

    /// Maximum file size in KB
    #[arg(long, default_value = "0")]
    fsize: u64,

    /// Maximum number of open files
    #[arg(long, default_value = "64")]
    nofile: u64,

    /// Command and arguments to run
    #[arg(last = true)]
    cmd_args: Vec<String>,
}

#[cfg(target_os = "linux")]
#[derive(Subcommand, Debug)]
enum Commands {
    /// Start the HTTP and gRPC servers
    Serve {
        /// HTTP port to listen on
        #[arg(long, default_value = "8080")]
        port: u16,

        /// gRPC port to listen on
        #[arg(long, default_value = "50051")]
        grpc_port: u16,
    },
}

#[cfg(target_os = "linux")]
#[tokio::main]
async fn main() {
    use std::process::exit;

    tracing_subscriber::fmt::init();

    let args = Args::parse();

    // Must be root
    if !nix::unistd::geteuid().is_root() {
        eprintln!("Error: Must run as root (need CAP_SYS_ADMIN for namespaces)");
        exit(1);
    }

    match args.command {
        Some(Commands::Serve { port, grpc_port }) => {
            // Create shared state
            let state = state::AppState::new();

            // Spawn HTTP server
            let http_state = state.clone();
            let http_handle = tokio::spawn(async move {
                http_server::run_server(port, http_state).await;
            });

            // Spawn gRPC server
            let grpc_state = state.clone();
            let grpc_handle = tokio::spawn(async move {
                grpc_server::run_server(grpc_port, grpc_state).await;
            });

            // Wait for both servers (they run forever)
            tokio::select! {
                _ = http_handle => eprintln!("HTTP server exited"),
                _ = grpc_handle => eprintln!("gRPC server exited"),
            }
        }
        None if args.run => {
            // Legacy CLI mode
            if args.cmd_args.is_empty() {
                eprintln!("Error: No command specified");
                exit(1);
            }
            let config = sandbox::RunConfig {
                command: args.cmd_args,
                time_ms: args.time,
                mem_kb: args.mem,
                fsize_kb: args.fsize,
                nofile: args.nofile,
                env: HashMap::new(),
                cwd: "/".to_string(),
            };
            match sandbox::run_oneshot(&config) {
                Ok(result) => {
                    print!("{}", result.stdout);
                    eprint!("{}", result.stderr);
                    exit(result.exit_code.unwrap_or(1));
                }
                Err(e) => {
                    eprintln!("Error: {}", e);
                    exit(1);
                }
            }
        }
        None => {
            eprintln!("Error: Use 'serve' subcommand or --run flag");
            exit(1);
        }
    }
}

#[cfg(not(target_os = "linux"))]
fn main() {
    eprintln!("This program only works on Linux.");
    std::process::exit(1);
}
