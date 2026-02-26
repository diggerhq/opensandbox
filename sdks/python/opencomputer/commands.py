"""Command execution inside a sandbox."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import httpx


@dataclass
class ProcessResult:
    """Result of a command execution."""

    exit_code: int
    stdout: str
    stderr: str


@dataclass
class Commands:
    """Command execution for a sandbox."""

    _client: httpx.AsyncClient
    _sandbox_id: str

    async def run(
        self,
        command: str,
        timeout: int = 60,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
    ) -> ProcessResult:
        """Run a command and wait for completion."""
        body: dict[str, Any] = {
            "cmd": command,
            "timeout": timeout,
        }
        if env:
            body["envs"] = env
        if cwd:
            body["cwd"] = cwd

        resp = await self._client.post(
            f"/sandboxes/{self._sandbox_id}/commands",
            json=body,
            timeout=timeout + 5,
        )
        resp.raise_for_status()
        data = resp.json()

        return ProcessResult(
            exit_code=data.get("exitCode", -1),
            stdout=data.get("stdout", ""),
            stderr=data.get("stderr", ""),
        )
