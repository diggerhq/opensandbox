use std::sync::Arc;

use serde::{Deserialize, Serialize};

use crate::error::{Error, Result};
use crate::sandbox::ClientCtx;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EntryInfo {
    pub name: String,
    #[serde(rename = "isDir", default)]
    pub is_dir: bool,
    #[serde(default)]
    pub path: String,
    #[serde(default)]
    pub size: u64,
}

pub struct Filesystem {
    ctx: Arc<ClientCtx>,
    sandbox_id: String,
}

impl Filesystem {
    pub(crate) fn new(ctx: Arc<ClientCtx>, sandbox_id: String) -> Self {
        Self { ctx, sandbox_id }
    }

    fn url(&self) -> String {
        format!("{}/sandboxes/{}/files", self.ctx.api_url, self.sandbox_id)
    }

    pub async fn read(&self, path: &str) -> Result<String> {
        let resp = self
            .ctx
            .http
            .get(self.url())
            .headers(self.ctx.auth_only_headers())
            .query(&[("path", path)])
            .send()
            .await?;
        let status = resp.status();
        if !status.is_success() {
            let body = resp.text().await.unwrap_or_default();
            return Err(Error::Api {
                status: status.as_u16(),
                body: format!("failed to read {}: {}", path, body),
            });
        }
        Ok(resp.text().await?)
    }

    pub async fn read_bytes(&self, path: &str) -> Result<Vec<u8>> {
        let resp = self
            .ctx
            .http
            .get(self.url())
            .headers(self.ctx.auth_only_headers())
            .query(&[("path", path)])
            .send()
            .await?;
        let status = resp.status();
        if !status.is_success() {
            let body = resp.text().await.unwrap_or_default();
            return Err(Error::Api {
                status: status.as_u16(),
                body: format!("failed to read {}: {}", path, body),
            });
        }
        Ok(resp.bytes().await?.to_vec())
    }

    pub async fn write(&self, path: &str, content: impl Into<Vec<u8>>) -> Result<()> {
        let body: Vec<u8> = content.into();
        let resp = self
            .ctx
            .http
            .put(self.url())
            .headers(self.ctx.auth_only_headers())
            .query(&[("path", path)])
            .body(body)
            .send()
            .await?;
        let status = resp.status();
        if !status.is_success() {
            let body = resp.text().await.unwrap_or_default();
            return Err(Error::Api {
                status: status.as_u16(),
                body: format!("failed to write {}: {}", path, body),
            });
        }
        Ok(())
    }

    pub async fn list(&self, path: &str) -> Result<Vec<EntryInfo>> {
        let resp = self
            .ctx
            .http
            .get(format!("{}/list", self.url()))
            .headers(self.ctx.auth_only_headers())
            .query(&[("path", path)])
            .send()
            .await?;
        let status = resp.status();
        if !status.is_success() {
            let body = resp.text().await.unwrap_or_default();
            return Err(Error::Api {
                status: status.as_u16(),
                body: format!("failed to list {}: {}", path, body),
            });
        }
        let bytes = resp.bytes().await?;
        if bytes.is_empty() {
            return Ok(Vec::new());
        }
        // Server may return null when empty.
        let v: serde_json::Value = serde_json::from_slice(&bytes)?;
        if v.is_null() {
            return Ok(Vec::new());
        }
        Ok(serde_json::from_value(v)?)
    }

    pub async fn make_dir(&self, path: &str) -> Result<()> {
        let resp = self
            .ctx
            .http
            .post(format!("{}/mkdir", self.url()))
            .headers(self.ctx.auth_only_headers())
            .query(&[("path", path)])
            .send()
            .await?;
        let status = resp.status();
        if !status.is_success() {
            let body = resp.text().await.unwrap_or_default();
            return Err(Error::Api {
                status: status.as_u16(),
                body: format!("failed to mkdir {}: {}", path, body),
            });
        }
        Ok(())
    }

    pub async fn remove(&self, path: &str) -> Result<()> {
        let resp = self
            .ctx
            .http
            .delete(self.url())
            .headers(self.ctx.auth_only_headers())
            .query(&[("path", path)])
            .send()
            .await?;
        let status = resp.status();
        if !status.is_success() {
            let body = resp.text().await.unwrap_or_default();
            return Err(Error::Api {
                status: status.as_u16(),
                body: format!("failed to remove {}: {}", path, body),
            });
        }
        Ok(())
    }

    pub async fn exists(&self, path: &str) -> bool {
        match self
            .ctx
            .http
            .get(self.url())
            .headers(self.ctx.auth_only_headers())
            .query(&[("path", path)])
            .send()
            .await
        {
            Ok(resp) => resp.status().is_success(),
            Err(_) => false,
        }
    }
}
