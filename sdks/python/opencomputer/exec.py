"""Session-based exec API for running commands inside a sandbox."""

from __future__ import annotations

import struct
from dataclasses import dataclass
from typing import Any

import httpx
import websockets


@dataclass
class ProcessResult:
    """Result of a command execution."""

    exit_code: int
    stdout: str
    stderr: str


@dataclass
class ExecSessionInfo:
    """Metadata for an exec session."""

    session_id: str
    sandbox_id: str
    command: str
    args: list[str]
    running: bool
    exit_code: int | None
    started_at: str
    attached_clients: int


class Exec:
    """Session-based command execution for a sandbox."""

    def __init__(
        self,
        client: httpx.AsyncClient,
        sandbox_id: str,
        connect_url: str,
        token: str,
    ):
        self._client = client
        self._sandbox_id = sandbox_id
        self._connect_url = connect_url
        self._token = token

    async def run(
        self,
        command: str,
        timeout: int = 60,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
    ) -> ProcessResult:
        """Run a shell command and wait for completion.

        The command is executed via `sh -c`, so shell features like pipes,
        redirects, and env var expansion work as expected.
        """
        body: dict[str, Any] = {"cmd": "sh", "args": ["-c", command]}
        if env:
            body["envs"] = env
        if cwd:
            body["cwd"] = cwd
        if timeout:
            body["timeout"] = timeout

        resp = await self._client.post(
            f"/sandboxes/{self._sandbox_id}/exec",
            json=body,
        )
        resp.raise_for_status()
        data = resp.json()
        session_id = data["sessionID"]

        # Attach via WebSocket and collect output
        ws_url = self._connect_url.replace("http://", "ws://").replace(
            "https://", "wss://"
        )
        ws_endpoint = f"{ws_url}/sandboxes/{self._sandbox_id}/exec/{session_id}"
        if self._token:
            ws_endpoint += f"?token={self._token}"

        stdout_parts: list[bytes] = []
        stderr_parts: list[bytes] = []
        exit_code = -1

        async with websockets.connect(ws_endpoint) as ws:
            async for message in ws:
                if isinstance(message, str):
                    continue
                if len(message) < 1:
                    continue

                stream_id = message[0]
                payload = message[1:]

                if stream_id == 0x01:  # stdout
                    stdout_parts.append(payload)
                elif stream_id == 0x02:  # stderr
                    stderr_parts.append(payload)
                elif stream_id == 0x03:  # exit
                    if len(payload) >= 4:
                        exit_code = struct.unpack(">i", payload[:4])[0]
                    break
                elif stream_id == 0x04:  # scrollback_end
                    pass

        return ProcessResult(
            exit_code=exit_code,
            stdout=b"".join(stdout_parts).decode("utf-8", errors="replace"),
            stderr=b"".join(stderr_parts).decode("utf-8", errors="replace"),
        )

    async def start(
        self,
        command: str,
        args: list[str] | None = None,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
        timeout: int | None = None,
    ) -> str:
        """Start a long-running command and return the session ID.

        Unlike `run()`, this does not wait for completion. Use `list()` to
        check session status, or `kill()` to stop it.
        """
        body: dict[str, Any] = {"cmd": command}
        if args:
            body["args"] = args
        if env:
            body["envs"] = env
        if cwd:
            body["cwd"] = cwd
        if timeout is not None:
            body["timeout"] = timeout

        resp = await self._client.post(
            f"/sandboxes/{self._sandbox_id}/exec",
            json=body,
        )
        resp.raise_for_status()
        return resp.json()["sessionID"]

    async def list(self) -> list[ExecSessionInfo]:
        """List all exec sessions for this sandbox."""
        resp = await self._client.get(
            f"/sandboxes/{self._sandbox_id}/exec",
        )
        resp.raise_for_status()
        return [
            ExecSessionInfo(
                session_id=s["sessionID"],
                sandbox_id=s.get("sandboxID", ""),
                command=s["command"],
                args=s.get("args") or [],
                running=s["running"],
                exit_code=s.get("exitCode"),
                started_at=s.get("startedAt", ""),
                attached_clients=s.get("attachedClients", 0),
            )
            for s in resp.json()
        ]

    async def kill(self, session_id: str, signal: int = 9) -> None:
        """Kill an exec session."""
        resp = await self._client.post(
            f"/sandboxes/{self._sandbox_id}/exec/{session_id}/kill",
            json={"signal": signal},
        )
        resp.raise_for_status()
