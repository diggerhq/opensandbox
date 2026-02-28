"""Sandbox class - main entry point for the OpenSandbox SDK."""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import Any

import httpx

from opencomputer.commands import Commands
from opencomputer.filesystem import Filesystem
from opencomputer.pty import Pty


@dataclass
class Sandbox:
    """E2B-compatible sandbox interface."""

    sandbox_id: str
    status: str = "running"
    template: str = ""
    _api_url: str = ""
    _api_key: str = ""
    _connect_url: str = ""
    _token: str = ""
    _client: httpx.AsyncClient = field(default=None, repr=False)
    _data_client: httpx.AsyncClient = field(default=None, repr=False)

    @classmethod
    async def create(
        cls,
        template: str = "base",
        timeout: int = 300,
        api_key: str | None = None,
        api_url: str | None = None,
        envs: dict[str, str] | None = None,
        metadata: dict[str, str] | None = None,
    ) -> Sandbox:
        """Create a new sandbox instance."""
        url = api_url or os.environ.get("OPENCOMPUTER_API_URL", "https://app.opencomputer.dev")
        url = url.rstrip("/")
        key = api_key or os.environ.get("OPENCOMPUTER_API_KEY", "")

        # Control plane client always uses /api prefix
        api_base = url if url.endswith("/api") else f"{url}/api"

        headers = {}
        if key:
            headers["X-API-Key"] = key

        client = httpx.AsyncClient(base_url=api_base, headers=headers, timeout=30.0)

        body: dict[str, Any] = {
            "templateID": template,
            "timeout": timeout,
        }
        if envs:
            body["envs"] = envs
        if metadata:
            body["metadata"] = metadata

        resp = await client.post("/sandboxes", json=body)
        resp.raise_for_status()
        data = resp.json()

        connect_url = data.get("connectURL", "")
        token = data.get("token", "")

        # If worker returned a direct connectURL, create a separate client for data ops
        data_client = None
        if connect_url and token:
            data_client = httpx.AsyncClient(
                base_url=connect_url,
                headers={"Authorization": f"Bearer {token}"},
                timeout=30.0,
            )

        return cls(
            sandbox_id=data["sandboxID"],
            status=data.get("status", "running"),
            template=template,
            _api_url=url,
            _api_key=key,
            _connect_url=connect_url,
            _token=token,
            _client=client,
            _data_client=data_client,
        )

    @classmethod
    async def connect(
        cls,
        sandbox_id: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> Sandbox:
        """Connect to an existing sandbox."""
        url = api_url or os.environ.get("OPENCOMPUTER_API_URL", "https://app.opencomputer.dev")
        url = url.rstrip("/")
        key = api_key or os.environ.get("OPENCOMPUTER_API_KEY", "")

        api_base = url if url.endswith("/api") else f"{url}/api"

        headers = {}
        if key:
            headers["X-API-Key"] = key

        client = httpx.AsyncClient(base_url=api_base, headers=headers, timeout=30.0)

        resp = await client.get(f"/sandboxes/{sandbox_id}")
        resp.raise_for_status()
        data = resp.json()

        connect_url = data.get("connectURL", "")
        token = data.get("token", "")

        data_client = None
        if connect_url and token:
            data_client = httpx.AsyncClient(
                base_url=connect_url,
                headers={"Authorization": f"Bearer {token}"},
                timeout=30.0,
            )

        return cls(
            sandbox_id=sandbox_id,
            status=data.get("status", "running"),
            template=data.get("templateID", ""),
            _api_url=url,
            _api_key=key,
            _connect_url=connect_url,
            _token=token,
            _client=client,
            _data_client=data_client,
        )

    @property
    def _ops_client(self) -> httpx.AsyncClient:
        """Return the client for data operations (direct worker if available, else CP)."""
        if self._data_client is not None:
            return self._data_client
        return self._client

    async def kill(self) -> None:
        """Kill and remove the sandbox."""
        resp = await self._client.delete(f"/sandboxes/{self.sandbox_id}")
        resp.raise_for_status()
        self.status = "stopped"

    async def is_running(self) -> bool:
        """Check if the sandbox is still running."""
        try:
            resp = await self._client.get(f"/sandboxes/{self.sandbox_id}")
            resp.raise_for_status()
            data = resp.json()
            self.status = data.get("status", "stopped")
            return self.status == "running"
        except httpx.HTTPStatusError:
            return False

    async def set_timeout(self, timeout: int) -> None:
        """Update the sandbox timeout in seconds."""
        resp = await self._client.post(
            f"/sandboxes/{self.sandbox_id}/timeout",
            json={"timeout": timeout},
        )
        resp.raise_for_status()

    @property
    def files(self) -> Filesystem:
        """Access filesystem operations."""
        return Filesystem(self._ops_client, self.sandbox_id)

    @property
    def commands(self) -> Commands:
        """Access command execution."""
        return Commands(self._ops_client, self.sandbox_id)

    @property
    def pty(self) -> Pty:
        """Access PTY terminal sessions."""
        pty_url = self._connect_url or self._api_url
        pty_key = self._token or self._api_key
        return Pty(self._ops_client, self.sandbox_id, pty_url, pty_key)

    async def create_preview_url(self, port: int, domain: str | None = None, auth_config: dict | None = None) -> dict:
        """Create a preview URL targeting a specific container port.

        Args:
            port: The container port to expose (1-65535).
            domain: Optional custom domain (must be verified on the org).
            auth_config: Optional auth configuration for the preview URL.
        """
        body: dict = {"port": port, "authConfig": auth_config or {}}
        if domain:
            body["domain"] = domain
        resp = await self._client.post(
            f"/sandboxes/{self.sandbox_id}/preview",
            json=body,
        )
        resp.raise_for_status()
        return resp.json()

    async def list_preview_urls(self) -> list[dict]:
        """List all preview URLs for this sandbox."""
        resp = await self._client.get(f"/sandboxes/{self.sandbox_id}/preview")
        resp.raise_for_status()
        return resp.json()

    async def delete_preview_url(self, port: int) -> None:
        """Delete the preview URL for a specific port."""
        resp = await self._client.delete(f"/sandboxes/{self.sandbox_id}/preview/{port}")
        if resp.status_code != 404:
            resp.raise_for_status()

    async def close(self) -> None:
        """Close the HTTP client (does not kill the sandbox)."""
        await self._client.aclose()
        if self._data_client is not None:
            await self._data_client.aclose()

    async def __aenter__(self) -> Sandbox:
        return self

    async def __aexit__(self, *args: object) -> None:
        await self.kill()
        await self.close()
