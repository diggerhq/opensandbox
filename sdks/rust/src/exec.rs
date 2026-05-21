use std::collections::HashMap;
use std::sync::Arc;

use futures_util::{SinkExt, StreamExt};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use tokio::sync::{mpsc, oneshot, Mutex};
use tokio::task::JoinHandle;
use tokio_tungstenite::tungstenite::Message;

use crate::error::Result;
use crate::sandbox::{check_ok, parse_response, ClientCtx};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProcessResult {
    #[serde(rename = "exitCode")]
    pub exit_code: i32,
    #[serde(default)]
    pub stdout: String,
    #[serde(default)]
    pub stderr: String,
}

#[derive(Debug, Clone, Default)]
pub struct RunOpts {
    pub timeout: Option<u64>,
    pub env: Option<HashMap<String, String>>,
    pub cwd: Option<String>,
}

impl RunOpts {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn timeout(mut self, secs: u64) -> Self {
        self.timeout = Some(secs);
        self
    }

    pub fn env(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.env
            .get_or_insert_with(HashMap::new)
            .insert(key.into(), value.into());
        self
    }

    pub fn envs(mut self, env: HashMap<String, String>) -> Self {
        self.env = Some(env);
        self
    }

    pub fn cwd(mut self, cwd: impl Into<String>) -> Self {
        self.cwd = Some(cwd.into());
        self
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ExecSessionInfo {
    #[serde(rename = "sessionID")]
    pub session_id: String,
    #[serde(rename = "sandboxID", default)]
    pub sandbox_id: String,
    pub command: String,
    #[serde(default)]
    pub args: Vec<String>,
    pub running: bool,
    #[serde(rename = "exitCode", default)]
    pub exit_code: Option<i32>,
    #[serde(rename = "startedAt", default)]
    pub started_at: String,
    #[serde(rename = "attachedClients", default)]
    pub attached_clients: u32,
}

#[derive(Debug, Clone, Default)]
pub struct ExecStartOpts {
    pub args: Option<Vec<String>>,
    pub env: Option<HashMap<String, String>>,
    pub cwd: Option<String>,
    pub timeout: Option<u64>,
    pub max_run_after_disconnect: Option<u64>,
}

impl ExecStartOpts {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn args(mut self, args: Vec<String>) -> Self {
        self.args = Some(args);
        self
    }

    pub fn env(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.env
            .get_or_insert_with(HashMap::new)
            .insert(key.into(), value.into());
        self
    }

    pub fn cwd(mut self, cwd: impl Into<String>) -> Self {
        self.cwd = Some(cwd.into());
        self
    }

    pub fn timeout(mut self, secs: u64) -> Self {
        self.timeout = Some(secs);
        self
    }
}

/// Channel of streaming events from an attached exec session.
pub enum StreamEvent {
    Stdout(Vec<u8>),
    Stderr(Vec<u8>),
    /// Server has finished replaying scrollback; subsequent stdout/stderr is live.
    ScrollbackEnd,
    /// Process exited with this code. Always the last event.
    Exit(i32),
}

pub struct ExecSession {
    pub session_id: String,
    pub sandbox_id: String,
    exit: Mutex<Option<oneshot::Receiver<i32>>>,
    cached_exit: Mutex<Option<i32>>,
    stdin_tx: mpsc::UnboundedSender<Vec<u8>>,
    reader: Mutex<Option<JoinHandle<()>>>,
    ctx: Arc<ClientCtx>,
}

impl ExecSession {
    /// Wait for the process to exit and return its exit code.
    pub async fn done(&self) -> i32 {
        if let Some(code) = *self.cached_exit.lock().await {
            return code;
        }
        let recv = self.exit.lock().await.take();
        let code = match recv {
            Some(rx) => rx.await.unwrap_or(-1),
            None => -1,
        };
        *self.cached_exit.lock().await = Some(code);
        code
    }

    /// Write to the process stdin.
    pub fn send_stdin(&self, data: impl Into<Vec<u8>>) {
        let _ = self.stdin_tx.send(data.into());
    }

    /// Kill the underlying process. Default signal is SIGKILL (9).
    pub async fn kill(&self, signal: Option<i32>) -> Result<()> {
        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/exec/{}/kill",
                self.ctx.api_url, self.sandbox_id, self.session_id
            ))
            .headers(self.ctx.headers())
            .json(&json!({ "signal": signal.unwrap_or(9) }))
            .send()
            .await?;
        check_ok(resp, "kill exec session").await
    }

