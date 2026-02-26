"""PTY terminal sessions inside a sandbox."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from typing import Callable

import httpx
import websockets


@dataclass
class PtySession:
    """An active PTY terminal session."""

    session_id: str
    sandbox_id: str
    _ws: websockets.WebSocketClientProtocol | None = None
    _read_task: asyncio.Task | None = None

    async def send(self, data: str | bytes) -> None:
        """Send input to the terminal."""
        if self._ws is None:
            raise RuntimeError("PTY session not connected")
        payload = data if isinstance(data, bytes) else data.encode()
        await self._ws.send(payload)

    async def recv(self) -> bytes:
        """Receive output from the terminal."""
        if self._ws is None:
            raise RuntimeError("PTY session not connected")
        data = await self._ws.recv()
        return data if isinstance(data, bytes) else data.encode()

    async def close(self) -> None:
        """Close the PTY session."""
        if self._read_task and not self._read_task.done():
            self._read_task.cancel()
        if self._ws:
            await self._ws.close()


@dataclass
class Pty:
    """PTY terminal session manager for a sandbox."""

    _client: httpx.AsyncClient
    _sandbox_id: str
    _api_url: str
    _api_key: str

    async def create(
        self,
        cols: int = 80,
        rows: int = 24,
        on_output: Callable[[bytes], None] | None = None,
    ) -> PtySession:
        """Create a new PTY session and connect via WebSocket."""
        # Create session via REST
        resp = await self._client.post(
            f"/sandboxes/{self._sandbox_id}/pty",
            json={"cols": cols, "rows": rows},
        )
        resp.raise_for_status()
        data = resp.json()
        session_id = data["sessionID"]

        # Connect via WebSocket
        ws_url = self._api_url.replace("http://", "ws://").replace("https://", "wss://")
        ws_url = f"{ws_url}/sandboxes/{self._sandbox_id}/pty/{session_id}"

        headers = {}
        if self._api_key:
            headers["X-API-Key"] = self._api_key

        ws = await websockets.connect(ws_url, additional_headers=headers)

        session = PtySession(
            session_id=session_id,
            sandbox_id=self._sandbox_id,
            _ws=ws,
        )

        if on_output:
            async def _reader() -> None:
                try:
                    async for msg in ws:
                        output = msg if isinstance(msg, bytes) else msg.encode()
                        on_output(output)
                except websockets.ConnectionClosed:
                    pass

            session._read_task = asyncio.create_task(_reader())

        return session
