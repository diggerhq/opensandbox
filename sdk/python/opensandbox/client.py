"""OpenSandbox client for creating and managing sandbox sessions."""

from typing import Optional, Dict
from urllib.parse import urlparse

import grpc
import httpx

from .sandbox import Sandbox
from .exceptions import SandboxConnectionError


class OpenSandbox:
    """Client for connecting to an OpenSandbox server.

    This client uses HTTP for sandbox lifecycle (create/destroy) and
    gRPC for fast command execution and file operations.

    Usage:
        async with OpenSandbox("https://opensandbox.example.com") as client:
            sandbox = await client.create()
            result = await sandbox.run("echo hello")
            print(result.stdout)
            await sandbox.destroy()
    """

    def __init__(
        self,
        base_url: str,
        *,
        grpc_port: Optional[int] = None,
        grpc_insecure: Optional[bool] = None,
        timeout: float = 30.0,
    ):
        """Initialize the OpenSandbox client.

        Args:
            base_url: Base URL of the OpenSandbox server (e.g., "https://opensandbox.fly.dev").
            grpc_port: gRPC port (default: 50051). If None, uses 50051.
            grpc_insecure: Force insecure gRPC even with HTTPS. Useful for Fly.io where
                          gRPC is exposed as raw TCP. If None, auto-detects from URL scheme.
            timeout: Default timeout for HTTP requests in seconds.
        """
        self._base_url = base_url.rstrip("/")
        self._grpc_port = grpc_port or 50051
        self._timeout = timeout
        self._http_client: Optional[httpx.AsyncClient] = None
        self._grpc_channel: Optional[grpc.aio.Channel] = None

        # Parse the URL to get host for gRPC
        parsed = urlparse(self._base_url)
        self._host = parsed.hostname or "localhost"
        # Use secure gRPC for HTTPS unless explicitly set to insecure
        self._grpc_secure = parsed.scheme == "https" and not grpc_insecure

    async def _ensure_connected(self) -> None:
        """Ensure HTTP and gRPC connections are established."""
        if self._http_client is None:
            self._http_client = httpx.AsyncClient(timeout=self._timeout)

        if self._grpc_channel is None:
            grpc_target = f"{self._host}:{self._grpc_port}"
            if self._grpc_secure:
                # Use secure channel for HTTPS
                credentials = grpc.ssl_channel_credentials()
                self._grpc_channel = grpc.aio.secure_channel(grpc_target, credentials)
            else:
                # Use insecure channel for HTTP or when grpc_insecure=True
                self._grpc_channel = grpc.aio.insecure_channel(grpc_target)

    async def create(
        self,
        *,
        env: Optional[Dict[str, str]] = None,
        timeout: int = 300,
    ) -> Sandbox:
        """Create a new sandbox session.

        Args:
            env: Initial environment variables for the sandbox.
            timeout: Sandbox timeout in seconds.

        Returns:
            A Sandbox instance ready for use.

        Raises:
            SandboxConnectionError: If connection to the server fails.
        """
        await self._ensure_connected()

        try:
            response = await self._http_client.post(
                f"{self._base_url}/sessions",
                json={"env": env or {}},
                timeout=timeout,
            )
            response.raise_for_status()
            data = response.json()
            session_id = data["session_id"]
        except httpx.HTTPError as e:
            raise SandboxConnectionError(f"Failed to create sandbox: {e}") from e

        return Sandbox(
            session_id=session_id,
            grpc_channel=self._grpc_channel,
            http_base_url=self._base_url,
            http_client=self._http_client,
        )

    async def close(self) -> None:
        """Close all connections to the server."""
        if self._grpc_channel is not None:
            await self._grpc_channel.close()
            self._grpc_channel = None

        if self._http_client is not None:
            await self._http_client.aclose()
            self._http_client = None

    async def __aenter__(self):
        """Async context manager entry."""
        await self._ensure_connected()
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb):
        """Async context manager exit."""
        await self.close()
        return False
