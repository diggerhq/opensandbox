use std::collections::HashMap;
use std::sync::Arc;

use reqwest::Client;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::error::{Error, Result};
use crate::exec::Exec;
use crate::filesystem::Filesystem;
use crate::{resolve_api_url, DEFAULT_API_URL};

#[derive(Debug, Clone, Default)]
pub struct SandboxOpts {
    pub template: Option<String>,
    /// Idle timeout in seconds. `0` (default) = persistent, never auto-hibernates.
    pub timeout: Option<u64>,
    pub api_key: Option<String>,
    pub api_url: Option<String>,
    pub envs: Option<HashMap<String, String>>,
    pub metadata: Option<HashMap<String, String>>,
    pub cpu_count: Option<u32>,
    pub memory_mb: Option<u64>,
    pub disk_mb: Option<u64>,
    pub secret_store: Option<String>,
    pub snapshot: Option<String>,
}

impl SandboxOpts {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn template(mut self, template: impl Into<String>) -> Self {
        self.template = Some(template.into());
        self
    }

    pub fn timeout(mut self, timeout: u64) -> Self {
        self.timeout = Some(timeout);
        self
    }

    pub fn api_key(mut self, api_key: impl Into<String>) -> Self {
        self.api_key = Some(api_key.into());
        self
    }

    pub fn api_url(mut self, api_url: impl Into<String>) -> Self {
        self.api_url = Some(api_url.into());
        self
    }

    pub fn env(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.envs
            .get_or_insert_with(HashMap::new)
            .insert(key.into(), value.into());
        self
    }

    pub fn envs(mut self, envs: HashMap<String, String>) -> Self {
        self.envs = Some(envs);
        self
    }

    pub fn metadata(mut self, metadata: HashMap<String, String>) -> Self {
        self.metadata = Some(metadata);
        self
    }

    pub fn cpu_count(mut self, cpu_count: u32) -> Self {
        self.cpu_count = Some(cpu_count);
        self
    }

    pub fn memory_mb(mut self, memory_mb: u64) -> Self {
        self.memory_mb = Some(memory_mb);
        self
    }

    pub fn disk_mb(mut self, disk_mb: u64) -> Self {
        self.disk_mb = Some(disk_mb);
        self
    }

    pub fn secret_store(mut self, secret_store: impl Into<String>) -> Self {
        self.secret_store = Some(secret_store.into());
        self
    }

    pub fn snapshot(mut self, snapshot: impl Into<String>) -> Self {
        self.snapshot = Some(snapshot.into());
        self
    }
}

