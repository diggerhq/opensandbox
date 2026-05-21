//! Python SDK Production Test (Rust port)
//!
//! Validates that the Python template works end-to-end by running a Python
//! test script inside a sandbox that exercises stdlib, file ops, env vars, etc.
//!
//! Usage:
//!   cargo run --example test_python_sdk

use std::sync::atomic::{AtomicUsize, Ordering};

use opencomputer::{RunOpts, Sandbox, SandboxOpts};

static PASSED: AtomicUsize = AtomicUsize::new(0);
static FAILED: AtomicUsize = AtomicUsize::new(0);

fn green(msg: &str) {
    println!("\x1b[32m✓ {}\x1b[0m", msg);
}
fn red(msg: &str) {
    println!("\x1b[31m✗ {}\x1b[0m", msg);
}
fn bold(msg: &str) {
    println!("\x1b[1m{}\x1b[0m", msg);
}
fn dim(msg: &str) {
    println!("\x1b[2m  {}\x1b[0m", msg);
}

fn check(desc: &str, condition: bool, detail: &str) {
    if condition {
        green(desc);
        PASSED.fetch_add(1, Ordering::SeqCst);
    } else if detail.is_empty() {
        red(desc);
        FAILED.fetch_add(1, Ordering::SeqCst);
    } else {
        red(&format!("{} ({})", desc, detail));
        FAILED.fetch_add(1, Ordering::SeqCst);
    }
}

const PYTHON_TEST_SCRIPT: &str = r#"
import json
import os

results = {}

# Test 1: Basic echo
import subprocess
r = subprocess.run(["echo", "hello-from-python"], capture_output=True, text=True)
results["echo"] = r.stdout.strip()

# Test 2: File write + read
with open("/tmp/py-test.txt", "w") as f:
    f.write("python-sdk-data")
with open("/tmp/py-test.txt", "r") as f:
    results["file_content"] = f.read()

# Test 3: Environment variables
results["home"] = os.environ.get("HOME", "unknown")
results["path_exists"] = "PATH" in os.environ

# Test 4: Nested directory
os.makedirs("/tmp/py-nested/deep/dir", exist_ok=True)
with open("/tmp/py-nested/deep/dir/file.txt", "w") as f:
    f.write("nested-content")
with open("/tmp/py-nested/deep/dir/file.txt", "r") as f:
    results["nested"] = f.read()

# Test 5: Python-specific features
import sys
results["python_version"] = sys.version.split()[0]
results["platform"] = sys.platform

# Test 6: Math/stdlib
import math
results["pi"] = str(round(math.pi, 5))

# Test 7: JSON handling
data = {"key": "value", "number": 42, "nested": {"a": True}}
results["json_roundtrip"] = json.loads(json.dumps(data)) == data

print(json.dumps(results))
"#;

#[tokio::main]
async fn main() {
    bold("\n╔══════════════════════════════════════════════════╗");
    bold("║       Python SDK Production Test                 ║");
    bold("╚══════════════════════════════════════════════════╝\n");

    bold("[1/4] Creating Python sandbox...");
    let sandbox = match Sandbox::create(SandboxOpts::new().template("python").timeout(120)).await {
        Ok(s) => s,
        Err(e) => {
            red(&format!("Fatal error: {}", e));
            FAILED.fetch_add(1, Ordering::SeqCst);
            print_summary();
            return;
        }
    };
    green(&format!("Created: {}", sandbox.sandbox_id()));
    dim(&format!("Domain: {}", sandbox.domain()));
    println!();

    if let Err(e) = run_tests(&sandbox).await {
        red(&format!("Fatal error: {}", e));
        FAILED.fetch_add(1, Ordering::SeqCst);
    }

    if let Err(e) = sandbox.kill().await {
        red(&format!("Failed to kill sandbox: {}", e));
    } else {
        green("Sandbox killed");
    }
    print_summary();
}

async fn run_tests(sandbox: &Sandbox) -> opencomputer::Result<()> {
    // --- Write and run Python test script ---
    bold("[2/4] Writing Python test script...");
    sandbox
        .files()
        .write("/tmp/test_sdk.py", PYTHON_TEST_SCRIPT)
        .await?;
    green("Script written to /tmp/test_sdk.py");
    println!();

    bold("[3/4] Running Python tests inside sandbox...");
    let result = sandbox
        .commands()
        .run("python3 /tmp/test_sdk.py", RunOpts::new().timeout(30))
        .await?;
    check(
        "Python script exited cleanly",
        result.exit_code == 0,
        &format!("exit code: {}", result.exit_code),
    );

    if result.exit_code != 0 {
        dim(&format!("stderr: {}", result.stderr));
        dim(&format!("stdout: {}", result.stdout));
    } else {
        let data: serde_json::Value =
            serde_json::from_str(result.stdout.trim()).unwrap_or_default();

        let s = |k: &str| {
            data.get(k)
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .to_string()
        };
        let b = |k: &str| data.get(k).and_then(|v| v.as_bool()).unwrap_or(false);

        check(
            "Echo command works",
            s("echo") == "hello-from-python",
            &s("echo"),
        );
        check(
            "File write/read works",
            s("file_content") == "python-sdk-data",
            &s("file_content"),
        );
        check("HOME env var present", s("home") != "unknown", &s("home"));
        check("PATH env var exists", b("path_exists"), "");
        check(
            "Nested directory file works",
            s("nested") == "nested-content",
            &s("nested"),
        );
        check(
            "Python version detected",
            s("python_version").starts_with("3."),
            &s("python_version"),
        );
        check(
            "Platform is Linux",
            s("platform") == "linux",
            &s("platform"),
        );
        check("Math.pi correct", s("pi") == "3.14159", &s("pi"));
        check("JSON roundtrip works", b("json_roundtrip"), "");
        dim(&format!(
            "Python {} on {}",
            s("python_version"),
            s("platform")
        ));
    }
    println!();

    // --- Verify file ops from SDK side ---
    bold("[4/4] Verifying files from Rust SDK...");
    let content = sandbox.files().read("/tmp/py-test.txt").await?;
    check(
        "SDK can read Python-written file",
        content == "python-sdk-data",
        &content,
    );

    let entries = sandbox.files().list("/tmp/py-nested/deep/dir").await?;
    check(
        "SDK can list Python-created directory",
        entries.iter().any(|e| e.name == "file.txt"),
        "",
    );
    println!();

    Ok(())
}

fn print_summary() {
    bold("========================================");
    bold(&format!(
        " Results: {} passed, {} failed",
        PASSED.load(Ordering::SeqCst),
        FAILED.load(Ordering::SeqCst)
    ));
    bold("========================================\n");
    if FAILED.load(Ordering::SeqCst) > 0 {
        std::process::exit(1);
    }
}
