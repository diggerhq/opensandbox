//! Shared application state and session types.

use serde::Serialize;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU16, Ordering};
use std::sync::Arc;
use std::time::Instant;
use tokio::sync::RwLock;

/// Starting port for auto-assignment (each session gets a unique port)
const PORT_RANGE_START: u16 = 10000;

/// Session TTL in seconds (5 minutes)
pub const SESSION_TTL_SECS: u64 = 300;

/// Status of a sandbox session.
#[derive(Debug, Clone, Copy, Serialize, PartialEq)]
#[serde(rename_all = "lowercase")]
pub enum SessionStatus {
    Running,
    Idle,
    Terminating,
}

/// A sandbox session with persistent environment and working directory.
#[derive(Debug)]
pub struct Session {
    pub id: String,
    pub sandbox_root: PathBuf,
    pub env: HashMap<String, String>,
    pub cwd: String,
    pub created_at: Instant,
    pub last_used: Instant,
    /// Preview URL for accessing web servers in the sandbox
    pub preview_url: Option<String>,
    /// Exposed ports
    pub ports: Vec<u16>,
    /// Current session status
    pub status: SessionStatus,
    /// PIDs of background processes (e.g., dev servers)
    pub background_pids: Vec<u32>,
}

/// Thread-safe session storage.
pub type Sessions = Arc<RwLock<HashMap<String, Session>>>;

/// Shared application state.
#[derive(Clone)]
pub struct AppState {
    pub sessions: Sessions,
    /// Preview domain for generating preview URLs (e.g., "preview.opensandbox.fly.dev")
    pub preview_domain: Option<String>,
    /// Port counter for auto-assigning unique ports to background processes
    pub next_port: Arc<AtomicU16>,
}

impl AppState {
    pub fn new() -> Self {
        Self {
            sessions: Arc::new(RwLock::new(HashMap::new())),
            preview_domain: None,
            next_port: Arc::new(AtomicU16::new(PORT_RANGE_START)),
        }
    }

    pub fn with_preview_domain(preview_domain: Option<String>) -> Self {
        Self {
            sessions: Arc::new(RwLock::new(HashMap::new())),
            preview_domain,
            next_port: Arc::new(AtomicU16::new(PORT_RANGE_START)),
        }
    }

    /// Allocate the next available port for a background process.
    pub fn allocate_port(&self) -> u16 {
        self.next_port.fetch_add(1, Ordering::Relaxed)
    }
}

impl Default for AppState {
    fn default() -> Self {
        Self::new()
    }
}
