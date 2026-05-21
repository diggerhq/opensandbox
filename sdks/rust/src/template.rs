use std::sync::Arc;

use serde::{Deserialize, Serialize};
use serde_json::json;

use crate::error::Result;
use crate::sandbox::{check_ok, parse_response, ClientCtx};
use crate::{resolve_api_url, DEFAULT_API_URL};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TemplateInfo {
    #[serde(rename = "templateID")]
    pub template_id: String,
    pub name: String,
    #[serde(default = "default_tag")]
    pub tag: String,
    #[serde(default = "default_status")]
    pub status: String,
}

fn default_tag() -> String {
    "latest".into()
}
fn default_status() -> String {
    "ready".into()
}

pub struct Template {
    ctx: Arc<ClientCtx>,
}

impl Template {
    pub fn new(api_key: Option<String>, api_url: Option<String>) -> Result<Self> {
        let api_url = match api_url {
            Some(u) => resolve_api_url(&u),
            None => match std::env::var("OPENCOMPUTER_API_URL") {
                Ok(u) => resolve_api_url(&u),
                Err(_) => resolve_api_url(DEFAULT_API_URL),
            },
        };
        let api_key = api_key
            .or_else(|| std::env::var("OPENCOMPUTER_API_KEY").ok())
            .unwrap_or_default();

        let http = reqwest::Client::builder()
            .timeout(std::time::Duration::from_secs(300))
            .build()?;
        Ok(Self {
            ctx: Arc::new(ClientCtx {
                api_url,
                api_key,
                http,
            }),
        })
    }

    pub async fn build(&self, name: &str, dockerfile: &str) -> Result<TemplateInfo> {
        let resp = self
            .ctx
            .http
            .post(format!("{}/templates", self.ctx.api_url))
            .headers(self.ctx.headers())
            .json(&json!({ "name": name, "dockerfile": dockerfile }))
            .send()
            .await?;
        parse_response(resp, "build template").await
    }

    pub async fn list(&self) -> Result<Vec<TemplateInfo>> {
        let resp = self
            .ctx
            .http
            .get(format!("{}/templates", self.ctx.api_url))
            .headers(self.ctx.auth_only_headers())
            .send()
            .await?;
        parse_response(resp, "list templates").await
    }

    pub async fn get(&self, name: &str) -> Result<TemplateInfo> {
        let resp = self
            .ctx
            .http
            .get(format!("{}/templates/{}", self.ctx.api_url, name))
            .headers(self.ctx.auth_only_headers())
            .send()
            .await?;
        parse_response(resp, "get template").await
    }

    pub async fn delete(&self, name: &str) -> Result<()> {
        let resp = self
            .ctx
            .http
            .delete(format!("{}/templates/{}", self.ctx.api_url, name))
            .headers(self.ctx.auth_only_headers())
            .send()
            .await?;
        check_ok(resp, "delete template").await
    }
}
