"""Template management for custom sandbox environments."""

from __future__ import annotations

from dataclasses import dataclass

import httpx


@dataclass
class TemplateInfo:
    """Template metadata."""

    template_id: str
    name: str
    tag: str
    status: str


@dataclass
class Template:
    """Template management operations."""

    _client: httpx.AsyncClient

    @classmethod
    def _from_client(cls, client: httpx.AsyncClient) -> Template:
        return cls(_client=client)

    async def build(self, name: str, dockerfile: str) -> TemplateInfo:
        """Build a new template from a Dockerfile."""
        resp = await self._client.post(
            "/templates",
            json={"name": name, "dockerfile": dockerfile},
            timeout=300.0,
        )
        resp.raise_for_status()
        data = resp.json()
        return TemplateInfo(
            template_id=data["templateID"],
            name=data["name"],
            tag=data.get("tag", "latest"),
            status=data.get("status", "ready"),
        )

    async def list(self) -> list[TemplateInfo]:
        """List all available templates."""
        resp = await self._client.get("/templates")
        resp.raise_for_status()
        return [
            TemplateInfo(
                template_id=t["templateID"],
                name=t["name"],
                tag=t.get("tag", "latest"),
                status=t.get("status", "ready"),
            )
            for t in resp.json()
        ]

    async def get(self, name: str) -> TemplateInfo:
        """Get template details by name."""
        resp = await self._client.get(f"/templates/{name}")
        resp.raise_for_status()
        data = resp.json()
        return TemplateInfo(
            template_id=data["templateID"],
            name=data["name"],
            tag=data.get("tag", "latest"),
            status=data.get("status", "ready"),
        )

    async def delete(self, name: str) -> None:
        """Delete a template."""
        resp = await self._client.delete(f"/templates/{name}")
        resp.raise_for_status()