    /// Detach from the session. Does not kill the process.
    pub async fn close(&self) {
        if let Some(handle) = self.reader.lock().await.take() {
            handle.abort();
        }
    }
}

pub struct Exec {
    ctx: Arc<ClientCtx>,
    sandbox_id: String,
}

impl Exec {
    pub(crate) fn new(ctx: Arc<ClientCtx>, sandbox_id: String) -> Self {
        Self { ctx, sandbox_id }
    }

    /// Run a shell command and wait for completion.
    ///
    /// The command is executed via `sh -c`, so shell features like pipes,
    /// redirects, and env var expansion work as expected.
    pub async fn run(&self, command: &str, opts: RunOpts) -> Result<ProcessResult> {
        let mut body = serde_json::Map::new();
        body.insert("cmd".into(), Value::String("sh".into()));
        body.insert("args".into(), json!(["-c", command]));
        body.insert("timeout".into(), json!(opts.timeout.unwrap_or(60)));
        if let Some(env) = opts.env {
            body.insert("envs".into(), serde_json::to_value(env)?);
        }
        if let Some(cwd) = opts.cwd {
            body.insert("cwd".into(), Value::String(cwd));
        }

        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/exec/run",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.headers())
            .json(&Value::Object(body))
            .send()
            .await?;

        parse_response(resp, "run command").await
    }

    /// Start a long-running command and attach for streaming I/O.
    pub async fn start(
        &self,
        command: &str,
        opts: ExecStartOpts,
    ) -> Result<(ExecSession, mpsc::UnboundedReceiver<StreamEvent>)> {
        let mut body = serde_json::Map::new();
        body.insert("cmd".into(), Value::String(command.into()));
        if let Some(args) = opts.args {
            body.insert("args".into(), serde_json::to_value(args)?);
        }
        if let Some(env) = opts.env {
            body.insert("envs".into(), serde_json::to_value(env)?);
        }
        if let Some(cwd) = opts.cwd {
            body.insert("cwd".into(), Value::String(cwd));
        }
        if let Some(t) = opts.timeout {
            body.insert("timeout".into(), json!(t));
        }
        if let Some(t) = opts.max_run_after_disconnect {
            body.insert("maxRunAfterDisconnect".into(), json!(t));
        }

        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/exec",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.headers())
            .json(&Value::Object(body))
            .send()
            .await?;

        #[derive(Deserialize)]
        struct StartResp {
            #[serde(rename = "sessionID")]
            session_id: String,
        }
        let started: StartResp = parse_response(resp, "start exec session").await?;
        self.attach(&started.session_id).await
    }

    /// Alias for [`Exec::start`].
    pub async fn background(
        &self,
        command: &str,
        opts: ExecStartOpts,
    ) -> Result<(ExecSession, mpsc::UnboundedReceiver<StreamEvent>)> {
        self.start(command, opts).await
    }

