"""Sandbox session class for interacting with a running sandbox."""

from dataclasses import dataclass
from typing import Optional, Dict
import grpc

from .proto import sandbox_pb2, sandbox_pb2_grpc
from .exceptions import (
    CommandExecutionError,
    FileOperationError,
    SandboxNotFoundError,
)


@dataclass
class CommandResult:
    """Result of a command execution."""
    stdout: str
    stderr: str
    exit_code: int
    signal: int = 0

    @property
    def success(self) -> bool:
        """Returns True if the command exited with code 0."""
        return self.exit_code == 0


class Sandbox:
    """A sandbox session that can execute commands and manage files.

    This class uses gRPC for fast command execution and file operations.
    """

    def __init__(
        self,
        session_id: str,
        grpc_channel: grpc.Channel,
        http_base_url: str,
        http_client,
    ):
        """Initialize a sandbox session.

        Args:
            session_id: The unique session ID from the server.
            grpc_channel: The gRPC channel for fast operations.
            http_base_url: Base URL for HTTP API (used for destroy).
            http_client: HTTP client instance.
        """
        self.session_id = session_id
        self._grpc_channel = grpc_channel
        self._stub = sandbox_pb2_grpc.SandboxServiceStub(grpc_channel)
        self._http_base_url = http_base_url
        self._http_client = http_client
        self._destroyed = False

    async def run(
        self,
        command: str,
        *,
        timeout_ms: int = 300000,
        mem_kb: int = 2097152,
        env: Optional[Dict[str, str]] = None,
        cwd: Optional[str] = None,
    ) -> CommandResult:
        """Execute a shell command in the sandbox.

        Args:
            command: The shell command to execute.
            timeout_ms: CPU time limit in milliseconds.
            mem_kb: Memory limit in KB.
            env: Additional environment variables.
            cwd: Working directory for the command.

        Returns:
            CommandResult with stdout, stderr, exit_code, and signal.

        Raises:
            SandboxNotFoundError: If the session no longer exists.
            CommandExecutionError: If there was an error executing the command.
        """
        if self._destroyed:
            raise SandboxNotFoundError("Sandbox has been destroyed")

        request = sandbox_pb2.RunCommandRequest(
            session_id=self.session_id,
            command=["/bin/sh", "-c", command],
            time_ms=timeout_ms,
            mem_kb=mem_kb,
            fsize_kb=1048576,
            nofile=256,
            env=env or {},
            cwd=cwd or "",
        )

        try:
            response = await self._stub.RunCommand(request)
            return CommandResult(
                stdout=response.stdout,
                stderr=response.stderr,
                exit_code=response.exit_code,
                signal=response.signal,
            )
        except grpc.RpcError as e:
            if e.code() == grpc.StatusCode.NOT_FOUND:
                raise SandboxNotFoundError("Session not found") from e
            raise CommandExecutionError(f"gRPC error: {e.details()}") from e

    async def write_file(self, path: str, content: str | bytes) -> None:
        """Write content to a file in the sandbox.

        Args:
            path: Absolute path in the sandbox filesystem.
            content: File content (string or bytes).

        Raises:
            SandboxNotFoundError: If the session no longer exists.
            FileOperationError: If the file cannot be written.
        """
        if self._destroyed:
            raise SandboxNotFoundError("Sandbox has been destroyed")

        if isinstance(content, str):
            content = content.encode("utf-8")

        request = sandbox_pb2.WriteFileRequest(
            session_id=self.session_id,
            path=path,
            content=content,
        )

        try:
            response = await self._stub.WriteFile(request)
            if not response.success:
                raise FileOperationError(f"Failed to write file: {response.error}")
        except grpc.RpcError as e:
            if e.code() == grpc.StatusCode.NOT_FOUND:
                raise SandboxNotFoundError("Session not found") from e
            raise FileOperationError(f"gRPC error: {e.details()}") from e

    async def read_file(self, path: str) -> bytes:
        """Read a file from the sandbox.

        Args:
            path: Absolute path in the sandbox filesystem.

        Returns:
            The file contents as bytes.

        Raises:
            SandboxNotFoundError: If the session no longer exists.
            FileOperationError: If the file cannot be read.
        """
        if self._destroyed:
            raise SandboxNotFoundError("Sandbox has been destroyed")

        request = sandbox_pb2.ReadFileRequest(
            session_id=self.session_id,
            path=path,
        )

        try:
            response = await self._stub.ReadFile(request)
            if response.error:
                raise FileOperationError(f"Failed to read file: {response.error}")
            return response.content
        except grpc.RpcError as e:
            if e.code() == grpc.StatusCode.NOT_FOUND:
                raise SandboxNotFoundError("Session not found") from e
            raise FileOperationError(f"gRPC error: {e.details()}") from e

    async def read_file_text(self, path: str, encoding: str = "utf-8") -> str:
        """Read a text file from the sandbox.

        Args:
            path: Absolute path in the sandbox filesystem.
            encoding: Text encoding (default: utf-8).

        Returns:
            The file contents as a string.
        """
        content = await self.read_file(path)
        return content.decode(encoding)

    async def set_env(self, env: Dict[str, str]) -> None:
        """Set environment variables for subsequent commands.

        Args:
            env: Dictionary of environment variables to set.

        Raises:
            SandboxNotFoundError: If the session no longer exists.
        """
        if self._destroyed:
            raise SandboxNotFoundError("Sandbox has been destroyed")

        request = sandbox_pb2.SetEnvRequest(
            session_id=self.session_id,
            env=env,
        )

        try:
            await self._stub.SetEnv(request)
        except grpc.RpcError as e:
            if e.code() == grpc.StatusCode.NOT_FOUND:
                raise SandboxNotFoundError("Session not found") from e
            raise

    async def set_cwd(self, cwd: str) -> None:
        """Set the working directory for subsequent commands.

        Args:
            cwd: The new working directory path.

        Raises:
            SandboxNotFoundError: If the session no longer exists.
        """
        if self._destroyed:
            raise SandboxNotFoundError("Sandbox has been destroyed")

        request = sandbox_pb2.SetCwdRequest(
            session_id=self.session_id,
            cwd=cwd,
        )

        try:
            await self._stub.SetCwd(request)
        except grpc.RpcError as e:
            if e.code() == grpc.StatusCode.NOT_FOUND:
                raise SandboxNotFoundError("Session not found") from e
            raise

    async def destroy(self) -> None:
        """Destroy this sandbox session.

        After calling this method, the sandbox cannot be used anymore.
        """
        if self._destroyed:
            return

        try:
            response = await self._http_client.delete(
                f"{self._http_base_url}/sessions/{self.session_id}"
            )
            response.raise_for_status()
        except Exception:
            pass  # Best effort cleanup
        finally:
            self._destroyed = True

    async def __aenter__(self):
        """Async context manager entry."""
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb):
        """Async context manager exit - destroys the sandbox."""
        await self.destroy()
        return False
