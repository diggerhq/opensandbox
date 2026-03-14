"""Secret store management — CRUD for secret stores and encrypted secrets."""

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


class SecretStore:
    """Static methods for managing secret stores and their secrets."""

    @staticmethod
    async def create(
        name: str,
        egress_allowlist: list[str] | None = None,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> dict:
        """Create a new secret store.

        Args:
            name: Store name (unique per org).
            egress_allowlist: Allowed egress hosts (e.g. ["api.anthropic.com"]).
            api_key: API key (or OPENCOMPUTER_API_KEY env var).
            api_url: API URL (or OPENCOMPUTER_API_URL env var).

        Returns:
            Secret store info dict with id, name, egressAllowlist, etc.
        """
        body: dict[str, Any] = {"name": name}
        if egress_allowlist:
            body["egressAllowlist"] = egress_allowlist

        async with _get_client(api_key, api_url) as client:
            resp = await client.post("/secret-stores", json=body)
            resp.raise_for_status()
            return resp.json()

    @staticmethod
    async def list(
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> list[dict]:
        """List all secret stores for the authenticated org."""
        async with _get_client(api_key, api_url) as client:
            resp = await client.get("/secret-stores")
            resp.raise_for_status()
            return resp.json()

    @staticmethod
    async def get(
        store_id: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> dict:
        """Get secret store details by ID."""
        async with _get_client(api_key, api_url) as client:
            resp = await client.get(f"/secret-stores/{store_id}")
            resp.raise_for_status()
            return resp.json()

    @staticmethod
    async def update(
        store_id: str,
        name: str = "",
        egress_allowlist: list[str] | None = None,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> dict:
        """Update a secret store's configuration.

        Args:
            store_id: UUID of the store to update.
            name: New store name (empty = no change).
            egress_allowlist: New allowed egress hosts (None = no change).
            api_key: API key (or OPENCOMPUTER_API_KEY env var).
            api_url: API URL (or OPENCOMPUTER_API_URL env var).

        Returns:
            Updated secret store info dict.
        """
        body: dict[str, Any] = {}
        if name:
            body["name"] = name
        if egress_allowlist is not None:
            body["egressAllowlist"] = egress_allowlist

        async with _get_client(api_key, api_url) as client:
            resp = await client.put(f"/secret-stores/{store_id}", json=body)
            resp.raise_for_status()
            return resp.json()

    @staticmethod
    async def delete(
        store_id: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> None:
        """Delete a secret store and all its secrets."""
        async with _get_client(api_key, api_url) as client:
            resp = await client.delete(f"/secret-stores/{store_id}")
            resp.raise_for_status()

    # ── Secret Entries ──────────────────────────────────────────────────────

    @staticmethod
    async def set_secret(
        store_id: str,
        name: str,
        value: str,
        allowed_hosts: list[str] | None = None,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> None:
        """Set (create or update) an encrypted secret in a store.

        Args:
            store_id: UUID of the secret store.
            name: Secret name (used as env var name in sandboxes).
            value: Secret value (encrypted at rest, never returned by API).
            allowed_hosts: Restrict this secret to specific hosts only.
            api_key: API key (or OPENCOMPUTER_API_KEY env var).
            api_url: API URL (or OPENCOMPUTER_API_URL env var).
        """
        body: dict[str, Any] = {"value": value}
        if allowed_hosts:
            body["allowedHosts"] = allowed_hosts

        async with _get_client(api_key, api_url) as client:
            resp = await client.put(
                f"/secret-stores/{store_id}/secrets/{name}",
                json=body,
            )
            resp.raise_for_status()

    @staticmethod
    async def delete_secret(
        store_id: str,
        name: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> None:
        """Delete a secret from a store."""
        async with _get_client(api_key, api_url) as client:
            resp = await client.delete(f"/secret-stores/{store_id}/secrets/{name}")
            if resp.status_code != 404:
                resp.raise_for_status()

    @staticmethod
    async def list_secrets(
        store_id: str,
        api_key: str | None = None,
        api_url: str | None = None,
    ) -> list[dict]:
        """List secret entries in a store (names and allowed hosts, no values).

        Returns:
            List of secret entry dicts with name, allowedHosts, etc.
        """
        async with _get_client(api_key, api_url) as client:
            resp = await client.get(f"/secret-stores/{store_id}/secrets")
            resp.raise_for_status()
            return resp.json()
