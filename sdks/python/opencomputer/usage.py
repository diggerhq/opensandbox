"""Usage + tags — per-sandbox and per-tag spend attribution.

All numeric fields are GB-seconds. Dollars live in Stripe; the SDK
mirrors the server's physical-quantity model so invoices stay the
source of truth for currency.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import httpx


@dataclass
class UsageSandboxItem:
    sandbox_id: str
    memory_gb_seconds: float
    disk_overage_gb_seconds: float
    tags: dict[str, str] = field(default_factory=dict)
    tags_last_updated_at: str | None = None
    alias: str | None = None
    status: str | None = None


@dataclass
class UsageTagItem:
    tag_key: str
    tag_value: str
    memory_gb_seconds: float
    disk_overage_gb_seconds: float
    sandbox_count: int


@dataclass
class UsageTotals:
    memory_gb_seconds: float
    disk_overage_gb_seconds: float


@dataclass
class UsageUntaggedBucket:
    memory_gb_seconds: float
    disk_overage_gb_seconds: float
    sandbox_count: int


@dataclass
class UsageBySandboxResponse:
    from_: str
    to: str
    total: UsageTotals
    items: list[UsageSandboxItem]
    next_cursor: str | None


@dataclass
class UsageByTagResponse:
    from_: str
    to: str
    group_by: str
    total: UsageTotals
    untagged: UsageUntaggedBucket
    items: list[UsageTagItem]
    next_cursor: str | None


@dataclass
class SandboxUsageResponse:
    sandbox_id: str
    from_: str
    to: str
    memory_gb_seconds: float
    disk_overage_gb_seconds: float
    tags: dict[str, str]
    tags_last_updated_at: str | None
    first_started_at: str | None
    last_ended_at: str | None
    alias: str | None = None
    status: str | None = None


@dataclass
class TagKeyInfo:
    key: str
    sandbox_count: int
    value_count: int


def _build_params(
    *,
    group_by: str | None = None,
    from_: str | None = None,
    to: str | None = None,
    sort: str | None = None,
    limit: int | None = None,
    cursor: str | None = None,
    filter: dict[str, str] | None = None,
) -> list[tuple[str, str]]:
    """Build a flat list of (key, value) query params.

    v1 filter contract: one ``filter[tag:<key>]`` param per dimension.
    Comma-separated values within a param are OR'd; different dimension
    params are AND'd. The server rejects repeating the same
    ``filter[...]`` key — which is naturally prevented here because the
    ``filter`` argument is a ``dict[str, str]``.
    """
    params: list[tuple[str, str]] = []
    if group_by is not None:
        params.append(("groupBy", group_by))
    if from_ is not None:
        params.append(("from", from_))
    if to is not None:
        params.append(("to", to))
    if sort is not None:
        params.append(("sort", sort))
    if limit is not None:
        params.append(("limit", str(limit)))
    if cursor is not None:
        params.append(("cursor", cursor))
    if filter:
        for k, v in filter.items():
            params.append((f"filter[{k}]", v))
    return params


@dataclass
class Usage:
    """Usage aggregator. ``by_sandbox`` / ``by_tag`` call
    ``GET /usage``; ``for_sandbox`` is the drilldown."""

    _client: httpx.AsyncClient

    @classmethod
    def _from_client(cls, client: httpx.AsyncClient) -> "Usage":
        return cls(_client=client)

    async def by_sandbox(
        self,
        *,
        from_: str | None = None,
        to: str | None = None,
        filter: dict[str, str] | None = None,
        sort: str | None = None,
        limit: int | None = None,
        cursor: str | None = None,
    ) -> UsageBySandboxResponse:
        params = _build_params(
            group_by="sandbox",
            from_=from_,
            to=to,
            sort=sort,
            limit=limit,
            cursor=cursor,
            filter=filter,
        )
        resp = await self._client.get("/usage", params=params)
        resp.raise_for_status()
        body = resp.json()
        return UsageBySandboxResponse(
            from_=body["from"],
            to=body["to"],
            total=UsageTotals(**_camel_to_snake(body["total"])),
            items=[
                UsageSandboxItem(
                    sandbox_id=i["sandboxId"],
                    memory_gb_seconds=i["memoryGbSeconds"],
                    disk_overage_gb_seconds=i["diskOverageGbSeconds"],
                    tags=i.get("tags") or {},
                    tags_last_updated_at=i.get("tagsLastUpdatedAt"),
                    alias=i.get("alias"),
                    status=i.get("status"),
                )
                for i in body.get("items") or []
            ],
            next_cursor=body.get("nextCursor"),
        )

    async def by_tag(
        self,
        tag_key: str,
        *,
        from_: str | None = None,
        to: str | None = None,
        filter: dict[str, str] | None = None,
        sort: str | None = None,
        limit: int | None = None,
        cursor: str | None = None,
    ) -> UsageByTagResponse:
        params = _build_params(
            group_by=f"tag:{tag_key}",
            from_=from_,
            to=to,
            sort=sort,
            limit=limit,
            cursor=cursor,
            filter=filter,
        )
        resp = await self._client.get("/usage", params=params)
        resp.raise_for_status()
        body = resp.json()
        u = body.get("untagged") or {"memoryGbSeconds": 0, "diskOverageGbSeconds": 0, "sandboxCount": 0}
        return UsageByTagResponse(
            from_=body["from"],
            to=body["to"],
            group_by=body["groupBy"],
            total=UsageTotals(**_camel_to_snake(body["total"])),
            untagged=UsageUntaggedBucket(
                memory_gb_seconds=u["memoryGbSeconds"],
                disk_overage_gb_seconds=u["diskOverageGbSeconds"],
                sandbox_count=u["sandboxCount"],
            ),
            items=[
                UsageTagItem(
                    tag_key=i["tagKey"],
                    tag_value=i["tagValue"],
                    memory_gb_seconds=i["memoryGbSeconds"],
                    disk_overage_gb_seconds=i["diskOverageGbSeconds"],
                    sandbox_count=i["sandboxCount"],
                )
                for i in body.get("items") or []
            ],
            next_cursor=body.get("nextCursor"),
        )

    async def for_sandbox(
        self,
        sandbox_id: str,
        *,
        from_: str | None = None,
        to: str | None = None,
    ) -> SandboxUsageResponse:
        params = _build_params(from_=from_, to=to)
        resp = await self._client.get(f"/sandboxes/{sandbox_id}/usage", params=params)
        resp.raise_for_status()
        b = resp.json()
        return SandboxUsageResponse(
            sandbox_id=b["sandboxId"],
            from_=b["from"],
            to=b["to"],
            memory_gb_seconds=b["memoryGbSeconds"],
            disk_overage_gb_seconds=b["diskOverageGbSeconds"],
            tags=b.get("tags") or {},
            tags_last_updated_at=b.get("tagsLastUpdatedAt"),
            first_started_at=b.get("firstStartedAt"),
            last_ended_at=b.get("lastEndedAt"),
            alias=b.get("alias"),
            status=b.get("status"),
        )


@dataclass
class Tags:
    """Tag management. ``list_keys`` is org-wide discovery;
    ``get`` and ``set`` operate on one sandbox."""

    _client: httpx.AsyncClient

    @classmethod
    def _from_client(cls, client: httpx.AsyncClient) -> "Tags":
        return cls(_client=client)

    async def list_keys(self) -> list[TagKeyInfo]:
        resp = await self._client.get("/tags")
        resp.raise_for_status()
        return [
            TagKeyInfo(key=k["key"], sandbox_count=k["sandboxCount"], value_count=k["valueCount"])
            for k in resp.json().get("keys") or []
        ]

    async def get(self, sandbox_id: str) -> tuple[dict[str, str], str | None]:
        """Return (tags, tagsLastUpdatedAt)."""
        resp = await self._client.get(f"/sandboxes/{sandbox_id}/tags")
        resp.raise_for_status()
        body = resp.json()
        return body.get("tags") or {}, body.get("tagsLastUpdatedAt")

    async def set(self, sandbox_id: str, tags: dict[str, str]) -> tuple[dict[str, str], str | None]:
        """Full replace. ``{}`` clears all tags."""
        resp = await self._client.put(f"/sandboxes/{sandbox_id}/tags", json=tags)
        resp.raise_for_status()
        body = resp.json()
        return body.get("tags") or {}, body.get("tagsLastUpdatedAt")


def _camel_to_snake(d: dict[str, Any]) -> dict[str, Any]:
    """Narrow helper — only covers the two keys on UsageTotals."""
    return {
        "memory_gb_seconds": d.get("memoryGbSeconds", 0.0),
        "disk_overage_gb_seconds": d.get("diskOverageGbSeconds", 0.0),
    }
