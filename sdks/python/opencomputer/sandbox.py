"""Sandbox class - main entry point for the OpenSandbox SDK."""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import Any, Callable

import httpx

from opencomputer.agent import Agent
from opencomputer.exec import Exec
from opencomputer.filesystem import Filesystem
from opencomputer.image import Image
from opencomputer.pty import Pty
from opencomputer.sse import parse_sse_stream


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
        timeout: int = 0,
        api_key: str | None = None,
        api_url: str | None = None,
        envs: dict[str, str] | None = None,
        metadata: dict[str, str] | None = None,
        disk_mb: int | None = None,
        secret_store: str | None = None,
        image: Image | None = None,
        snapshot: str | None = None,
        on_build_log: Callable[[str], None] | None = None,
    ) -> Sandbox:
        """Create a new sandbox instance.

        Args:
            template: Template to use (default "base").
            timeout: Idle timeout in seconds. 0 = persistent, never auto-hibernates (default).
            api_key: API key (or OPENCOMPUTER_API_KEY env var).
            api_url: API URL (or OPENCOMPUTER_API_URL env var).
            envs: Environment variables to inject. Overrides store secrets.
            metadata: Custom metadata key-value pairs.
            disk_mb: Workspace disk size in MB (default 20480 = 20GB). Any
                additional GB above 20GB is metered at a per-second rate
                comparable to EBS gp3. Closed beta: requests above 20GB
                require the org's ``max_disk_mb`` to be raised. Contact us:
                https://cal.com/team/digger/opencomputer-founder-chat
            secret_store: Secret store name — resolves encrypted secrets
                and egress allowlist.
            image: Declarative Image definition. The server builds and caches it as a checkpoint.
            snapshot: Name of a pre-built snapshot to create the sandbox from.
            on_build_log: Callback for build log streaming when using image/snapshot.
        """
        url = api_url or os.environ.get("OPENCOMPUTER_API_URL", "https://app.opencomputer.dev")
        url = url.rstrip("/")
        key = api_key or os.environ.get("OPENCOMPUTER_API_KEY", "")

        # Control plane client always uses /api prefix
        api_base = url if url.endswith("/api") else f"{url}/api"

        headers: dict[str, str] = {}
        if key:
            headers["X-API-Key"] = key

        # Always use SSE for image/snapshot creation to keep the connection alive
        # through proxies (Cloudflare has a 100s idle timeout).
        use_sse = image is not None or snapshot is not None
        if use_sse:
            headers["Accept"] = "text/event-stream"

        # Image builds may take longer
        client_timeout = 300.0 if image else 30.0
        client = httpx.AsyncClient(base_url=api_base, headers=headers, timeout=client_timeout)

        body: dict[str, Any] = {
            "templateID": template,
            "timeout": timeout,
        }
        if envs:
            body["envs"] = envs
        if metadata:
            body["metadata"] = metadata
        if disk_mb is not None:
            body["diskMB"] = disk_mb
        if secret_store:
            body["secretStore"] = secret_store
        if image is not None:
            body["image"] = image.to_dict()
        if snapshot is not None:
            body["snapshot"] = snapshot

        if use_sse:
            data = await cls._create_with_sse(client, body, on_build_log)
        else:
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
    async def _create_with_sse(
        cls,
        client: httpx.AsyncClient,
        body: dict[str, Any],
        on_build_log: Callable[[str], None] | None,
    ) -> dict[str, Any]:
        """Create sandbox with SSE build log streaming."""
        async with client.stream("POST", "/sandboxes", json=body) as resp:
            resp.raise_for_status()
            return await parse_sse_stream(resp, on_build_log or (lambda _: None))

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
        """Return the client for data operations. Always goes through the CP,
        which handles readiness waiting and proxies to workers."""
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
        # Route to worker directly (like commands/files/pty) — the control plane
        # rejects this call in server mode.
        resp = await self._ops_client.post(
            f"/sandboxes/{self.sandbox_id}/timeout",
            json={"timeout": timeout},
        )
        resp.raise_for_status()

    async def download_url(self, path: str, *, expires_in: int = 3600) -> str:
        """Generate a signed download URL for a sandbox file.

        The URL can be used by anyone (e.g. in a browser) without an API key.

        Args:
            path: Absolute path inside the sandbox.
            expires_in: URL validity in seconds (default: 3600, max: 86400).
        """
        resp = await self._client.post(
            f"/sandboxes/{self.sandbox_id}/files/download-url",
            json={"path": path, "expiresIn": expires_in},
        )
        resp.raise_for_status()
        return resp.json()["url"]

    async def upload_url(self, path: str, *, expires_in: int = 3600) -> str:
        """Generate a signed upload URL for a sandbox file.

        The URL can be used by anyone to PUT file content without an API key.

        Args:
            path: Absolute path inside the sandbox.
            expires_in: URL validity in seconds (default: 3600, max: 86400).
        """
        resp = await self._client.post(
            f"/sandboxes/{self.sandbox_id}/files/upload-url",
            json={"path": path, "expiresIn": expires_in},
        )
        resp.raise_for_status()
        return resp.json()["url"]

    @property
    def agent(self) -> Agent:
        """Access Claude Agent SDK sessions."""
        return Agent(self._ops_client, self.sandbox_id, self._connect_url, self._token, self._api_key)

    @property
    def files(self) -> Filesystem:
        """Access filesystem operations."""
        return Filesystem(self._ops_client, self.sandbox_id)

    @property
    def exec(self) -> Exec:
        """Access session-based command execution."""
        return Exec(self._ops_client, self.sandbox_id, self._connect_url, self._token)

    @property
    def commands(self) -> Exec:
        """Backwards-compatible alias for ``exec``. Prefer ``sandbox.exec`` instead."""
        return self.exec

    @property
    def pty(self) -> Pty:
        """Access PTY terminal sessions."""
        pty_url = self._connect_url or self._api_url
        pty_key = self._token or self._api_key
        return Pty(self._ops_client, self.sandbox_id, pty_url, pty_key)

    async def create_checkpoint(self, name: str) -> dict:
        """Create a named checkpoint of the running sandbox.

        Args:
            name: A unique name for this checkpoint.

        Returns:
            Checkpoint info dict with id, sandboxId, name, status, etc.
        """
        resp = await self._client.post(
            f"/sandboxes/{self.sandbox_id}/checkpoints",
            json={"name": name},
        )
        resp.raise_for_status()
        return resp.json()

    async def list_checkpoints(self) -> list[dict]:
        """List all checkpoints for this sandbox."""
        resp = await self._client.get(f"/sandboxes/{self.sandbox_id}/checkpoints")
        resp.raise_for_status()
        return resp.json()

    async def restore_checkpoint(self, checkpoint_id: str) -> None:
        """Restore the sandbox to a previous checkpoint (in-place revert).

        The VM is rebooted from the checkpoint's drives. After restore,
        internal clients are refreshed automatically.

        Args:
            checkpoint_id: UUID of the checkpoint to restore.
        """
        resp = await self._client.post(
            f"/sandboxes/{self.sandbox_id}/checkpoints/{checkpoint_id}/restore",
        )
        resp.raise_for_status()

        # Refresh connection info since the VM was rebooted
        info = await self._client.get(f"/sandboxes/{self.sandbox_id}")
        info.raise_for_status()
        data = info.json()
        self._connect_url = data.get("connectURL", "")
        self._token = data.get("token", "")
        if self._connect_url and self._token:
            if self._data_client is not None:
                await self._data_client.aclose()
            self._data_client = httpx.AsyncClient(
                base_url=self._connect_url,
                headers={"Authorization": f"Bearer {self._token}"},
                timeout=30.0,
            )

    @classmethod
    async def create_from_checkpoint(
        cls,
        checkpoint_id: str,
        timeout: int = 0,
        api_key: str | None = None,
        api_url: str | None = None,
        envs: dict[str, str] | None = None,
        secret_store: str | None = None,
    ) -> Sandbox:
        """Create a new sandbox from an existing checkpoint (fork).

        Args:
            checkpoint_id: UUID of the checkpoint to fork from.
            timeout: Idle timeout in seconds. 0 = persistent, never auto-hibernates (default).
            api_key: API key (or OPENCOMPUTER_API_KEY env var).
            api_url: API URL (or OPENCOMPUTER_API_URL env var).
            envs: Environment variables to override on the fork.
            secret_store: Secret store name to attach. If the checkpoint
                already has a store, secrets are merged (new store wins
                on collision, egress allowlists aggregate).
        """
        url = api_url or os.environ.get("OPENCOMPUTER_API_URL", "https://app.opencomputer.dev")
        url = url.rstrip("/")
        key = api_key or os.environ.get("OPENCOMPUTER_API_KEY", "")

        api_base = url if url.endswith("/api") else f"{url}/api"

        headers = {}
        if key:
            headers["X-API-Key"] = key

        client = httpx.AsyncClient(base_url=api_base, headers=headers, timeout=120.0)

        body: dict[str, Any] = {"timeout": timeout}
        if envs:
            body["envs"] = envs
        if secret_store:
            body["secretStore"] = secret_store

        resp = await client.post(
            f"/sandboxes/from-checkpoint/{checkpoint_id}",
            json=body,
        )
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
            sandbox_id=data["sandboxID"],
            status=data.get("status", "running"),
            _api_url=url,
            _api_key=key,
            _connect_url=connect_url,
            _token=token,
            _client=client,
            _data_client=data_client,
        )

    @staticmethod
    async def create_checkpoint_patch(
        checkpoint_id: str,
        script: str,
        description: str = "",
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> dict:
        """Create a patch for a checkpoint (applied on next wake/boot).

        Args:
            checkpoint_id: UUID of the checkpoint to patch.
            script: Bash script to execute on each forked sandbox.
            description: Human-readable description of the patch.
            api_key: API key (or OPENCOMPUTER_API_KEY env var).
            api_url: API URL (or OPENCOMPUTER_API_URL env var).

        Returns:
            Dict with "patch" info (id, sequence, script, etc.).
        """
        url = api_url or os.environ.get("OPENCOMPUTER_API_URL", "https://app.opencomputer.dev")
        url = url.rstrip("/")
        key = api_key or os.environ.get("OPENCOMPUTER_API_KEY", "")

        api_base = url if url.endswith("/api") else f"{url}/api"

        headers = {}
        if key:
            headers["X-API-Key"] = key

        async with httpx.AsyncClient(base_url=api_base, headers=headers, timeout=300.0) as client:
            resp = await client.post(
                f"/sandboxes/checkpoints/{checkpoint_id}/patches",
                json={"script": script, "description": description},
            )
            resp.raise_for_status()
            return resp.json()

    @staticmethod
    async def list_checkpoint_patches(
        checkpoint_id: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> list[dict]:
        """List all patches for a checkpoint, ordered by sequence.

        Args:
            checkpoint_id: UUID of the checkpoint.
            api_key: API key (or OPENCOMPUTER_API_KEY env var).
            api_url: API URL (or OPENCOMPUTER_API_URL env var).

        Returns:
            List of patch dicts with id, sequence, script, strategy, etc.
        """
        url = api_url or os.environ.get("OPENCOMPUTER_API_URL", "https://app.opencomputer.dev")
        url = url.rstrip("/")
        key = api_key or os.environ.get("OPENCOMPUTER_API_KEY", "")

        api_base = url if url.endswith("/api") else f"{url}/api"

        headers = {}
        if key:
            headers["X-API-Key"] = key

        async with httpx.AsyncClient(base_url=api_base, headers=headers, timeout=30.0) as client:
            resp = await client.get(f"/sandboxes/checkpoints/{checkpoint_id}/patches")
            resp.raise_for_status()
            return resp.json()

    @staticmethod
    async def delete_checkpoint_patch(
        checkpoint_id: str,
        patch_id: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> None:
        """Delete a patch from a checkpoint.

        Args:
            checkpoint_id: UUID of the checkpoint.
            patch_id: UUID of the patch to delete.
            api_key: API key (or OPENCOMPUTER_API_KEY env var).
            api_url: API URL (or OPENCOMPUTER_API_URL env var).
        """
        url = api_url or os.environ.get("OPENCOMPUTER_API_URL", "https://app.opencomputer.dev")
        url = url.rstrip("/")
        key = api_key or os.environ.get("OPENCOMPUTER_API_KEY", "")

        api_base = url if url.endswith("/api") else f"{url}/api"

        headers = {}
        if key:
            headers["X-API-Key"] = key

        async with httpx.AsyncClient(base_url=api_base, headers=headers, timeout=30.0) as client:
            resp = await client.delete(
                f"/sandboxes/checkpoints/{checkpoint_id}/patches/{patch_id}"
            )
            if resp.status_code != 404:
                resp.raise_for_status()

    async def delete_checkpoint(self, checkpoint_id: str) -> None:
        """Delete a checkpoint.

        Args:
            checkpoint_id: UUID of the checkpoint to delete.
        """
        resp = await self._client.delete(
            f"/sandboxes/{self.sandbox_id}/checkpoints/{checkpoint_id}",
        )
        if resp.status_code != 404:
            resp.raise_for_status()

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
