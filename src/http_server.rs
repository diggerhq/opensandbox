//! HTTP server implementation using Axum.

use crate::sandbox::{self, RunConfig, RunResult};
use crate::state::{AppState, Session, Sessions, SESSION_TTL_SECS};
use axum::{
    extract::{Path, State},
    http::StatusCode,
    routing::{delete, get, post},
    Json, Router,
};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::net::SocketAddr;
use std::time::{Duration, Instant};
use tokio::time::interval;
use tracing::info;

// Request/Response types
#[derive(Deserialize)]
struct CreateSessionRequest {
    #[serde(default)]
    env: HashMap<String, String>,
}

#[derive(Serialize)]
struct CreateSessionResponse {
    session_id: String,
}

#[derive(Deserialize)]
struct RunRequest {
    command: Vec<String>,
    #[serde(default = "default_time")]
    time: u64,
    #[serde(default = "default_mem")]
    mem: u64,
    #[serde(default = "default_fsize")]
    fsize: u64,
    #[serde(default = "default_nofile")]
    nofile: u64,
    #[serde(default)]
    env: HashMap<String, String>,
    #[serde(default = "default_cwd")]
    cwd: String,
}

fn default_time() -> u64 { 300000 }
fn default_mem() -> u64 { 2097152 }
fn default_nofile() -> u64 { 256 }
fn default_fsize() -> u64 { 1048576 }
fn default_cwd() -> String { "/".to_string() }

#[derive(Serialize)]
struct SessionInfo {
    id: String,
    env: HashMap<String, String>,
    cwd: String,
    age_secs: u64,
    idle_secs: u64,
}

#[derive(Deserialize)]
struct SetEnvRequest {
    env: HashMap<String, String>,
}

#[derive(Deserialize)]
struct SetCwdRequest {
    cwd: String,
}

/// Run the HTTP server on the given port with the provided state.
pub async fn run_server(port: u16, state: AppState) {
    // Spawn cleanup task
    let sessions_clone = state.sessions.clone();
    tokio::spawn(async move {
        let mut interval = interval(Duration::from_secs(60));
        loop {
            interval.tick().await;
            cleanup_expired_sessions(&sessions_clone).await;
        }
    });

    let app = Router::new()
        // Session management
        .route("/sessions", post(create_session))
        .route("/sessions", get(list_sessions))
        .route("/sessions/:id", get(get_session))
        .route("/sessions/:id", delete(delete_session))
        .route("/sessions/:id/run", post(run_in_session))
        .route("/sessions/:id/env", post(set_env))
        .route("/sessions/:id/cwd", post(set_cwd))
        // Stateless run
        .route("/run", post(run_oneshot))
        // Health check
        .route("/health", get(health))
        .with_state(state);

    let addr = SocketAddr::from(([0, 0, 0, 0], port));
    info!("Starting HTTP server on {}", addr);

    let listener = tokio::net::TcpListener::bind(addr).await.unwrap();
    axum::serve(listener, app).await.unwrap();
}

async fn health() -> &'static str {
    "OK"
}

async fn create_session(
    State(state): State<AppState>,
    Json(req): Json<CreateSessionRequest>,
) -> Result<Json<CreateSessionResponse>, (StatusCode, String)> {
    let session_id = uuid::Uuid::new_v4().to_string();

    let sandbox_root = tokio::task::spawn_blocking({
        let session_id = session_id.clone();
        move || sandbox::create_session_sandbox(&session_id)
    })
    .await
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e))?;

    let session = Session {
        id: session_id.clone(),
        sandbox_root,
        env: req.env,
        cwd: "/".to_string(),
        created_at: Instant::now(),
        last_used: Instant::now(),
    };

    state.sessions.write().await.insert(session_id.clone(), session);
    info!("Created session: {}", session_id);

    Ok(Json(CreateSessionResponse { session_id }))
}

async fn list_sessions(
    State(state): State<AppState>,
) -> Json<Vec<SessionInfo>> {
    let sessions = state.sessions.read().await;
    let now = Instant::now();
    let list: Vec<SessionInfo> = sessions
        .values()
        .map(|s| SessionInfo {
            id: s.id.clone(),
            env: s.env.clone(),
            cwd: s.cwd.clone(),
            age_secs: now.duration_since(s.created_at).as_secs(),
            idle_secs: now.duration_since(s.last_used).as_secs(),
        })
        .collect();
    Json(list)
}

