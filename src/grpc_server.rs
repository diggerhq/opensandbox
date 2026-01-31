//! gRPC server implementation using Tonic.

use crate::sandbox::{self, RunConfig};
use crate::state::AppState;
use std::net::SocketAddr;
use std::time::Instant;
use tonic::{Request, Response, Status};
use tracing::info;

// Import generated protobuf types
pub mod proto {
    tonic::include_proto!("sandbox");
}

use proto::sandbox_service_server::{SandboxService, SandboxServiceServer};
use proto::{
    ReadFileRequest, ReadFileResponse, RunCommandRequest, RunCommandResponse,
    SetCwdRequest, SetCwdResponse, SetEnvRequest, SetEnvResponse,
    WriteFileRequest, WriteFileResponse,
};

/// gRPC service implementation.
pub struct SandboxServiceImpl {
    state: AppState,
}

impl SandboxServiceImpl {
    pub fn new(state: AppState) -> Self {
        Self { state }
    }
}

#[tonic::async_trait]
impl SandboxService for SandboxServiceImpl {
    async fn run_command(
        &self,
        request: Request<RunCommandRequest>,
    ) -> Result<Response<RunCommandResponse>, Status> {
        let req = request.into_inner();
        info!("gRPC RunCommand: session={}, command={:?}", req.session_id, req.command);

        // Get session info
        let (sandbox_root, mut env, cwd) = {
            let mut sessions = self.state.sessions.write().await;
            let session = sessions
                .get_mut(&req.session_id)
                .ok_or_else(|| Status::not_found("Session not found"))?;
            session.last_used = Instant::now();
            (session.sandbox_root.clone(), session.env.clone(), session.cwd.clone())
        };

        // Merge request env with session env
        env.extend(req.env);
        let cwd = if !req.cwd.is_empty() && req.cwd != "/" { req.cwd } else { cwd };

        let config = RunConfig {
            command: req.command,
            time_ms: if req.time_ms > 0 { req.time_ms } else { 300000 },
            mem_kb: if req.mem_kb > 0 { req.mem_kb } else { 2097152 },
            fsize_kb: if req.fsize_kb > 0 { req.fsize_kb } else { 1048576 },
            nofile: if req.nofile > 0 { req.nofile } else { 256 },
            env,
            cwd,
        };

        let result = tokio::task::spawn_blocking(move || {
            sandbox::run_in_session(&sandbox_root, &config)
        })
        .await
        .map_err(|e| Status::internal(e.to_string()))?
        .map_err(|e| Status::internal(e))?;

        Ok(Response::new(RunCommandResponse {
            stdout: result.stdout,
            stderr: result.stderr,
            exit_code: result.exit_code.unwrap_or(0),
            signal: result.signal.unwrap_or(0),
        }))
    }

    async fn write_file(
        &self,
        request: Request<WriteFileRequest>,
    ) -> Result<Response<WriteFileResponse>, Status> {
        let req = request.into_inner();
        info!("gRPC WriteFile: session={}, path={}", req.session_id, req.path);

        // Get sandbox root
        let sandbox_root = {
            let mut sessions = self.state.sessions.write().await;
            let session = sessions
                .get_mut(&req.session_id)
                .ok_or_else(|| Status::not_found("Session not found"))?;
            session.last_used = Instant::now();
            session.sandbox_root.clone()
        };

        // Write file directly (no shell command needed)
        let path = req.path;
        let content = req.content;
        let result = tokio::task::spawn_blocking(move || {
            sandbox::write_file_in_sandbox(&sandbox_root, &path, &content)
        })
        .await
        .map_err(|e| Status::internal(e.to_string()))?;

        match result {
            Ok(()) => Ok(Response::new(WriteFileResponse {
                success: true,
                error: String::new(),
            })),
            Err(e) => Ok(Response::new(WriteFileResponse {
                success: false,
                error: e,
            })),
        }
    }

    async fn read_file(
        &self,
        request: Request<ReadFileRequest>,
    ) -> Result<Response<ReadFileResponse>, Status> {
        let req = request.into_inner();
        info!("gRPC ReadFile: session={}, path={}", req.session_id, req.path);

        // Get sandbox root
        let sandbox_root = {
            let mut sessions = self.state.sessions.write().await;
            let session = sessions
                .get_mut(&req.session_id)
                .ok_or_else(|| Status::not_found("Session not found"))?;
            session.last_used = Instant::now();
            session.sandbox_root.clone()
        };

        // Read file directly (no shell command needed)
        let path = req.path;
        let result = tokio::task::spawn_blocking(move || {
            sandbox::read_file_in_sandbox(&sandbox_root, &path)
        })
        .await
        .map_err(|e| Status::internal(e.to_string()))?;

        match result {
            Ok(content) => Ok(Response::new(ReadFileResponse {
                content,
                error: String::new(),
            })),
            Err(e) => Ok(Response::new(ReadFileResponse {
                content: Vec::new(),
                error: e,
            })),
        }
    }

    async fn set_env(
        &self,
        request: Request<SetEnvRequest>,
    ) -> Result<Response<SetEnvResponse>, Status> {
        let req = request.into_inner();
        info!("gRPC SetEnv: session={}", req.session_id);

        let mut sessions = self.state.sessions.write().await;
        let session = sessions
            .get_mut(&req.session_id)
            .ok_or_else(|| Status::not_found("Session not found"))?;
        session.env.extend(req.env);
        session.last_used = Instant::now();

        Ok(Response::new(SetEnvResponse { success: true }))
    }

    async fn set_cwd(
        &self,
        request: Request<SetCwdRequest>,
    ) -> Result<Response<SetCwdResponse>, Status> {
        let req = request.into_inner();
        info!("gRPC SetCwd: session={}, cwd={}", req.session_id, req.cwd);

        let mut sessions = self.state.sessions.write().await;
        let session = sessions
            .get_mut(&req.session_id)
            .ok_or_else(|| Status::not_found("Session not found"))?;
        session.cwd = req.cwd;
        session.last_used = Instant::now();

        Ok(Response::new(SetCwdResponse { success: true }))
    }
}

/// Run the gRPC server on the given port with the provided state.
pub async fn run_server(port: u16, state: AppState) {
    let addr = SocketAddr::from(([0, 0, 0, 0], port));
    info!("Starting gRPC server on {}", addr);

    let service = SandboxServiceImpl::new(state);

    tonic::transport::Server::builder()
        .add_service(SandboxServiceServer::new(service))
        .serve(addr)
        .await
        .unwrap();
}
