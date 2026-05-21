//! File Operations Edge Cases Test
//!
//! Tests:
//!   1. Large file write/read (1MB)
//!   2. Special characters in content
//!   3. Deeply nested directories
//!   4. File deletion and overwrite
//!   5. Large directory listing
//!   6. Empty file handling
//!   7. File exists / not exists
//!   8. Write via commands + read via SDK
//!
//! Usage:
//!   cargo run --example test_file_ops

use std::sync::atomic::{AtomicUsize, Ordering};
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
    bold("║       File Operations Edge Cases Test            ║");
    bold("╚══════════════════════════════════════════════════╝\n");

    let sandbox = match Sandbox::create(SandboxOpts::new().template("base").timeout(120)).await {
        Ok(s) => s,
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

async fn run_tests(sandbox: &Sandbox) -> opencomputer::Result<()> {
    // ── Test 1: Large file ──
    bold("━━━ Test 1: Large file (1MB) ━━━\n");

    let one_mb: String = "X".repeat(1024 * 1024);
    let write_start = Instant::now();
    sandbox
        .files()
        .write("/tmp/large.txt", one_mb.clone())
        .await?;
    dim(&format!("Write: {}ms", write_start.elapsed().as_millis()));

    let read_start = Instant::now();
    let large_content = sandbox.files().read("/tmp/large.txt").await?;
    dim(&format!("Read: {}ms", read_start.elapsed().as_millis()));

    check(
        "1MB file size preserved",
        large_content.len() == one_mb.len(),
        &format!("{} bytes", large_content.len()),
    );
    check("1MB file content intact", large_content == one_mb, "");
    println!();

    // ── Test 2: Special characters ──
    bold("━━━ Test 2: Special characters ━━━\n");

    let special_content =
        "Hello \"world\" & <tag> 'quotes' \\ newline\nTab\there 日本語 emoji🎉 nullish ?? chain?.";
    sandbox
        .files()
        .write("/tmp/special.txt", special_content)
        .await?;
    let special_read = sandbox.files().read("/tmp/special.txt").await?;
    check(
        "Special characters preserved",
        special_read == special_content,
        &format!(
            "got: {}...",
            &special_read.chars().take(50).collect::<String>()
        ),
    );

    let json_content = serde_json::to_string_pretty(&serde_json::json!({
        "key": "value",
        "nested": { "arr": [1, 2, 3] },
        "unicode": "日本語"
    }))
    .unwrap();
    sandbox
        .files()
        .write("/tmp/data.json", json_content.clone())
        .await?;
    let json_read = sandbox.files().read("/tmp/data.json").await?;
    check("JSON content preserved", json_read == json_content, "");

    let multiline: String = (0..100)
        .map(|i| format!("Line {}: Some content here", i + 1))
        .collect::<Vec<_>>()
        .join("\n");
    sandbox
        .files()
        .write("/tmp/multiline.txt", multiline.clone())
        .await?;
    let multi_read = sandbox.files().read("/tmp/multiline.txt").await?;
    check(
        "100-line file preserved",
        multi_read == multiline,
        &format!("lines: {}", multi_read.split('\n').count()),
    );
    println!();

    // ── Test 3: Deeply nested directories ──
    bold("━━━ Test 3: Deeply nested directories ━━━\n");

    let deep_path = "/tmp/a/b/c/d/e/f/g/h";
    sandbox
        .commands()
        .run(&format!("mkdir -p {}", deep_path), RunOpts::new())
        .await?;
    sandbox
        .files()
        .write(&format!("{}/deep.txt", deep_path), "bottom-of-tree")
        .await?;
    let deep_content = sandbox
        .files()
        .read(&format!("{}/deep.txt", deep_path))
        .await?;
    check(
        "8-level nested file created and read",
        deep_content == "bottom-of-tree",
        "",
    );

    let mid_entries = sandbox.files().list("/tmp/a/b/c/d").await?;
    check(
        "Intermediate dir lists correctly",
        mid_entries.iter().any(|e| e.name == "e" && e.is_dir),
        "",
    );
    println!();

    // ── Test 4: File deletion and overwrite ──
    bold("━━━ Test 4: File deletion and overwrite ━━━\n");

    sandbox
        .files()
        .write("/tmp/overwrite.txt", "original")
        .await?;
    let mut content = sandbox.files().read("/tmp/overwrite.txt").await?;
    check("Original content written", content == "original", "");

    sandbox
        .files()
        .write("/tmp/overwrite.txt", "overwritten")
        .await?;
    content = sandbox.files().read("/tmp/overwrite.txt").await?;
    check("Overwritten content correct", content == "overwritten", "");

    sandbox.files().write("/tmp/overwrite.txt", "short").await?;
    content = sandbox.files().read("/tmp/overwrite.txt").await?;
    check(
        "Shorter overwrite correct (no trailing data)",
        content == "short",
        "",
    );

    let exists_before = sandbox.files().exists("/tmp/overwrite.txt").await;
    check("File exists before delete", exists_before, "");

    sandbox.files().remove("/tmp/overwrite.txt").await?;
    let exists_after = sandbox.files().exists("/tmp/overwrite.txt").await;
    check("File gone after delete", !exists_after, "");

    sandbox.files().remove("/tmp/a").await?;
    let dir_gone = sandbox
        .files()
        .exists(&format!("{}/deep.txt", deep_path))
        .await;
    check("Recursive directory deletion", !dir_gone, "");
    println!();

    // ── Test 5: Large directory listing ──
    bold("━━━ Test 5: Large directory listing ━━━\n");

    sandbox
        .commands()
        .run(
            "for i in $(seq 1 50); do echo content-$i > /tmp/listtest-$i.txt; done",
            RunOpts::new(),
        )
        .await?;
    let entries = sandbox.files().list("/tmp").await?;
    let list_test_files: Vec<_> = entries
        .iter()
        .filter(|e| e.name.starts_with("listtest-"))
        .collect();
    check(
        "50 files visible in listing",
        list_test_files.len() == 50,
        &format!("found {}", list_test_files.len()),
    );

    if let Some(entry) = list_test_files.first() {
        check("Entry has name", !entry.name.is_empty(), "");
        check("Entry has is_dir=false", !entry.is_dir, "");
        check(
            "Entry has size > 0",
            entry.size > 0,
            &format!("size={}", entry.size),
        );
    }
    println!();

    // ── Test 6: Empty file ──
    bold("━━━ Test 6: Empty file handling ━━━\n");

    sandbox.files().write("/tmp/empty.txt", "").await?;
    let empty_content = sandbox.files().read("/tmp/empty.txt").await?;
    check(
        "Empty file returns empty string",
        empty_content.is_empty(),
        &format!("got: \"{}\"", empty_content),
    );
    check(
        "Empty file exists",
        sandbox.files().exists("/tmp/empty.txt").await,
        "",
    );
    println!();

    // ── Test 7: File exists checks ──
    bold("━━━ Test 7: File exists checks ━━━\n");

    check(
        "Existing file → true",
        sandbox.files().exists("/tmp/special.txt").await,
        "",
    );
    check(
        "Non-existent file → false",
        !sandbox.files().exists("/tmp/nope-no-way.txt").await,
        "",
    );
    check(
        "Non-existent deep path → false",
        !sandbox.files().exists("/tmp/no/such/path/file.txt").await,
        "",
    );
    println!();

    // ── Test 8: Write via commands + read via SDK ──
    bold("━━━ Test 8: Write via commands + read via SDK ━━━\n");

    sandbox
        .commands()
        .run(
            "dd if=/dev/urandom bs=256 count=1 2>/dev/null | base64 > /tmp/random.b64",
            RunOpts::new(),
        )
        .await?;
    let b64_content = sandbox.files().read("/tmp/random.b64").await?;
    check(
        "Base64 random data readable",
        b64_content.len() > 100,
        &format!("{} chars", b64_content.len()),
    );

    sandbox
        .commands()
        .run(
            "echo -n \"command-written\" > /tmp/cmd-file.txt",
            RunOpts::new(),
        )
        .await?;
    let cmd_file_content = sandbox.files().read("/tmp/cmd-file.txt").await?;
    check(
        "Command-written file readable via SDK",
        cmd_file_content == "command-written",
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
