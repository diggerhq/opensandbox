//! HTTP server implementation using Axum.

use crate::sandbox::{self, RunConfig, RunResult, SandboxFileEntry};
use crate::state::{AppState, Session, SessionStatus, Sessions, SESSION_TTL_SECS};
use axum::{
    body::Body,
    extract::{Host, Path, Query, State},
    extract::ws::{WebSocket, WebSocketUpgrade, Message as AxumWsMsg},
    http::{header, Request, StatusCode, Uri},
    response::{IntoResponse, Response},
    routing::{delete, get, post},
    Json, Router,
};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine};
use futures_util::{SinkExt, StreamExt};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::net::SocketAddr;
use std::time::{Duration, Instant};
use tokio::time::interval;
use tokio_tungstenite::tungstenite::Message as TungsteniteMsg;
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
    preview_url: Option<String>,
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
    preview_url: Option<String>,
    ports: Vec<u16>,
    status: String,
}

// File operation request/response types
#[derive(Deserialize)]
struct WriteFileRequest {
    path: String,
    content: String, // base64 encoded
}

#[derive(Serialize)]
struct WriteFileResponse {
    success: bool,
}

#[derive(Deserialize)]
struct WriteFilesRequest {
    files: Vec<WriteFileEntry>,
}

#[derive(Deserialize)]
struct WriteFileEntry {
    path: String,
    content: String, // base64 encoded
}

#[derive(Serialize)]
struct WriteFilesResponse {
    success: bool,
    errors: Vec<WriteFileError>,
}

#[derive(Serialize)]
struct WriteFileError {
    path: String,
    error: String,
}

#[derive(Deserialize)]
struct ReadFileQuery {
    path: String,
}

#[derive(Serialize)]
struct ReadFileResponse {
    content: String, // base64 encoded
}

#[derive(Deserialize)]
struct ListFilesQuery {
    path: String,
}

#[derive(Serialize)]
struct FileEntry {
    name: String,
    path: String,
    is_directory: bool,
    size: u64,
}

#[derive(Serialize)]
struct ListFilesResponse {
    files: Vec<FileEntry>,
}

#[derive(Deserialize)]
struct BackgroundRunRequest {
    command: Vec<String>,
    #[serde(default = "default_port")]
    port: u16,
    #[serde(default)]
    env: HashMap<String, String>,
    #[serde(default = "default_cwd")]
    cwd: String,
}

fn default_port() -> u16 { 5173 }

#[derive(Serialize)]
struct BackgroundRunResponse {
    pid: u32,
    port: u16,
    preview_url: Option<String>,
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

    let preview_domain = state.preview_domain.clone();

    let app = Router::new()
        // Session management
        .route("/sessions", post(create_session))
        .route("/sessions", get(list_sessions))
        .route("/sessions/:id", get(get_session))
        .route("/sessions/:id", delete(delete_session))
        .route("/sessions/:id/run", post(run_in_session))
        .route("/sessions/:id/background", post(run_background))
        .route("/sessions/:id/background", delete(kill_background))
        .route("/sessions/:id/env", post(set_env))
        .route("/sessions/:id/cwd", post(set_cwd))
        // File operations
        .route("/sessions/:id/files/write", post(write_file))
        .route("/sessions/:id/files/write-bulk", post(write_files_bulk))
        .route("/sessions/:id/files/read", get(read_file))
        .route("/sessions/:id/files/list", get(list_files))
        // Background diagnostics
        .route("/sessions/:id/background/status", get(background_status))
        // Stateless run
        .route("/run", post(run_oneshot))
        // Health check
        .route("/health", get(health))
        // Preview proxy: catches all unmatched requests and checks Host header
        .fallback(preview_proxy)
        .with_state(state);

    let addr = SocketAddr::from(([0, 0, 0, 0], port));
    info!("Starting HTTP server on {}", addr);
    if let Some(ref domain) = preview_domain {
        info!("Preview domain: {}", domain);
    }

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

    // Generate preview URL if preview_domain is configured
    let preview_url = state
        .preview_domain
        .as_ref()
        .map(|domain| format!("https://{}.{}", session_id, domain));

