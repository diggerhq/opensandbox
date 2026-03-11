"""Project management — CRUD for projects and encrypted secrets."""

from __future__ import annotations

import os
from typing import Any

import httpx


def _get_client(
    api_key: str | None = None,
    api_url: str | None = None,
) -> httpx.AsyncClient:
    url = api_url or os.environ.get("OPENCOMPUTER_API_URL", "https://app.opencomputer.dev")
    url = url.rstrip("/")
    key = api_key or os.environ.get("OPENCOMPUTER_API_KEY", "")

    api_base = url if url.endswith("/api") else f"{url}/api"

    headers: dict[str, str] = {}
    if key:
        headers["X-API-Key"] = key

    return httpx.AsyncClient(base_url=api_base, headers=headers, timeout=30.0)


class Project:
    """Static methods for managing projects and their secrets."""

    @staticmethod
    async def create(
        name: str,
        template: str = "",
        cpu_count: int = 0,
        memory_mb: int = 0,
        timeout_sec: int = 0,
        egress_allowlist: list[str] | None = None,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> dict:
        """Create a new project.

        Args:
            name: Project name (unique per org).
            template: Default template for sandboxes in this project.
            cpu_count: Default vCPU count.
            memory_mb: Default memory in MB.
            timeout_sec: Default sandbox timeout in seconds.
            egress_allowlist: Allowed egress hosts (e.g. ["api.anthropic.com"]).
            api_key: API key (or OPENCOMPUTER_API_KEY env var).
            api_url: API URL (or OPENCOMPUTER_API_URL env var).

        Returns:
            Project info dict with id, name, template, etc.
        """
        body: dict[str, Any] = {"name": name}
        if template:
            body["template"] = template
        if cpu_count:
            body["cpuCount"] = cpu_count
        if memory_mb:
            body["memoryMB"] = memory_mb
        if timeout_sec:
            body["timeoutSec"] = timeout_sec
        if egress_allowlist:
            body["egressAllowlist"] = egress_allowlist

        async with _get_client(api_key, api_url) as client:
            resp = await client.post("/projects", json=body)
            resp.raise_for_status()
            return resp.json()

    @staticmethod
    async def list(
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> list[dict]:
        """List all projects for the authenticated org."""
        async with _get_client(api_key, api_url) as client:
            resp = await client.get("/projects")
            resp.raise_for_status()
            return resp.json()

    @staticmethod
    async def get(
        project_id: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> dict:
        """Get project details by ID."""
        async with _get_client(api_key, api_url) as client:
            resp = await client.get(f"/projects/{project_id}")
            resp.raise_for_status()
            return resp.json()

    @staticmethod
    async def update(
        project_id: str,
        name: str = "",
        template: str = "",
        cpu_count: int = 0,
        memory_mb: int = 0,
        timeout_sec: int = 0,
        egress_allowlist: list[str] | None = None,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> dict:
        """Update a project's configuration.

        Args:
            project_id: UUID of the project to update.
            name: New project name (empty = no change).
            template: New default template (empty = no change).
            cpu_count: New default vCPU count (0 = no change).
            memory_mb: New default memory in MB (0 = no change).
            timeout_sec: New default timeout in seconds (0 = no change).
            egress_allowlist: New allowed egress hosts (None = no change).
            api_key: API key (or OPENCOMPUTER_API_KEY env var).
            api_url: API URL (or OPENCOMPUTER_API_URL env var).

        Returns:
            Updated project info dict.
        """
        body: dict[str, Any] = {}
        if name:
            body["name"] = name
        if template:
            body["template"] = template
        if cpu_count:
            body["cpuCount"] = cpu_count
        if memory_mb:
            body["memoryMB"] = memory_mb
        if timeout_sec:
            body["timeoutSec"] = timeout_sec
        if egress_allowlist is not None:
            body["egressAllowlist"] = egress_allowlist

        async with _get_client(api_key, api_url) as client:
            resp = await client.put(f"/projects/{project_id}", json=body)
            resp.raise_for_status()
            return resp.json()

    @staticmethod
    async def delete(
        project_id: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> None:
        """Delete a project and all its secrets."""
        async with _get_client(api_key, api_url) as client:
            resp = await client.delete(f"/projects/{project_id}")
            resp.raise_for_status()

    # ── Secrets ──────────────────────────────────────────────────────────────

    @staticmethod
    async def set_secret(
        project_id: str,
        name: str,
        value: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> None:
        """Set (create or update) an encrypted secret on a project.

        Args:
            project_id: UUID of the project.
            name: Secret name (used as env var name in sandboxes).
            value: Secret value (encrypted at rest, never returned by API).
            api_key: API key (or OPENCOMPUTER_API_KEY env var).
            api_url: API URL (or OPENCOMPUTER_API_URL env var).
        """
        async with _get_client(api_key, api_url) as client:
            resp = await client.put(
                f"/projects/{project_id}/secrets/{name}",
                json={"value": value},
            )
            resp.raise_for_status()

    @staticmethod
    async def delete_secret(
        project_id: str,
        name: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> None:
        """Delete a secret from a project."""
        async with _get_client(api_key, api_url) as client:
            resp = await client.delete(f"/projects/{project_id}/secrets/{name}")
            if resp.status_code != 404:
                resp.raise_for_status()

    @staticmethod
    async def list_secrets(
        project_id: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> list[str]:
        """List secret names for a project (values are never returned).

        Returns:
            List of secret names (e.g. ["ANTHROPIC_API_KEY", "DATABASE_URL"]).
        """
        async with _get_client(api_key, api_url) as client:
            resp = await client.get(f"/projects/{project_id}/secrets")
            resp.raise_for_status()
            return resp.json()
