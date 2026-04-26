//! Rust SDK for [OpenComputer](https://github.com/diggerhq/opencomputer) — cloud sandbox platform.
//!
//! ```no_run
//! use opencomputer::{Sandbox, SandboxOpts};
//!
//! # async fn run() -> opencomputer::Result<()> {
//! let sandbox = Sandbox::create(SandboxOpts::new().template("base")).await?;
//!
//! let result = sandbox.commands().run("echo hello").await?;
//! println!("{}", result.stdout); // "hello\n"
//!
//! sandbox.files().write("/tmp/test.txt", "Hello, world!").await?;
//! let content = sandbox.files().read("/tmp/test.txt").await?;
//!
//! sandbox.kill().await?;
//! # Ok(())
//! # }
//! ```

mod error;
mod exec;
mod filesystem;
mod sandbox;
mod template;

pub use error::{Error, Result};
pub use exec::{Exec, ExecSessionInfo, ProcessResult, RunOpts};
pub use filesystem::{EntryInfo, Filesystem};
pub use sandbox::{CheckpointInfo, PatchInfo, PatchResult, PreviewURLResult, Sandbox, SandboxOpts};
pub use template::{Template, TemplateInfo};

pub(crate) const DEFAULT_API_URL: &str = "https://app.opencomputer.dev";

pub(crate) fn resolve_api_url(url: &str) -> String {
    let trimmed = url.trim_end_matches('/');
    if trimmed.ends_with("/api") {
        trimmed.to_string()
    } else {
        format!("{}/api", trimmed)
    }
}