    let session = Session {
        id: session_id.clone(),
        sandbox_root,
        env: req.env,
        cwd: "/".to_string(),
        created_at: Instant::now(),
        last_used: Instant::now(),
        preview_url: preview_url.clone(),
        ports: Vec::new(),
        status: SessionStatus::Running,
        background_pids: Vec::new(),
    };

    state.sessions.write().await.insert(session_id.clone(), session);
    info!("Created session: {}", session_id);

    Ok(Json(CreateSessionResponse {
        session_id,
        preview_url,
    }))
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
            preview_url: s.preview_url.clone(),
            ports: s.ports.clone(),
            status: format!("{:?}", s.status).to_lowercase(),
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
        preview_url: session.preview_url.clone(),
        ports: session.ports.clone(),
        status: format!("{:?}", session.status).to_lowercase(),
    }))
}

async fn delete_session(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> Result<StatusCode, StatusCode> {
    let mut sessions = state.sessions.write().await;
    if let Some(session) = sessions.remove(&id) {
        let sandbox_root = session.sandbox_root;
        let pids = session.background_pids;
        tokio::task::spawn_blocking(move || {
            // Kill background processes first
            for pid in pids {
                let _ = nix::sys::signal::kill(
                    nix::unistd::Pid::from_raw(pid as i32),
                    nix::sys::signal::Signal::SIGKILL,
                );
            }
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
            let pids = session.background_pids;
            tokio::task::spawn_blocking(move || {
                for pid in pids {
                    let _ = nix::sys::signal::kill(
                        nix::unistd::Pid::from_raw(pid as i32),
                        nix::sys::signal::Signal::SIGKILL,
                    );
                }
                sandbox::destroy_session_sandbox(&sandbox_root);
            });
        }
    }
}

// File operation handlers

async fn write_file(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(req): Json<WriteFileRequest>,
) -> Result<Json<WriteFileResponse>, (StatusCode, String)> {
    let sandbox_root = {
        let mut sessions = state.sessions.write().await;
        let session = sessions
            .get_mut(&id)
            .ok_or((StatusCode::NOT_FOUND, "Session not found".to_string()))?;
        session.last_used = Instant::now();
        session.sandbox_root.clone()
    };

    // Decode base64 content
    let content = BASE64
        .decode(&req.content)
        .map_err(|e| (StatusCode::BAD_REQUEST, format!("Invalid base64: {}", e)))?;

    tokio::task::spawn_blocking(move || {
        sandbox::write_file_in_sandbox(&sandbox_root, &req.path, &content)
    })
    .await
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e))?;

    Ok(Json(WriteFileResponse { success: true }))
}

async fn write_files_bulk(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(req): Json<WriteFilesRequest>,
) -> Result<Json<WriteFilesResponse>, (StatusCode, String)> {
    let sandbox_root = {
        let mut sessions = state.sessions.write().await;
        let session = sessions
            .get_mut(&id)
            .ok_or((StatusCode::NOT_FOUND, "Session not found".to_string()))?;
        session.last_used = Instant::now();
        session.sandbox_root.clone()
    };

    // Decode all files from base64 first
    let mut decoded_files: Vec<(String, Vec<u8>)> = Vec::with_capacity(req.files.len());
    for entry in &req.files {
        let content = BASE64
            .decode(&entry.content)
            .map_err(|e| (StatusCode::BAD_REQUEST, format!("Invalid base64 for {}: {}", entry.path, e)))?;
        decoded_files.push((entry.path.clone(), content));
    }

    // Write all files in a single blocking task
    let errors = tokio::task::spawn_blocking(move || {
        let mut errors = Vec::new();
        for (path, content) in &decoded_files {
            if let Err(e) = sandbox::write_file_in_sandbox(&sandbox_root, path, content) {
                errors.push(WriteFileError {
                    path: path.clone(),
                    error: e,
                });
            }
        }
        errors
    })
    .await
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?;

    Ok(Json(WriteFilesResponse {
        success: errors.is_empty(),
        errors,
    }))
}