async fn get_session(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> Result<Json<SessionInfo>, StatusCode> {
    let sessions = state.sessions.read().await;
    let session = sessions.get(&id).ok_or(StatusCode::NOT_FOUND)?;
    let now = Instant::now();
    Ok(Json(SessionInfo {
        id: session.id.clone(),
        env: session.env.clone(),
        cwd: session.cwd.clone(),
        age_secs: now.duration_since(session.created_at).as_secs(),
        idle_secs: now.duration_since(session.last_used).as_secs(),
    }))
}

async fn delete_session(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> Result<StatusCode, StatusCode> {
    let mut sessions = state.sessions.write().await;
    if let Some(session) = sessions.remove(&id) {
        let sandbox_root = session.sandbox_root;
        tokio::task::spawn_blocking(move || {
            sandbox::destroy_session_sandbox(&sandbox_root);
        });
        info!("Deleted session: {}", id);
        Ok(StatusCode::NO_CONTENT)
    } else {
        Err(StatusCode::NOT_FOUND)
    }
}

async fn set_env(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(req): Json<SetEnvRequest>,
) -> Result<StatusCode, StatusCode> {
    let mut sessions = state.sessions.write().await;
    let session = sessions.get_mut(&id).ok_or(StatusCode::NOT_FOUND)?;
    session.env.extend(req.env);
    session.last_used = Instant::now();
    Ok(StatusCode::OK)
}

async fn set_cwd(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(req): Json<SetCwdRequest>,
) -> Result<StatusCode, StatusCode> {
    let mut sessions = state.sessions.write().await;
    let session = sessions.get_mut(&id).ok_or(StatusCode::NOT_FOUND)?;
    session.cwd = req.cwd;
    session.last_used = Instant::now();
    Ok(StatusCode::OK)
}

async fn run_in_session(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(req): Json<RunRequest>,
) -> Result<Json<RunResult>, (StatusCode, String)> {
    // Get session info
    let (sandbox_root, mut env, cwd) = {
        let mut sessions = state.sessions.write().await;
        let session = sessions
            .get_mut(&id)
            .ok_or((StatusCode::NOT_FOUND, "Session not found".to_string()))?;
        session.last_used = Instant::now();
        (session.sandbox_root.clone(), session.env.clone(), session.cwd.clone())
    };

    // Merge request env with session env
    env.extend(req.env);
    let cwd = if req.cwd != "/" { req.cwd } else { cwd };

    let config = RunConfig {
        command: req.command,
        time_ms: req.time,
        mem_kb: req.mem,
        fsize_kb: req.fsize,
        nofile: req.nofile,
        env,
        cwd,
    };

    let result = tokio::task::spawn_blocking(move || {
        sandbox::run_in_session(&sandbox_root, &config)
    })
    .await
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e))?;

    Ok(Json(result))
}

async fn run_oneshot(
    Json(req): Json<RunRequest>,
) -> Result<Json<RunResult>, (StatusCode, String)> {
    info!("POST /run - command: {:?}", req.command);
    let config = RunConfig {
        command: req.command,
        time_ms: req.time,
        mem_kb: req.mem,
        fsize_kb: req.fsize,
        nofile: req.nofile,
        env: req.env,
        cwd: req.cwd,
    };

    let result = tokio::task::spawn_blocking(move || sandbox::run_oneshot(&config))
        .await
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e))?;

    info!("POST /run - result: exit={:?} signal={:?}", result.exit_code, result.signal);
    Ok(Json(result))
}

async fn cleanup_expired_sessions(sessions: &Sessions) {
    let mut sessions = sessions.write().await;
    let now = Instant::now();
    let ttl = Duration::from_secs(SESSION_TTL_SECS);

    let expired: Vec<String> = sessions
        .iter()
        .filter(|(_, s)| now.duration_since(s.last_used) > ttl)
        .map(|(id, _)| id.clone())
        .collect();

    for id in expired {
        if let Some(session) = sessions.remove(&id) {
            info!("Cleaning up expired session: {}", id);
            let sandbox_root = session.sandbox_root;
            tokio::task::spawn_blocking(move || {
                sandbox::destroy_session_sandbox(&sandbox_root);
            });
        }
    }
}
