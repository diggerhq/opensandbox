//! Shared application state and session types.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Instant;
use tokio::sync::RwLock;

/// Session TTL in seconds (5 minutes)
pub const SESSION_TTL_SECS: u64 = 300;

/// A sandbox session with persistent environment and working directory.
#[derive(Debug)]
pub struct Session {
    pub id: String,
    pub sandbox_root: PathBuf,
    pub env: HashMap<String, String>,
    pub cwd: String,
    pub created_at: Instant,
    pub last_used: Instant,
}

/// Thread-safe session storage.
pub type Sessions = Arc<RwLock<HashMap<String, Session>>>;

/// Shared application state.
#[derive(Clone)]
pub struct AppState {
    pub sessions: Sessions,
}

impl AppState {
    pub fn new() -> Self {
        Self {
            sessions: Arc::new(RwLock::new(HashMap::new())),
        }
    }
}

impl Default for AppState {
    fn default() -> Self {
        Self::new()
    }
}