async fn read_file(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Query(query): Query<ReadFileQuery>,
) -> Result<Json<ReadFileResponse>, (StatusCode, String)> {
    let sandbox_root = {
        let mut sessions = state.sessions.write().await;
        let session = sessions
            .get_mut(&id)
            .ok_or((StatusCode::NOT_FOUND, "Session not found".to_string()))?;
        session.last_used = Instant::now();
        session.sandbox_root.clone()
    };

    let path = query.path.clone();
    let content = tokio::task::spawn_blocking(move || {
        sandbox::read_file_in_sandbox(&sandbox_root, &path)
    })
    .await
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?
    .map_err(|e| (StatusCode::NOT_FOUND, e))?;

    Ok(Json(ReadFileResponse {
        content: BASE64.encode(&content),
    }))
}

async fn list_files(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Query(query): Query<ListFilesQuery>,
) -> Result<Json<ListFilesResponse>, (StatusCode, String)> {
    let sandbox_root = {
        let mut sessions = state.sessions.write().await;
        let session = sessions
            .get_mut(&id)
            .ok_or((StatusCode::NOT_FOUND, "Session not found".to_string()))?;
        session.last_used = Instant::now();
        session.sandbox_root.clone()
    };

    let path = query.path.clone();
    let entries = tokio::task::spawn_blocking(move || {
        sandbox::list_files_in_sandbox(&sandbox_root, &path)
    })
    .await
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?
    .map_err(|e| (StatusCode::NOT_FOUND, e))?;

    let files: Vec<FileEntry> = entries
        .into_iter()
        .map(|e| FileEntry {
            name: e.name,
            path: e.path,
            is_directory: e.is_directory,
            size: e.size,
        })
        .collect();

    Ok(Json(ListFilesResponse { files }))
}

// Background process handler

async fn run_background(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(req): Json<BackgroundRunRequest>,
) -> Result<Json<BackgroundRunResponse>, (StatusCode, String)> {
    let (sandbox_root, mut env, cwd, preview_url) = {
        let mut sessions = state.sessions.write().await;
        let session = sessions
            .get_mut(&id)
            .ok_or((StatusCode::NOT_FOUND, "Session not found".to_string()))?;
        session.last_used = Instant::now();
        (
            session.sandbox_root.clone(),
            session.env.clone(),
            session.cwd.clone(),
            session.preview_url.clone(),
        )
    };

    env.extend(req.env);
    let cwd = if req.cwd != "/" { req.cwd } else { cwd };

    // Auto-assign a unique port if client sends 0, otherwise use requested port
    let port = if req.port == 0 {
        state.allocate_port()
    } else {
        req.port
    };

    // Inject port as env var so vite/dev servers can use it
    env.insert("VITE_PORT".to_string(), port.to_string());
    env.insert("PORT".to_string(), port.to_string());

    info!("Assigning port {} for background process in session {}", port, id);

    let config = RunConfig {
        command: req.command,
        time_ms: 0,
        mem_kb: 0,
        fsize_kb: 0,
        nofile: 0,
        env,
        cwd,
    };

    let pid = tokio::task::spawn_blocking(move || {
        sandbox::run_background_in_session(&sandbox_root, &config)
    })
    .await
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e))?;

    // Track the background process and port
    {
        let mut sessions = state.sessions.write().await;
        if let Some(session) = sessions.get_mut(&id) {
            session.background_pids.push(pid);
            if !session.ports.contains(&port) {
                session.ports.push(port);
            }
        }
    }

    info!("Started background process pid={} port={} session={}", pid, port, id);

    Ok(Json(BackgroundRunResponse {
        pid,
        port,
        preview_url,
    }))
}

// Kill all background processes for a session

