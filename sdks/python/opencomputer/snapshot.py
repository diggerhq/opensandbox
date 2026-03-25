"""Snapshot management for OpenSandbox - pre-built named image checkpoints."""

from __future__ import annotations

import os
from typing import Any, Callable

import httpx

from opencomputer.image import Image
from opencomputer.sse import parse_sse_stream


class Snapshots:
    """Manage pre-built snapshots (named, persistent image checkpoints).

    Example::

        snapshots = Snapshots()

        image = (
            Image.base()
            .apt_install(["python3", "python3-pip"])
            .pip_install(["pandas", "numpy"])
        )

        await snapshots.create(name="data-science", image=image)
        info = await snapshots.get("data-science")
        all_snapshots = await snapshots.list()
        await snapshots.delete("data-science")
    """

    def __init__(
        self,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> None:
        url = api_url or os.environ.get("OPENCOMPUTER_API_URL", "https://app.opencomputer.dev")
        url = url.rstrip("/")
        key = api_key or os.environ.get("OPENCOMPUTER_API_KEY", "")

        api_base = url if url.endswith("/api") else f"{url}/api"

        self._headers: dict[str, str] = {}
        if key:
            self._headers["X-API-Key"] = key

        self._api_base = api_base
        self._client = httpx.AsyncClient(base_url=api_base, headers=self._headers, timeout=300.0)

    async def create(
        self,
        name: str,
        image: Image,
        on_build_logs: Callable[[str], None] | None = None,
    ) -> dict[str, Any]:
        """Create a pre-built snapshot from a declarative image.

        Args:
            name: Unique name for this snapshot.
            image: Declarative Image definition.
            on_build_logs: Optional callback for build log streaming.

        Returns:
            Snapshot info dict with id, name, status, etc.
        """
        body = {"name": name, "image": image.to_dict()}

        # Always use SSE streaming — builds can take minutes and
        # non-streaming requests will hit Cloudflare 524 timeouts.
        headers = {**self._headers, "Accept": "text/event-stream"}
        log_fn = on_build_logs if on_build_logs is not None else lambda _: None
        client = httpx.AsyncClient(
            base_url=self._api_base, headers=headers, timeout=600.0,
        )
        try:
            async with client.stream("POST", "/snapshots", json=body) as resp:
                resp.raise_for_status()
                return await parse_sse_stream(resp, log_fn)
        finally:
            await client.aclose()

    async def list(self) -> list[dict[str, Any]]:
        """List all named snapshots for the current org."""
        resp = await self._client.get("/snapshots")
        resp.raise_for_status()
        return resp.json()

    async def get(self, name: str) -> dict[str, Any]:
        """Get a snapshot by name."""
        resp = await self._client.get(f"/snapshots/{name}")
        resp.raise_for_status()
        return resp.json()

    async def delete(self, name: str) -> None:
        """Delete a named snapshot."""
        resp = await self._client.delete(f"/snapshots/{name}")
        if resp.status_code != 404:
            resp.raise_for_status()

    async def close(self) -> None:
        """Close the HTTP client."""
        await self._client.aclose()
