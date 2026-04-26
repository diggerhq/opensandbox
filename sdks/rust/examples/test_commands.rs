//! Command Edge Cases Test
//!
//! Tests:
//!   1. Basic commands
//!   2. stderr handling
//!   3. Non-zero exit codes
//!   4. Large stdout output
//!   5. Environment variable passing
//!   6. Working directory
//!   7. Shell features (pipes, redirects, subshells)
//!   8. Concurrent commands on same sandbox
//!   9. Command timeout
//!
//! Usage:
//!   cargo run --example test_commands

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Instant;

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

#[tokio::main]
async fn main() {
    bold("\n╔══════════════════════════════════════════════════╗");
    bold("║       Command Edge Cases Test                    ║");
    bold("╚══════════════════════════════════════════════════╝\n");

    let sandbox = match Sandbox::create(SandboxOpts::new().template("base").timeout(120)).await {
        Ok(s) => Arc::new(s),
        Err(e) => {
            red(&format!("Fatal error: {}", e));
            FAILED.fetch_add(1, Ordering::SeqCst);
            print_summary();
            return;
        }
    };
    green(&format!("Created sandbox: {}", sandbox.sandbox_id()));
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

async fn run_tests(sandbox: &Arc<Sandbox>) -> opencomputer::Result<()> {
    // ── Test 1: Basic commands ──
    bold("━━━ Test 1: Basic commands ━━━\n");

    let echo = sandbox
        .commands()
        .run("echo hello-world", RunOpts::new())
        .await?;
    check(
        "Echo returns correct output",
        echo.stdout.trim() == "hello-world",
        "",
    );
    check("Echo exit code is 0", echo.exit_code == 0, "");

    let multi = sandbox
        .commands()
        .run("echo line1 && echo line2 && echo line3", RunOpts::new())
        .await?;
    let lines: Vec<&str> = multi.stdout.trim().split('\n').collect();
    check("Multi-command outputs 3 lines", lines.len() == 3, "");
    check(
        "Multi-command content correct",
        lines.first() == Some(&"line1") && lines.get(2) == Some(&"line3"),
        "",
    );
    println!();

    // ── Test 2: stderr handling ──
    bold("━━━ Test 2: stderr handling ━━━\n");

    let stderr_cmd = sandbox
        .commands()
        .run("echo error-msg >&2", RunOpts::new())
        .await?;
    check(
        "stderr captured",
        stderr_cmd.stderr.trim() == "error-msg",
        "",
    );
    check(
        "stdout empty when writing to stderr",
        stderr_cmd.stdout.trim().is_empty(),
        "",
    );
    check(
        "Exit code 0 even with stderr",
        stderr_cmd.exit_code == 0,
        "",
    );

    let mixed = sandbox
        .commands()
        .run("echo stdout-data && echo stderr-data >&2", RunOpts::new())
        .await?;
    check(
        "Mixed: stdout captured",
        mixed.stdout.contains("stdout-data"),
        "",
    );
    check(
        "Mixed: stderr captured",
        mixed.stderr.contains("stderr-data"),
        "",
    );
    println!();

    // ── Test 3: Non-zero exit codes ──
    bold("━━━ Test 3: Non-zero exit codes ━━━\n");

    let exit1 = sandbox.commands().run("exit 1", RunOpts::new()).await?;
    check(
        "Exit code 1 captured",
        exit1.exit_code == 1,
        &format!("got {}", exit1.exit_code),
    );

    let exit42 = sandbox.commands().run("exit 42", RunOpts::new()).await?;
    check(
        "Exit code 42 captured",
        exit42.exit_code == 42,
        &format!("got {}", exit42.exit_code),
    );

    let false_cmd = sandbox.commands().run("false", RunOpts::new()).await?;
    check(
        "'false' returns exit code 1",
        false_cmd.exit_code == 1,
        &format!("got {}", false_cmd.exit_code),
    );

    let not_found = sandbox
        .commands()
        .run("nonexistent-command-xyz 2>&1 || true", RunOpts::new())
        .await?;
    check("Non-existent command handled", not_found.exit_code == 0, "");
    println!();

    // ── Test 4: Large stdout ──
    bold("━━━ Test 4: Large stdout output ━━━\n");

    let large_out = sandbox
        .commands()
        .run("seq 1 10000", RunOpts::new())
        .await?;
    let line_count = large_out.stdout.trim().split('\n').count();
    check(
        "10000 lines of output captured",
        line_count == 10000,
        &format!("got {} lines", line_count),
    );
    dim(&format!("Output size: {} chars", large_out.stdout.len()));

    let large_lines: Vec<&str> = large_out.stdout.trim().split('\n').collect();
    check("First line is 1", large_lines.first() == Some(&"1"), "");
    check(
        "Last line is 10000",
        large_lines.last() == Some(&"10000"),
        "",
    );
    println!();

    // ── Test 5: Environment variables ──
    bold("━━━ Test 5: Environment variable passing ━━━\n");

    let env_result = sandbox
        .commands()
        .run(
            "echo $MY_VAR",
            RunOpts::new().env("MY_VAR", "secret-value-123"),
        )
        .await?;
    check(
        "Env var passed correctly",
        env_result.stdout.trim() == "secret-value-123",
        "",
    );

    let multi_env = sandbox
        .commands()
        .run(
            "echo \"$A:$B:$C\"",
            RunOpts::new()
                .env("A", "alpha")
                .env("B", "beta")
                .env("C", "gamma"),
        )
        .await?;
    check(
        "Multiple env vars",
        multi_env.stdout.trim() == "alpha:beta:gamma",
        "",
    );

    let special_env = sandbox
        .commands()
        .run(
            "echo $SPECIAL",
            RunOpts::new().env("SPECIAL", "hello world with spaces & stuff"),
        )
        .await?;
    check(
        "Env var with special chars",
        special_env.stdout.trim() == "hello world with spaces & stuff",
        "",
    );
    println!();

    // ── Test 6: Working directory ──
    bold("━━━ Test 6: Working directory ━━━\n");

    sandbox
        .commands()
        .run("mkdir -p /tmp/workdir/sub", RunOpts::new())
        .await?;
    sandbox
        .files()
        .write("/tmp/workdir/sub/data.txt", "found-it")
        .await?;

    let cwd_result = sandbox
        .commands()
        .run("cat data.txt", RunOpts::new().cwd("/tmp/workdir/sub"))
        .await?;
    check(
        "Working directory respected",
        cwd_result.stdout.trim() == "found-it",
        "",
    );

    let pwd_result = sandbox
        .commands()
        .run("pwd", RunOpts::new().cwd("/tmp/workdir"))
        .await?;
    check(
        "pwd reflects cwd",
        pwd_result.stdout.trim() == "/tmp/workdir",
        "",
    );
    println!();

    // ── Test 7: Shell features ──
    bold("━━━ Test 7: Shell features (pipes, redirects, subshells) ━━━\n");

    let pipe_result = sandbox
        .commands()
        .run("echo 'hello world' | tr ' ' '-'", RunOpts::new())
        .await?;
    check("Pipe works", pipe_result.stdout.trim() == "hello-world", "");

    let subshell = sandbox
        .commands()
        .run("echo $(hostname)", RunOpts::new())
        .await?;
    check(
        "Command substitution works",
        !subshell.stdout.trim().is_empty(),
        subshell.stdout.trim(),
    );

    sandbox
        .commands()
        .run("echo redirect-test > /tmp/redirect.txt", RunOpts::new())
        .await?;
    let redirect_content = sandbox.files().read("/tmp/redirect.txt").await?;
    check(
        "Redirect to file works",
        redirect_content.trim() == "redirect-test",
        "",
    );

    sandbox
        .commands()
        .run(
            "touch /tmp/wc-a.txt /tmp/wc-b.txt /tmp/wc-c.txt",
            RunOpts::new(),
        )
        .await?;
    let wc_result = sandbox
        .commands()
        .run("ls /tmp/wc-*.txt | wc -l", RunOpts::new())
        .await?;
    check(
        "Wildcard expansion works",
        wc_result.stdout.trim() == "3",
        "",
    );

    let arith = sandbox
        .commands()
        .run("echo $((42 * 7))", RunOpts::new())
        .await?;
    check(
        "Arithmetic expansion works",
        arith.stdout.trim() == "294",
        "",
    );

    let here_str = sandbox
        .commands()
        .run("bash -c \"cat <<< 'here-string-data'\"", RunOpts::new())
        .await?;
    check(
        "Here string works",
        here_str.stdout.trim() == "here-string-data",
        "",
    );
    println!();

    // ── Test 8: Concurrent commands ──
    bold("━━━ Test 8: Concurrent commands on same sandbox ━━━\n");

    let concurrent_start = Instant::now();
    let mut handles = Vec::new();
    for i in 0..10 {
        let sb = sandbox.clone();
        handles.push(tokio::spawn(async move {
            let r = sb
                .commands()
                .run(&format!("echo concurrent-{}", i), RunOpts::new())
                .await;
            (i, r)
        }));
    }

    let mut all_correct = true;
    for h in handles {
        let (index, result) = match h.await {
            Ok(v) => v,
            Err(_) => {
                all_correct = false;
                continue;
            }
        };
        match result {
            Ok(r) => {
                if r.stdout.trim() != format!("concurrent-{}", index) || r.exit_code != 0 {
                    all_correct = false;
                    dim(&format!(
                        "Command {}: expected \"concurrent-{}\", got \"{}\" (exit {})",
                        index,
                        index,
                        r.stdout.trim(),
                        r.exit_code
                    ));
                }
            }
            Err(_) => all_correct = false,
        }
    }
    let concurrent_ms = concurrent_start.elapsed().as_millis();

    check(
        "10 concurrent commands all returned correctly",
        all_correct,
        "",
    );
    dim(&format!("Total concurrent time: {}ms", concurrent_ms));
    println!();

    // ── Test 9: Command timeout ──
    bold("━━━ Test 9: Command timeout ━━━\n");

    let timeout_start = Instant::now();
    let _ = sandbox
        .commands()
        .run("sleep 30", RunOpts::new().timeout(3))
        .await;
    let timeout_ms = timeout_start.elapsed().as_millis();
    check(
        "Command timed out within ~3s",
        timeout_ms < 10_000,
        &format!("took {}ms", timeout_ms),
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