async fn kill_background(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> Result<Json<serde_json::Value>, (StatusCode, String)> {
    let pids = {
        let mut sessions = state.sessions.write().await;
        let session = sessions
            .get_mut(&id)
            .ok_or((StatusCode::NOT_FOUND, "Session not found".to_string()))?;
        session.last_used = Instant::now();
        let pids = session.background_pids.clone();
        session.background_pids.clear();
        session.ports.clear();
        pids
    };

    let killed: Vec<u32> = pids
        .iter()
        .filter(|&&pid| {
            let result = nix::sys::signal::kill(
                nix::unistd::Pid::from_raw(pid as i32),
                nix::sys::signal::Signal::SIGKILL,
            );
            result.is_ok()
        })
        .copied()
        .collect();

    info!("Killed {} background processes for session {}: {:?}", killed.len(), id, killed);

    Ok(Json(serde_json::json!({
        "killed": killed,
        "total": pids.len(),
    })))
}

// Background diagnostics handler

#[derive(Serialize)]
struct BackgroundStatusResponse {
    pids: Vec<BackgroundPidStatus>,
    log: String,
}

#[derive(Serialize)]
struct BackgroundPidStatus {
    pid: u32,
    alive: bool,
}

async fn background_status(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> Result<Json<BackgroundStatusResponse>, (StatusCode, String)> {
    let (sandbox_root, pids) = {
        let sessions = state.sessions.read().await;
        let session = sessions
            .get(&id)
            .ok_or((StatusCode::NOT_FOUND, "Session not found".to_string()))?;
        (session.sandbox_root.clone(), session.background_pids.clone())
    };

    let (pid_statuses, log) = tokio::task::spawn_blocking(move || {
        let statuses: Vec<BackgroundPidStatus> = pids
            .iter()
            .map(|&pid| BackgroundPidStatus {
                pid,
                alive: sandbox::is_process_alive(pid),
            })
            .collect();
        let log = sandbox::read_background_log(&sandbox_root).unwrap_or_default();
        (statuses, log)
    })
    .await
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?;

    Ok(Json(BackgroundStatusResponse {
        pids: pid_statuses,
        log,
    }))
}

// Preview proxy handler (HTTP + WebSocket)

async fn preview_proxy(
    State(state): State<AppState>,
    Host(host): Host,
    ws: Option<WebSocketUpgrade>,
    req: Request<Body>,
) -> Response {
    let preview_domain = match &state.preview_domain {
        Some(d) => d.clone(),
        None => {
            return (StatusCode::NOT_FOUND, "Not found").into_response();
        }
    };

    // Parse session ID from host: {session-id}.{preview_domain}
    let suffix = format!(".{}", preview_domain);
    let session_id = match host.strip_suffix(&suffix) {
        Some(id) => id.to_string(),
        None => {
            return (StatusCode::NOT_FOUND, "Not found").into_response();
        }
    };

    // Look up session and find the port
    let port = {
        let mut sessions = state.sessions.write().await;
        let session = match sessions.get_mut(&session_id) {
            Some(s) => s,
            None => {
                return (StatusCode::NOT_FOUND, format!("Session {} not found", session_id))
                    .into_response();
            }
        };
        session.last_used = Instant::now();
        // Use first registered port, default to 5173
        session.ports.first().copied().unwrap_or(5173)
    };

    // Handle WebSocket upgrade
    if let Some(ws) = ws {
        let path = req.uri().path().to_string();
        let query = req.uri().query().map(|q| format!("?{}", q)).unwrap_or_default();
        let ws_url = format!("ws://127.0.0.1:{}{}{}", port, path, query);
        info!("WebSocket proxy: {} -> {}", host, ws_url);
        return ws.on_upgrade(move |socket| ws_proxy(socket, ws_url));
    }

    // Regular HTTP proxy
    let path = req.uri().path();
    let query = req.uri().query().map(|q| format!("?{}", q)).unwrap_or_default();
    let target_url = format!("http://127.0.0.1:{}{}{}", port, path, query);

    info!("Preview proxy: {} -> {}", host, target_url);

    let client = reqwest::Client::new();
    let method = match req.method().as_str() {
        "GET" => reqwest::Method::GET,
        "POST" => reqwest::Method::POST,
        "PUT" => reqwest::Method::PUT,
        "DELETE" => reqwest::Method::DELETE,
        "PATCH" => reqwest::Method::PATCH,
        "HEAD" => reqwest::Method::HEAD,
        "OPTIONS" => reqwest::Method::OPTIONS,
        _ => reqwest::Method::GET,
    };

    let mut proxy_req = client.request(method, &target_url);

    // Forward relevant headers
    for (name, value) in req.headers() {
        if name != header::HOST {
            if let Ok(v) = value.to_str() {
                proxy_req = proxy_req.header(name.as_str(), v);
            }
        }
    }

    // Forward body
    let body_bytes = match axum::body::to_bytes(req.into_body(), 10 * 1024 * 1024).await {
        Ok(b) => b,
        Err(e) => {
            return (StatusCode::BAD_REQUEST, format!("Failed to read body: {}", e))
                .into_response();
        }
    };
    if !body_bytes.is_empty() {
        proxy_req = proxy_req.body(body_bytes.to_vec());
    }

    // Execute the proxied request
    match proxy_req.send().await {
        Ok(proxy_resp) => {
            let status = StatusCode::from_u16(proxy_resp.status().as_u16())
                .unwrap_or(StatusCode::BAD_GATEWAY);
            let mut response = Response::builder().status(status);

            // Forward response headers
            for (name, value) in proxy_resp.headers() {
                response = response.header(name.as_str(), value.as_bytes());
            }

            match proxy_resp.bytes().await {
                Ok(body) => response
                    .body(Body::from(body))
                    .unwrap_or_else(|_| (StatusCode::BAD_GATEWAY, "Proxy error").into_response()),
                Err(e) => (StatusCode::BAD_GATEWAY, format!("Failed to read response: {}", e))
                    .into_response(),
            }
        }
        Err(e) => {
            info!("Preview proxy error: {}", e);
            (
                StatusCode::BAD_GATEWAY,
                format!("Could not connect to sandbox web server on port {}: {}", port, e),
            )
                .into_response()
        }
    }
}

/// Bidirectional WebSocket proxy between client and backend (e.g., Vite HMR).
async fn ws_proxy(client_ws: WebSocket, backend_url: String) {
    // Connect to backend WebSocket
    let backend_result = tokio_tungstenite::connect_async(&backend_url).await;
    let (backend_ws, _) = match backend_result {
        Ok(conn) => conn,
        Err(e) => {
            info!("WebSocket backend connection failed: {} -> {}", backend_url, e);
            return;
        }
    };

    info!("WebSocket proxy connected: {}", backend_url);

    let (mut client_tx, mut client_rx) = client_ws.split();
    let (mut backend_tx, mut backend_rx) = backend_ws.split();

    // Relay: client -> backend
    let c2b = async move {
        while let Some(msg) = client_rx.next().await {
            match msg {
                Ok(axum_msg) => {
                    let tung_msg = match axum_msg {
                        AxumWsMsg::Text(t) => TungsteniteMsg::Text(t.to_string()),
                        AxumWsMsg::Binary(b) => TungsteniteMsg::Binary(b.to_vec()),
                        AxumWsMsg::Ping(p) => TungsteniteMsg::Ping(p.to_vec()),
                        AxumWsMsg::Pong(p) => TungsteniteMsg::Pong(p.to_vec()),
                        AxumWsMsg::Close(_) => return,
                    };
                    if backend_tx.send(tung_msg).await.is_err() {
                        return;
                    }
                }
                Err(_) => return,
            }
        }
    };

    // Relay: backend -> client
    let b2c = async move {
        while let Some(msg) = backend_rx.next().await {
            match msg {
                Ok(tung_msg) => {
                    let axum_msg = match tung_msg {
                        TungsteniteMsg::Text(t) => AxumWsMsg::Text(t.to_string()),
                        TungsteniteMsg::Binary(b) => AxumWsMsg::Binary(b.into()),
                        TungsteniteMsg::Ping(p) => AxumWsMsg::Ping(p.into()),
                        TungsteniteMsg::Pong(p) => AxumWsMsg::Pong(p.into()),
                        TungsteniteMsg::Close(_) => return,
                        _ => continue,
                    };
                    if client_tx.send(axum_msg).await.is_err() {
                        return;
                    }
                }
                Err(_) => return,
            }
        }
    };

    // Run both directions concurrently; when one ends, drop the other
    tokio::select! {
        _ = c2b => {},
        _ = b2c => {},
    }

    info!("WebSocket proxy closed: {}", backend_url);
}
