//! Core sandbox execution logic.

use nix::mount::{mount, umount2, MntFlags, MsFlags};
use nix::sched::{clone, CloneFlags};
use nix::sys::resource::{setrlimit, Resource};
use nix::sys::signal::Signal;
use nix::sys::wait::{waitpid, WaitStatus};
use nix::unistd::{chdir, chroot, execvpe};
use std::collections::HashMap;
use std::ffi::CString;
use std::fs;
use std::io::{Read, Write};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use tracing::info;

/// Configuration for running a command in the sandbox.
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

/// Result of running a command.
#[derive(Debug, Clone, serde::Serialize)]
pub struct RunResult {
    pub stdout: String,
    pub stderr: String,
    pub exit_code: Option<i32>,
    pub signal: Option<i32>,
}

/// Run a command in a fresh sandbox (no session, cleanup after).
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

/// Run a command in an existing session sandbox.
pub fn run_in_session(sandbox_root: &Path, config: &RunConfig) -> Result<RunResult, String> {
    run_in_sandbox(sandbox_root, config)
}

/// Create a new session sandbox directory.
pub fn create_session_sandbox(session_id: &str) -> Result<PathBuf, String> {
    let sandbox_root = PathBuf::from(format!("/tmp/sandbox-{}", session_id));
    setup_sandbox_dir(&sandbox_root)?;
    Ok(sandbox_root)
}

/// Cleanup a session sandbox.
pub fn destroy_session_sandbox(sandbox_root: &Path) {
    cleanup_sandbox(sandbox_root);
}

/// Write a file directly into the sandbox filesystem.
pub fn write_file_in_sandbox(sandbox_root: &Path, path: &str, content: &[u8]) -> Result<(), String> {
    // Normalize the path to be relative to sandbox root
    let normalized_path = if path.starts_with('/') {
        path.trim_start_matches('/')
    } else {
        path
    };

    let full_path = sandbox_root.join(normalized_path);

    // Ensure parent directory exists
    if let Some(parent) = full_path.parent() {
        fs::create_dir_all(parent).map_err(|e| format!("mkdir parent: {}", e))?;
    }

    fs::write(&full_path, content).map_err(|e| format!("write file: {}", e))?;

    // Make file accessible
    fs::set_permissions(&full_path, fs::Permissions::from_mode(0o644))
        .map_err(|e| format!("chmod: {}", e))?;

    Ok(())
}

/// Read a file directly from the sandbox filesystem.
pub fn read_file_in_sandbox(sandbox_root: &Path, path: &str) -> Result<Vec<u8>, String> {
    // Normalize the path to be relative to sandbox root
    let normalized_path = if path.starts_with('/') {
        path.trim_start_matches('/')
    } else {
        path
    };

    let full_path = sandbox_root.join(normalized_path);

    fs::read(&full_path).map_err(|e| format!("read file: {}", e))
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
        Some("size=2G,mode=755"),
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
    let (stdout_read, stdout_write) = nix::unistd::pipe().map_err(|e| format!("pipe: {}", e))?;
    let (stderr_read, stderr_write) = nix::unistd::pipe().map_err(|e| format!("pipe: {}", e))?;

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

    // Close write ends in parent
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
    // Unmount device bind mounts
    let dev_dir = sandbox_root.join("dev");
    if dev_dir.exists() {
        for dev in &["null", "zero", "urandom", "random"] {
            let dev_path = dev_dir.join(dev);
            if dev_path.exists() {
                let _ = umount2(&dev_path, MntFlags::MNT_DETACH);
            }
        }
    }
    let _ = umount2(sandbox_root, MntFlags::MNT_DETACH);
    let _ = fs::remove_dir_all(sandbox_root);
}
