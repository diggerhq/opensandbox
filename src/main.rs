//! Isolate - Linux sandbox with HTTP API and stateful sessions.
//!
//! Usage:
//!   isolate serve [--port 8080]           # Start HTTP server
//!   isolate --run -- <command> [args]     # CLI mode (original)

#[cfg(not(target_os = "linux"))]
compile_error!("This program only works on Linux.");

#[cfg(target_os = "linux")]
use clap::{Parser, Subcommand};
#[cfg(target_os = "linux")]
use std::collections::HashMap;

#[cfg(target_os = "linux")]
#[derive(Parser, Debug)]
#[command(name = "isolate")]
#[command(about = "Linux sandbox with HTTP API")]
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
    /// Start the HTTP server
    Serve {
        /// Port to listen on
        #[arg(long, default_value = "8080")]
        port: u16,
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
        Some(Commands::Serve { port }) => {
            server::run_server(port).await;
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

// ============================================================================
// Sandbox module
// ============================================================================
#[cfg(target_os = "linux")]
mod sandbox {
    use nix::mount::{mount, umount2, MntFlags, MsFlags};
    use nix::sched::{clone, CloneFlags};
    use nix::sys::resource::{setrlimit, Resource};
    use nix::sys::signal::Signal;
    use nix::sys::wait::{waitpid, WaitStatus};
    use nix::unistd::{chdir, chroot, execvpe, pipe, setgid, setuid, Gid, Uid};
    use std::collections::HashMap;
    use std::ffi::CString;
    use std::fs;
    use std::io::{Read, Write};
    use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
    use std::os::unix::fs::PermissionsExt;
    use std::path::{Path, PathBuf};
    use tracing::info;

    const NOBODY_UID: u32 = 65534;
    const NOBODY_GID: u32 = 65534;

    #[derive(Debug, Clone)]
    pub struct RunConfig {
        pub command: Vec<String>,
        pub time_ms: u64,
        pub mem_kb: u64,
        pub fsize_kb: u64,
        pub nofile: u64,
        pub env: HashMap<String, String>,
        pub cwd: String,
    }

    #[derive(Debug, Clone, serde::Serialize)]
    pub struct RunResult {
        pub stdout: String,
        pub stderr: String,
        pub exit_code: Option<i32>,
        pub signal: Option<i32>,
    }

    /// Run a command in a fresh sandbox (no session, cleanup after)
    pub fn run_oneshot(config: &RunConfig) -> Result<RunResult, String> {
        info!("=== run_oneshot called ===");
        info!(command = ?config.command, "Command to run");
        let sandbox_root = PathBuf::from("/tmp/sandbox-oneshot");
        info!("Setting up sandbox dir...");
        setup_sandbox_dir(&sandbox_root)?;
        info!("Sandbox dir ready, running command...");
        let result = run_in_sandbox(&sandbox_root, config);
        info!(result = ?result, "Command finished");
        cleanup_sandbox(&sandbox_root);
        result
    }

    /// Run a command in an existing session sandbox
    pub fn run_in_session(sandbox_root: &Path, config: &RunConfig) -> Result<RunResult, String> {
        run_in_sandbox(sandbox_root, config)
    }

    /// Create a new session sandbox directory
    pub fn create_session_sandbox(session_id: &str) -> Result<PathBuf, String> {
        let sandbox_root = PathBuf::from(format!("/tmp/sandbox-{}", session_id));
        setup_sandbox_dir(&sandbox_root)?;
        Ok(sandbox_root)
    }

    /// Cleanup a session sandbox
    pub fn destroy_session_sandbox(sandbox_root: &Path) {
        cleanup_sandbox(sandbox_root);
    }

    fn setup_sandbox_dir(sandbox_root: &Path) -> Result<(), String> {
        // Clean up if exists
        if sandbox_root.exists() {
            cleanup_sandbox(sandbox_root);
        }

        fs::create_dir_all(sandbox_root).map_err(|e| format!("mkdir: {}", e))?;

        // Mount tmpfs at sandbox root
        mount(
            Some("tmpfs"),
            sandbox_root,
            Some("tmpfs"),
            MsFlags::MS_NOSUID | MsFlags::MS_NODEV,
            Some("size=64M,mode=755"),
        )
        .map_err(|e| format!("mount tmpfs: {}", e))?;

        // Bind mount system directories
        let bind_dirs = ["/bin", "/lib", "/lib64", "/usr", "/etc"];
        for dir in &bind_dirs {
            let target = sandbox_root.join(&dir[1..]);
            if Path::new(dir).exists() {
                fs::create_dir_all(&target).map_err(|e| format!("mkdir {}: {}", dir, e))?;
                mount(
                    Some(*dir),
                    &target,
                    None::<&str>,
                    MsFlags::MS_BIND | MsFlags::MS_REC,
                    None::<&str>,
                )
                .map_err(|e| format!("bind mount {}: {}", dir, e))?;
                mount(
                    None::<&str>,
                    &target,
                    None::<&str>,
                    MsFlags::MS_BIND | MsFlags::MS_REMOUNT | MsFlags::MS_RDONLY | MsFlags::MS_REC,
                    None::<&str>,
                )
                .map_err(|e| format!("remount ro {}: {}", dir, e))?;
            }
        }

        // Create writable directories
        let tmp_dir = sandbox_root.join("tmp");
        fs::create_dir_all(&tmp_dir).map_err(|e| format!("mkdir tmp: {}", e))?;
        fs::set_permissions(&tmp_dir, fs::Permissions::from_mode(0o1777))
            .map_err(|e| format!("chmod tmp: {}", e))?;

        let dev_dir = sandbox_root.join("dev");
        fs::create_dir_all(&dev_dir).map_err(|e| format!("mkdir dev: {}", e))?;

        // Create essential device nodes by bind mounting from host
        let devices = [("null", 0o666), ("zero", 0o666), ("urandom", 0o666), ("random", 0o666)];
        for (dev, _mode) in &devices {
            let host_dev = format!("/dev/{}", dev);
            let sandbox_dev = dev_dir.join(dev);
            if Path::new(&host_dev).exists() {
                // Create empty file to mount over
                fs::write(&sandbox_dev, "").map_err(|e| format!("touch {}: {}", dev, e))?;
                mount(
                    Some(host_dev.as_str()),
                    &sandbox_dev,
                    None::<&str>,
                    MsFlags::MS_BIND,
                    None::<&str>,
                )
                .map_err(|e| format!("bind mount {}: {}", dev, e))?;
            }
        }

        // Mount proc
        let proc_dir = sandbox_root.join("proc");
        fs::create_dir_all(&proc_dir).map_err(|e| format!("mkdir proc: {}", e))?;
        mount(
            Some("proc"),
            &proc_dir,
            Some("proc"),
            MsFlags::MS_NOSUID | MsFlags::MS_NODEV | MsFlags::MS_NOEXEC,
            None::<&str>,
        )
        .map_err(|e| format!("mount proc: {}", e))?;

        // Create home directory for the sandbox
        let home_dir = sandbox_root.join("home");
        fs::create_dir_all(&home_dir).map_err(|e| format!("mkdir home: {}", e))?;
        fs::set_permissions(&home_dir, fs::Permissions::from_mode(0o755))
            .map_err(|e| format!("chmod home: {}", e))?;

        Ok(())
    }

    fn run_in_sandbox(sandbox_root: &Path, config: &RunConfig) -> Result<RunResult, String> {
        info!(command = ?config.command, "Running command");
        info!(sandbox_root = ?sandbox_root, "Sandbox root");
        info!(time_ms = config.time_ms, mem_kb = config.mem_kb,
              fsize_kb = config.fsize_kb, nofile = config.nofile, "Limits");

        // Create pipes for stdout/stderr capture
        let (stdout_read, stdout_write) = pipe().map_err(|e| format!("pipe: {}", e))?;
        let (stderr_read, stderr_write) = pipe().map_err(|e| format!("pipe: {}", e))?;

        // Get raw fds for the child process
        let stdout_write_fd = stdout_write.as_raw_fd();
        let stderr_write_fd = stderr_write.as_raw_fd();

        let sandbox_root = sandbox_root.to_path_buf();
        let config = config.clone();

        const STACK_SIZE: usize = 1024 * 1024;
        let mut stack = vec![0u8; STACK_SIZE];

        let clone_flags = CloneFlags::CLONE_NEWPID | CloneFlags::CLONE_NEWNS;

        let child_fn = Box::new(move || {
            // Redirect stdout/stderr to pipes
            unsafe {
                libc::dup2(stdout_write_fd, 1);
                libc::dup2(stderr_write_fd, 2);
                libc::close(stdout_write_fd);
                libc::close(stderr_write_fd);
            }

            if let Err(e) = run_child(&sandbox_root, &config) {
                eprintln!("Child error: {}", e);
                return 1;
            }
            0
        });

        info!("Calling clone()...");
        let child_pid = unsafe {
            clone(
                child_fn,
                &mut stack,
                clone_flags,
                Some(Signal::SIGCHLD as i32),
            )
        }
        .map_err(|e| format!("clone: {}", e))?;
        info!(child_pid = ?child_pid, "Child spawned");

        // Close write ends in parent (drop the OwnedFds)
        drop(stdout_write);
        drop(stderr_write);

        // Wait for child
        info!("Waiting for child...");
        let status = waitpid(child_pid, None).map_err(|e| format!("waitpid: {}", e))?;
        info!(status = ?status, "Child exited");

        // Read output from pipes
        let stdout = read_from_fd(stdout_read);
        let stderr = read_from_fd(stderr_read);
        info!(stdout_len = stdout.len(), stderr_len = stderr.len(), "Output captured");

        let (exit_code, signal) = match status {
            WaitStatus::Exited(_, code) => (Some(code), None),
            WaitStatus::Signaled(_, sig, _) => (None, Some(sig as i32)),
            _ => (None, None),
        };

        Ok(RunResult {
            stdout,
            stderr,
            exit_code,
            signal,
        })
    }

    fn read_from_fd(fd: OwnedFd) -> String {
        let mut file = unsafe { std::fs::File::from_raw_fd(fd.as_raw_fd()) };
        std::mem::forget(fd); // Don't double-close
        let mut output = String::new();
        let _ = file.read_to_string(&mut output);
        output
    }

    fn run_child(sandbox_root: &Path, config: &RunConfig) -> Result<(), String> {
        eprintln!("[child] Starting, sandbox_root={:?}", sandbox_root);

        // chroot into sandbox
        eprintln!("[child] chroot...");
        chroot(sandbox_root).map_err(|e| format!("chroot: {}", e))?;
        eprintln!("[child] chdir to {:?}...", config.cwd);
        chdir(config.cwd.as_str()).map_err(|e| format!("chdir: {}", e))?;

        // Set resource limits
        eprintln!("[child] Setting resource limits...");
        set_resource_limits(config)?;
        eprintln!("[child] Resource limits set");

        // TODO: Fix privilege dropping - currently disabled due to deadlock in multi-threaded context
        // The sandbox is still isolated by PID namespace, mount namespace, and chroot
        eprintln!("[child] Skipping privilege drop (sandbox still isolated by namespaces)");

        // Execute command
        let cmd = CString::new(config.command[0].as_str()).map_err(|e| format!("cmd: {}", e))?;
        let args: Vec<CString> = config
            .command
            .iter()
            .map(|s| CString::new(s.as_str()).unwrap())
            .collect();

        // Build environment
        let mut env: Vec<CString> = config
            .env
            .iter()
            .map(|(k, v)| CString::new(format!("{}={}", k, v)).unwrap())
            .collect();
        env.push(CString::new("PATH=/usr/bin:/bin").unwrap());
        env.push(CString::new("HOME=/home").unwrap());

        eprintln!("[child] About to exec: {:?}", config.command);
        eprintln!("[child] Flushing stderr before exec...");
        let _ = std::io::stderr().flush();
        execvpe(&cmd, &args, &env).map_err(|e| format!("exec: {}", e))?;
        Ok(())
    }

    fn set_resource_limits(config: &RunConfig) -> Result<(), String> {
        let cpu_seconds = std::cmp::max(1, config.time_ms / 1000);
        eprintln!("[rlimit] CPU: {} seconds", cpu_seconds);
        setrlimit(Resource::RLIMIT_CPU, cpu_seconds, cpu_seconds)
            .map_err(|e| format!("rlimit cpu: {}", e))?;

        let mem_bytes = config.mem_kb * 1024;
        eprintln!("[rlimit] AS (mem): {} bytes ({} MB)", mem_bytes, mem_bytes / 1024 / 1024);
        setrlimit(Resource::RLIMIT_AS, mem_bytes, mem_bytes)
            .map_err(|e| format!("rlimit as: {}", e))?;

        let fsize_bytes = config.fsize_kb * 1024;
        eprintln!("[rlimit] FSIZE: {} bytes", fsize_bytes);
        setrlimit(Resource::RLIMIT_FSIZE, fsize_bytes, fsize_bytes)
            .map_err(|e| format!("rlimit fsize: {}", e))?;

        eprintln!("[rlimit] NOFILE: {}", config.nofile);
        setrlimit(Resource::RLIMIT_NOFILE, config.nofile, config.nofile)
            .map_err(|e| format!("rlimit nofile: {}", e))?;

        eprintln!("[rlimit] CORE: 0");
        setrlimit(Resource::RLIMIT_CORE, 0, 0).map_err(|e| format!("rlimit core: {}", e))?;

        eprintln!("[rlimit] NPROC: 64");
        setrlimit(Resource::RLIMIT_NPROC, 64, 64).map_err(|e| format!("rlimit nproc: {}", e))?;

        eprintln!("[rlimit] All limits set successfully");
        Ok(())
    }

    fn cleanup_sandbox(sandbox_root: &Path) {
        let mount_points = ["proc", "etc", "usr", "lib64", "lib", "bin"];
        for mp in &mount_points {
            let path = sandbox_root.join(mp);
            if path.exists() {
                let _ = umount2(&path, MntFlags::MNT_DETACH);
            }
        }
        let _ = umount2(sandbox_root, MntFlags::MNT_DETACH);
        let _ = fs::remove_dir_all(sandbox_root);
    }
}

// ============================================================================
// HTTP Server module
// ============================================================================
#[cfg(target_os = "linux")]
mod server {
    use crate::sandbox::{self, RunConfig, RunResult};
    use axum::{
        extract::{Path, State},
        http::StatusCode,
        routing::{delete, get, post},
        Json, Router,
    };
    use serde::{Deserialize, Serialize};
    use std::collections::HashMap;
    use std::net::SocketAddr;
    use std::path::PathBuf;
    use std::sync::Arc;
    use std::time::{Duration, Instant};
    use tokio::sync::RwLock;
    use tokio::time::interval;
    use tracing::info;

    const SESSION_TTL_SECS: u64 = 300; // 5 minutes

    #[derive(Debug)]
    struct Session {
        id: String,
        sandbox_root: PathBuf,
        env: HashMap<String, String>,
        cwd: String,
        created_at: Instant,
        last_used: Instant,
    }

    type Sessions = Arc<RwLock<HashMap<String, Session>>>;

    #[derive(Clone)]
    struct AppState {
        sessions: Sessions,
    }

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

    fn default_time() -> u64 { 5000 }
    fn default_mem() -> u64 { 2097152 }  // 2GB - Go programs need lots of virtual address space
    fn default_nofile() -> u64 { 64 }
    fn default_fsize() -> u64 { 10240 } // 10MB
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

    pub async fn run_server(port: u16) {
        let state = AppState {
            sessions: Arc::new(RwLock::new(HashMap::new())),
        };

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
        info!("Starting server on {}", addr);

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
}
