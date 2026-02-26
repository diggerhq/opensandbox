"""Filesystem operations inside a sandbox."""

from __future__ import annotations

from dataclasses import dataclass

import httpx


@dataclass
class EntryInfo:
    """File or directory entry."""

    name: str
    is_dir: bool
    path: str
    size: int = 0


@dataclass
class Filesystem:
    """Filesystem operations for a sandbox."""

    _client: httpx.AsyncClient
    _sandbox_id: str

    async def read(self, path: str) -> str:
        """Read a file as text."""
        resp = await self._client.get(
            f"/sandboxes/{self._sandbox_id}/files",
            params={"path": path},
        )
        resp.raise_for_status()
        return resp.text

    async def read_bytes(self, path: str) -> bytes:
        """Read a file as bytes."""
        resp = await self._client.get(
            f"/sandboxes/{self._sandbox_id}/files",
            params={"path": path},
        )
        resp.raise_for_status()
        return resp.content

    async def write(self, path: str, content: str | bytes) -> None:
        """Write content to a file."""
        data = content if isinstance(content, bytes) else content.encode()
        resp = await self._client.put(
            f"/sandboxes/{self._sandbox_id}/files",
            params={"path": path},
            content=data,
        )
        resp.raise_for_status()

    async def list(self, path: str = "/") -> list[EntryInfo]:
        """List directory contents."""
        resp = await self._client.get(
            f"/sandboxes/{self._sandbox_id}/files/list",
            params={"path": path},
        )
        resp.raise_for_status()
        data = resp.json()
        if data is None:
            return []
        return [
            EntryInfo(
                name=entry["name"],
                is_dir=entry.get("isDir", False),
                path=entry.get("path", ""),
                size=entry.get("size", 0),
            )
            for entry in data
        ]

    async def make_dir(self, path: str) -> None:
        """Create a directory (and parents)."""
        resp = await self._client.post(
            f"/sandboxes/{self._sandbox_id}/files/mkdir",
            params={"path": path},
        )
        resp.raise_for_status()

    async def remove(self, path: str) -> None:
        """Remove a file or directory."""
        resp = await self._client.delete(
            f"/sandboxes/{self._sandbox_id}/files",
            params={"path": path},
        )
        resp.raise_for_status()

    async def exists(self, path: str) -> bool:
        """Check if a path exists."""
        try:
            resp = await self._client.get(
                f"/sandboxes/{self._sandbox_id}/files",
                params={"path": path},
            )
            return resp.status_code == 200
        except httpx.HTTPError:
            return False