#[derive(Debug, Deserialize)]
struct SandboxData {
    #[serde(rename = "sandboxID")]
    sandbox_id: String,
    #[serde(default)]
    status: String,
    #[serde(rename = "templateID", default)]
    template_id: String,
    #[serde(rename = "connectURL", default)]
    #[allow(dead_code)]
    connect_url: String,
    #[serde(default)]
    #[allow(dead_code)]
    token: String,
    #[serde(rename = "sandboxDomain", default)]
    sandbox_domain: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CheckpointInfo {
    pub id: String,
    #[serde(rename = "sandboxId")]
    pub sandbox_id: String,
    #[serde(rename = "orgId")]
    pub org_id: String,
    pub name: String,
    pub status: String,
    #[serde(rename = "sizeBytes", default)]
    pub size_bytes: u64,
    #[serde(rename = "createdAt", default)]
    pub created_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PatchInfo {
    pub id: String,
    #[serde(rename = "checkpointId")]
    pub checkpoint_id: String,
    pub sequence: u64,
    pub script: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub strategy: String,
    #[serde(rename = "createdAt", default)]
    pub created_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PatchResult {
    pub patch: PatchInfo,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PreviewURLResult {
    pub id: String,
    #[serde(rename = "sandboxId")]
    pub sandbox_id: String,
    #[serde(rename = "orgId", default)]
    pub org_id: String,
    pub hostname: String,
    #[serde(rename = "customHostname", default)]
    pub custom_hostname: Option<String>,
    pub port: u16,
    #[serde(rename = "sslStatus", default)]
    pub ssl_status: String,
    #[serde(rename = "createdAt", default)]
    pub created_at: String,
}

pub(crate) struct ClientCtx {
    pub api_url: String,
    pub api_key: String,
    pub http: Client,
}

impl ClientCtx {
    pub fn headers(&self) -> reqwest::header::HeaderMap {
        let mut h = reqwest::header::HeaderMap::new();
        h.insert(
            reqwest::header::CONTENT_TYPE,
            reqwest::header::HeaderValue::from_static("application/json"),
        );
        if !self.api_key.is_empty() {
            if let Ok(v) = reqwest::header::HeaderValue::from_str(&self.api_key) {
                h.insert("X-API-Key", v);
            }
        }
        h
    }

    pub fn auth_only_headers(&self) -> reqwest::header::HeaderMap {
        let mut h = reqwest::header::HeaderMap::new();
        if !self.api_key.is_empty() {
            if let Ok(v) = reqwest::header::HeaderValue::from_str(&self.api_key) {
                h.insert("X-API-Key", v);
            }
        }
        h
    }
}

pub struct Sandbox {
    sandbox_id: String,
    template_id: String,
    sandbox_domain: String,
    status: std::sync::Mutex<String>,
    ctx: Arc<ClientCtx>,
}

impl std::fmt::Debug for Sandbox {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Sandbox")
            .field("sandbox_id", &self.sandbox_id)
            .field("template", &self.template_id)
            .finish()
    }
}

impl Sandbox {
    pub fn sandbox_id(&self) -> &str {
        &self.sandbox_id
    }

    pub fn template(&self) -> &str {
        &self.template_id
    }

    pub fn status(&self) -> String {
        self.status.lock().unwrap().clone()
    }

    /// Preview URL domain for port 80 (e.g., `sb-xxx-p80.workers.opencomputer.dev`).
    pub fn domain(&self) -> String {
        if self.sandbox_domain.is_empty() {
            return String::new();
        }
        format!("{}-p80.{}", self.sandbox_id, self.sandbox_domain)
    }

    pub fn preview_domain(&self, port: u16) -> String {
        if self.sandbox_domain.is_empty() {
            return String::new();
        }
        format!("{}-p{}.{}", self.sandbox_id, port, self.sandbox_domain)
    }

    pub fn files(&self) -> Filesystem {
        Filesystem::new(self.ctx.clone(), self.sandbox_id.clone())
    }

    pub fn exec(&self) -> Exec {
        Exec::new(self.ctx.clone(), self.sandbox_id.clone())
    }

    /// Backwards-compatible alias for [`Sandbox::exec`]. Mirrors the Python and
    /// TypeScript SDKs.
    pub fn commands(&self) -> Exec {
        self.exec()
    }

    pub async fn create(opts: SandboxOpts) -> Result<Sandbox> {
        let api_url = match opts.api_url.clone() {
            Some(u) => resolve_api_url(&u),
            None => match std::env::var("OPENCOMPUTER_API_URL") {
                Ok(u) => resolve_api_url(&u),
                Err(_) => resolve_api_url(DEFAULT_API_URL),
            },
        };
        let api_key = opts
            .api_key
            .or_else(|| std::env::var("OPENCOMPUTER_API_KEY").ok())
            .unwrap_or_default();

        let mut body = serde_json::Map::new();
        body.insert(
            "templateID".into(),
            Value::String(opts.template.unwrap_or_else(|| "base".into())),
        );
        body.insert("timeout".into(), json!(opts.timeout.unwrap_or(0)));
        if let Some(envs) = opts.envs {
            body.insert("envs".into(), serde_json::to_value(envs)?);
        }
        if let Some(metadata) = opts.metadata {
            body.insert("metadata".into(), serde_json::to_value(metadata)?);
        }
        if let Some(c) = opts.cpu_count {
            body.insert("cpuCount".into(), json!(c));
        }
        if let Some(m) = opts.memory_mb {
            body.insert("memoryMB".into(), json!(m));
        }
        if let Some(d) = opts.disk_mb {
            body.insert("diskMB".into(), json!(d));
        }
        if let Some(store) = opts.secret_store {
            body.insert("secretStore".into(), Value::String(store));
        }
        let has_snapshot = opts.snapshot.is_some();
        if let Some(snap) = opts.snapshot {
            body.insert("snapshot".into(), Value::String(snap));
        }

        let http = build_client(has_snapshot)?;
        let ctx = Arc::new(ClientCtx {
            api_url: api_url.clone(),
            api_key: api_key.clone(),
            http,
        });

        let resp = ctx
            .http
            .post(format!("{}/sandboxes", api_url))
            .headers(ctx.headers())
            .json(&Value::Object(body))
            .send()
            .await?;

        let data: SandboxData = parse_response(resp, "create sandbox").await?;
        let status = data.status.clone();
        Ok(Sandbox {
            sandbox_id: data.sandbox_id,
            template_id: data.template_id,
            sandbox_domain: data.sandbox_domain,
            status: std::sync::Mutex::new(if status.is_empty() {
                "running".into()
            } else {
                status
            }),
            ctx,
        })
    }

    pub async fn connect(sandbox_id: impl Into<String>, opts: SandboxOpts) -> Result<Sandbox> {
        let sandbox_id = sandbox_id.into();
        let api_url = match opts.api_url {
            Some(u) => resolve_api_url(&u),
            None => match std::env::var("OPENCOMPUTER_API_URL") {
                Ok(u) => resolve_api_url(&u),
                Err(_) => resolve_api_url(DEFAULT_API_URL),
            },
        };
        let api_key = opts
            .api_key
            .or_else(|| std::env::var("OPENCOMPUTER_API_KEY").ok())
            .unwrap_or_default();

        let http = build_client(false)?;
        let ctx = Arc::new(ClientCtx {
            api_url: api_url.clone(),
            api_key,
            http,
        });

        let resp = ctx
            .http
            .get(format!("{}/sandboxes/{}", api_url, sandbox_id))
            .headers(ctx.auth_only_headers())
            .send()
            .await?;
        let data: SandboxData = parse_response(resp, "connect sandbox").await?;
        let status = if data.status.is_empty() {
            "running".into()
        } else {
            data.status.clone()
        };
        Ok(Sandbox {
            sandbox_id: data.sandbox_id,
            template_id: data.template_id,
            sandbox_domain: data.sandbox_domain,
            status: std::sync::Mutex::new(status),
            ctx,
        })
    }

    pub async fn kill(&self) -> Result<()> {
        let resp = self
            .ctx
            .http
            .delete(format!(
                "{}/sandboxes/{}",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.auth_only_headers())
            .send()
            .await?;
        check_ok(resp, "kill sandbox").await?;
        *self.status.lock().unwrap() = "stopped".into();
        Ok(())
    }

    pub async fn is_running(&self) -> bool {
        match self
            .ctx
            .http
            .get(format!(
                "{}/sandboxes/{}",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.auth_only_headers())
            .send()
            .await
        {
            Ok(resp) if resp.status().is_success() => match resp.json::<SandboxData>().await {
                Ok(data) => {
                    let running = data.status == "running";
                    *self.status.lock().unwrap() = data.status;
                    running
                }
                Err(_) => false,
            },
            _ => false,
        }
    }

    pub async fn hibernate(&self) -> Result<()> {
        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/hibernate",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.headers())
            .send()
            .await?;
        check_ok(resp, "hibernate sandbox").await?;
        *self.status.lock().unwrap() = "hibernated".into();
        Ok(())
    }

    pub async fn wake(&self, timeout: Option<u64>) -> Result<()> {
        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/wake",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.headers())
            .json(&json!({ "timeout": timeout.unwrap_or(0) }))
            .send()
            .await?;
        let data: SandboxData = parse_response(resp, "wake sandbox").await?;
        *self.status.lock().unwrap() = data.status;
        Ok(())
    }

    pub async fn set_timeout(&self, timeout: u64) -> Result<()> {
        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/timeout",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.headers())
            .json(&json!({ "timeout": timeout }))
            .send()
            .await?;
        check_ok(resp, "set timeout").await
    }

    pub async fn create_checkpoint(&self, name: impl Into<String>) -> Result<CheckpointInfo> {
        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/checkpoints",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.headers())
            .json(&json!({ "name": name.into() }))
            .send()
            .await?;
        parse_response(resp, "create checkpoint").await
    }

    pub async fn list_checkpoints(&self) -> Result<Vec<CheckpointInfo>> {
        let resp = self
            .ctx
            .http
            .get(format!(
                "{}/sandboxes/{}/checkpoints",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.auth_only_headers())
            .send()
            .await?;
        parse_response(resp, "list checkpoints").await
    }

    pub async fn restore_checkpoint(&self, checkpoint_id: &str) -> Result<()> {
        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/checkpoints/{}/restore",
                self.ctx.api_url, self.sandbox_id, checkpoint_id
            ))
            .headers(self.ctx.headers())
            .send()
            .await?;
        check_ok(resp, "restore checkpoint").await
    }

    pub async fn delete_checkpoint(&self, checkpoint_id: &str) -> Result<()> {
        let resp = self
            .ctx
            .http
            .delete(format!(
                "{}/sandboxes/{}/checkpoints/{}",
                self.ctx.api_url, self.sandbox_id, checkpoint_id
            ))
            .headers(self.ctx.auth_only_headers())
            .send()
            .await?;
        if resp.status().as_u16() == 404 {
            return Ok(());
        }
        check_ok(resp, "delete checkpoint").await
    }

    pub async fn create_preview_url(
        &self,
        port: u16,
        domain: Option<&str>,
    ) -> Result<PreviewURLResult> {
        let mut body = serde_json::Map::new();
        body.insert("port".into(), json!(port));
        body.insert("authConfig".into(), json!({}));
        if let Some(d) = domain {
            body.insert("domain".into(), Value::String(d.into()));
        }
        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/preview",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.headers())
            .json(&Value::Object(body))
            .send()
            .await?;
        parse_response(resp, "create preview URL").await
    }

    pub async fn list_preview_urls(&self) -> Result<Vec<PreviewURLResult>> {
        let resp = self
            .ctx
            .http
            .get(format!(
                "{}/sandboxes/{}/preview",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.auth_only_headers())
            .send()
            .await?;
        parse_response(resp, "list preview URLs").await
    }

    pub async fn delete_preview_url(&self, port: u16) -> Result<()> {
        let resp = self
            .ctx
            .http
            .delete(format!(
                "{}/sandboxes/{}/preview/{}",
                self.ctx.api_url, self.sandbox_id, port
            ))
            .headers(self.ctx.auth_only_headers())
            .send()
            .await?;
        if resp.status().as_u16() == 404 {
            return Ok(());
        }
        check_ok(resp, "delete preview URL").await
    }

    pub async fn download_url(&self, path: &str, expires_in: Option<u64>) -> Result<String> {
        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/files/download-url",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.headers())
            .json(&json!({ "path": path, "expiresIn": expires_in.unwrap_or(3600) }))
            .send()
            .await?;
        let v: Value = parse_response(resp, "download URL").await?;
        Ok(v.get("url")
            .and_then(|u| u.as_str())
            .unwrap_or_default()
            .to_string())
    }

    pub async fn upload_url(&self, path: &str, expires_in: Option<u64>) -> Result<String> {
        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/files/upload-url",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.headers())
            .json(&json!({ "path": path, "expiresIn": expires_in.unwrap_or(3600) }))
            .send()
            .await?;
        let v: Value = parse_response(resp, "upload URL").await?;
        Ok(v.get("url")
            .and_then(|u| u.as_str())
            .unwrap_or_default()
            .to_string())
    }
}

fn build_client(long_timeout: bool) -> Result<Client> {
    let secs = if long_timeout { 300 } else { 30 };
    Client::builder()
        .timeout(std::time::Duration::from_secs(secs))
        .build()
        .map_err(Into::into)
}

pub(crate) async fn parse_response<T: serde::de::DeserializeOwned>(
    resp: reqwest::Response,
    op: &str,
) -> Result<T> {
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(Error::Api {
            status: status.as_u16(),
            body: format!("failed to {}: {}", op, body),
        });
    }
    let bytes = resp.bytes().await?;
    if bytes.is_empty() {
        // Some endpoints return empty bodies on success.
        let v: T = serde_json::from_slice(b"null").map_err(|_| {
            Error::other(format!(
                "empty response from {} (and not deserializable)",
                op
            ))
        })?;
        return Ok(v);
    }
    serde_json::from_slice(&bytes).map_err(Into::into)
}

pub(crate) async fn check_ok(resp: reqwest::Response, op: &str) -> Result<()> {
    let status = resp.status();
    if !status.is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(Error::Api {
            status: status.as_u16(),
            body: format!("failed to {}: {}", op, body),
        });
    }
    Ok(())
}