    pub async fn attach(
        &self,
        session_id: &str,
    ) -> Result<(ExecSession, mpsc::UnboundedReceiver<StreamEvent>)> {
        let ws_base = self
            .ctx
            .api_url
            .replacen("https://", "wss://", 1)
            .replacen("http://", "ws://", 1);
        let auth = if !self.ctx.api_key.is_empty() {
            format!("?api_key={}", urlencoding(&self.ctx.api_key))
        } else {
            String::new()
        };
        let ws_url = format!(
            "{}/sandboxes/{}/exec/{}{}",
            ws_base, self.sandbox_id, session_id, auth
        );

        let (ws_stream, _) = tokio_tungstenite::connect_async(&ws_url).await?;
        let (mut ws_sink, mut ws_read) = ws_stream.split();

        let (events_tx, events_rx) = mpsc::unbounded_channel();
        let (stdin_tx, mut stdin_rx) = mpsc::unbounded_channel::<Vec<u8>>();
        let (exit_tx, exit_rx) = oneshot::channel::<i32>();

        let writer = tokio::spawn(async move {
            while let Some(payload) = stdin_rx.recv().await {
                let mut msg = Vec::with_capacity(1 + payload.len());
                msg.push(0x00);
                msg.extend_from_slice(&payload);
                if ws_sink.send(Message::Binary(msg)).await.is_err() {
                    break;
                }
            }
            let _ = ws_sink.close().await;
        });

        let events_tx_for_reader = events_tx.clone();
        let reader = tokio::spawn(async move {
            let mut got_exit = false;
            let mut exit_tx = Some(exit_tx);
            while let Some(msg) = ws_read.next().await {
                let msg = match msg {
                    Ok(m) => m,
                    Err(_) => break,
                };
                let data = match msg {
                    Message::Binary(b) => b,
                    Message::Close(_) => break,
                    _ => continue,
                };
                if data.is_empty() {
                    continue;
                }
                let stream_id = data[0];
                let payload: Vec<u8> = data[1..].to_vec();
                match stream_id {
                    0x01 => {
                        let _ = events_tx_for_reader.send(StreamEvent::Stdout(payload));
                    }
                    0x02 => {
                        let _ = events_tx_for_reader.send(StreamEvent::Stderr(payload));
                    }
                    0x03 => {
                        let code = if payload.len() >= 4 {
                            i32::from_be_bytes([payload[0], payload[1], payload[2], payload[3]])
                        } else {
                            0
                        };
                        got_exit = true;
                        let _ = events_tx_for_reader.send(StreamEvent::Exit(code));
                        if let Some(tx) = exit_tx.take() {
                            let _ = tx.send(code);
                        }
                    }
                    0x04 => {
                        let _ = events_tx_for_reader.send(StreamEvent::ScrollbackEnd);
                    }
                    _ => {}
                }
            }
            if !got_exit {
                let _ = events_tx_for_reader.send(StreamEvent::Exit(-1));
                if let Some(tx) = exit_tx.take() {
                    let _ = tx.send(-1);
                }
            }
            // Stop the writer once the reader is done (drops stdin_tx eventually).
            drop(events_tx_for_reader);
            let _ = writer.await;
        });

        let session = ExecSession {
            session_id: session_id.to_string(),
            sandbox_id: self.sandbox_id.clone(),
            exit: Mutex::new(Some(exit_rx)),
            cached_exit: Mutex::new(None),
            stdin_tx,
            reader: Mutex::new(Some(reader)),
            ctx: self.ctx.clone(),
        };
        Ok((session, events_rx))
    }

    pub async fn list(&self) -> Result<Vec<ExecSessionInfo>> {
        let resp = self
            .ctx
            .http
            .get(format!(
                "{}/sandboxes/{}/exec",
                self.ctx.api_url, self.sandbox_id
            ))
            .headers(self.ctx.auth_only_headers())
            .send()
            .await?;
        parse_response(resp, "list exec sessions").await
    }

    pub async fn kill(&self, session_id: &str, signal: Option<i32>) -> Result<()> {
        let resp = self
            .ctx
            .http
            .post(format!(
                "{}/sandboxes/{}/exec/{}/kill",
                self.ctx.api_url, self.sandbox_id, session_id
            ))
            .headers(self.ctx.headers())
            .json(&json!({ "signal": signal.unwrap_or(9) }))
            .send()
            .await?;
        check_ok(resp, "kill exec session").await
    }
}

fn urlencoding(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        let c = b as char;
        if c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '~') {
            out.push(c);
        } else {
            out.push_str(&format!("%{:02X}", b));
        }
    }
    out
}

impl Drop for ExecSession {
    fn drop(&mut self) {
        if let Ok(mut guard) = self.reader.try_lock() {
            if let Some(handle) = guard.take() {
                handle.abort();
            }
        }
    }
}
