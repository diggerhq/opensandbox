//! Compilation and offline smoke tests.
//!
//! These tests do not hit the network — they verify that the SDK's public
//! types, builders, and url-resolution logic behave as expected. The full
//! integration tests live in `examples/` and require a real backend.

use opencomputer::{ExecSessionInfo, ExecStartOpts, RunOpts, SandboxOpts, StreamEvent};

#[test]
fn sandbox_opts_builder_threads_values() {
    let opts = SandboxOpts::new()
        .template("base")
        .timeout(120)
        .api_key("osb_test")
        .api_url("https://example.com")
        .env("FOO", "bar")
        .cpu_count(2)
        .memory_mb(2048)
        .disk_mb(20480);

    assert_eq!(opts.template.as_deref(), Some("base"));
    assert_eq!(opts.timeout, Some(120));
    assert_eq!(opts.api_key.as_deref(), Some("osb_test"));
    assert_eq!(opts.api_url.as_deref(), Some("https://example.com"));
    assert_eq!(opts.cpu_count, Some(2));
    assert_eq!(opts.memory_mb, Some(2048));
    assert_eq!(opts.disk_mb, Some(20480));
    assert_eq!(
        opts.envs.unwrap().get("FOO").map(String::as_str),
        Some("bar")
    );
}

#[test]
fn run_opts_builder_threads_values() {
    let opts = RunOpts::new()
        .timeout(45)
        .env("A", "1")
        .env("B", "2")
        .cwd("/work");

    assert_eq!(opts.timeout, Some(45));
    assert_eq!(opts.cwd.as_deref(), Some("/work"));
    let env = opts.env.unwrap();
    assert_eq!(env.get("A").map(String::as_str), Some("1"));
    assert_eq!(env.get("B").map(String::as_str), Some("2"));
}

#[test]
fn exec_start_opts_and_stream_event_are_public() {
    // Compile-time proof that the public surface advertised in the docs
    // (and used by the streaming examples) is actually exported.
    let _ = ExecStartOpts::new()
        .args(vec!["-c".into(), "echo hi".into()])
        .env("FOO", "bar")
        .cwd("/tmp")
        .timeout(10);

    fn classify(e: StreamEvent) -> &'static str {
        match e {
            StreamEvent::Stdout(_) => "stdout",
            StreamEvent::Stderr(_) => "stderr",
            StreamEvent::ScrollbackEnd => "scrollback_end",
            StreamEvent::Exit(_) => "exit",
        }
    }
    assert_eq!(classify(StreamEvent::Stdout(vec![1, 2, 3])), "stdout");
    assert_eq!(classify(StreamEvent::Exit(42)), "exit");
}

#[test]
fn exec_session_info_deserializes_camelcase() {
    let json = r#"{
        "sessionID": "sess_123",
        "sandboxID": "sb_abc",
        "command": "bash",
        "args": ["-c", "echo hi"],
        "running": true,
        "exitCode": null,
        "startedAt": "2025-01-01T00:00:00Z",
        "attachedClients": 2
    }"#;

    let info: ExecSessionInfo = serde_json::from_str(json).expect("deserialize");
    assert_eq!(info.session_id, "sess_123");
    assert_eq!(info.sandbox_id, "sb_abc");
    assert_eq!(info.command, "bash");
    assert_eq!(info.args, vec!["-c", "echo hi"]);
    assert!(info.running);
    assert_eq!(info.exit_code, None);
    assert_eq!(info.attached_clients, 2);
}
