"""Agent session API for running Claude Agent SDK inside a sandbox."""

from __future__ import annotations

import json
import struct
from dataclasses import dataclass
from typing import Any, Callable

import httpx
import websockets


@dataclass
class AgentEvent:
    """A structured event from the Claude agent."""

    type: str
    data: dict[str, Any]

    def __getitem__(self, key: str) -> Any:
        if key == "type":
            return self.type
        return self.data[key]

    def get(self, key: str, default: Any = None) -> Any:
        if key == "type":
            return self.type
        return self.data.get(key, default)


@dataclass
class AgentSessionInfo:
    """Metadata for an agent session."""

    session_id: str
    sandbox_id: str
    running: bool
    started_at: str


class Agent:
    """Claude Agent SDK integration for a sandbox."""

    def __init__(
        self,
        client: httpx.AsyncClient,
        sandbox_id: str,
        connect_url: str,
        token: str,
        api_key: str = "",
    ):
        self._client = client
        self._sandbox_id = sandbox_id
        self._connect_url = connect_url
        self._token = token
        self._api_key = api_key

    async def start(
        self,
        prompt: str | None = None,
        model: str | None = None,
        system_prompt: str | None = None,
        allowed_tools: list[str] | None = None,
        permission_mode: str | None = None,
        max_turns: int | None = None,
        cwd: str | None = None,
        mcp_servers: dict[str, Any] | None = None,
        on_event: Callable[[AgentEvent], None] | None = None,
        on_error: Callable[[str], None] | None = None,
    ) -> AgentSession:
        """Start a new agent session.

        Args:
            prompt: Initial prompt to send to the agent.
            model: Claude model to use (default: claude-sonnet-4-20250514).
            system_prompt: Custom system instructions.
            allowed_tools: Built-in tools to enable.
            permission_mode: Permission mode for tool execution.
            max_turns: Max agentic turns per prompt.
            cwd: Working directory inside the sandbox.
            mcp_servers: MCP server configurations.
            on_event: Callback for structured agent events.
            on_error: Callback for stderr/error output.

        Returns:
            An AgentSession that can be used to interact with the agent.
        """
        body: dict[str, Any] = {}
        if prompt:
            body["prompt"] = prompt
        if model:
            body["model"] = model
        if system_prompt:
            body["systemPrompt"] = system_prompt
        if allowed_tools:
            body["allowedTools"] = allowed_tools
        if permission_mode:
            body["permissionMode"] = permission_mode
        if max_turns is not None:
            body["maxTurns"] = max_turns
        if cwd:
            body["cwd"] = cwd
        if mcp_servers:
            body["mcpServers"] = mcp_servers

        resp = await self._client.post(
            f"/sandboxes/{self._sandbox_id}/agent",
            json=body,
        )
        resp.raise_for_status()
        data = resp.json()
        session_id = data["sessionID"]

        return await self.attach(
            session_id,
            on_event=on_event,
            on_error=on_error,
        )

    async def attach(
        self,
        session_id: str,
        on_event: Callable[[AgentEvent], None] | None = None,
        on_error: Callable[[str], None] | None = None,
    ) -> AgentSession:
        """Attach to an existing agent session.

        Reconnects via WebSocket and replays scrollback events.

        Args:
            session_id: The session ID to attach to.
            on_event: Callback for structured agent events.
            on_error: Callback for stderr/error output.

        Returns:
            An AgentSession for interaction.
        """
        ws_url = self._connect_url.replace("http://", "ws://").replace(
            "https://", "wss://"
        )
        ws_endpoint = f"{ws_url}/sandboxes/{self._sandbox_id}/exec/{session_id}"
        # WebSocket API cannot set custom headers, so pass credentials as query params.
        # Prefer JWT token (direct worker access); fall back to API key (control plane).
        if self._token:
            ws_endpoint += f"?token={self._token}"
        elif self._api_key:
            ws_endpoint += f"?api_key={self._api_key}"

        ws = await websockets.connect(ws_endpoint)

        return AgentSession(
            session_id=session_id,
            sandbox_id=self._sandbox_id,
            ws=ws,
            client=self._client,
            on_event=on_event,
            on_error=on_error,
        )

    async def list(self) -> list[AgentSessionInfo]:
        """List all agent sessions for this sandbox."""
        resp = await self._client.get(
            f"/sandboxes/{self._sandbox_id}/agent",
        )
        resp.raise_for_status()
        return [
            AgentSessionInfo(
                session_id=s["sessionID"],
                sandbox_id=s.get("sandboxID", ""),
                running=s["running"],
                started_at=s.get("startedAt", ""),
            )
            for s in resp.json()
        ]


class AgentSession:
    """A connected agent session with WebSocket streaming."""

    def __init__(
        self,
        session_id: str,
        sandbox_id: str,
        ws: websockets.WebSocketClientProtocol,
        client: httpx.AsyncClient,
        on_event: Callable[[AgentEvent], None] | None = None,
        on_error: Callable[[str], None] | None = None,
    ):
        self.session_id = session_id
        self.sandbox_id = sandbox_id
        self._ws = ws
        self._client = client
        self._on_event = on_event
        self._on_error = on_error
        self._exit_code: int | None = None
        self._line_buf = ""

    def _parse_lines(self, text: str) -> None:
        """Parse JSON lines from stdout data."""
        self._line_buf += text
        lines = self._line_buf.split("\n")
        self._line_buf = lines.pop()  # Keep incomplete last line
        for line in lines:
            trimmed = line.strip()
            if not trimmed:
                continue
            try:
                data = json.loads(trimmed)
                event_type = data.pop("type", "unknown")
                if self._on_event:
                    self._on_event(AgentEvent(type=event_type, data=data))
            except json.JSONDecodeError:
                if self._on_error:
                    self._on_error(f"non-JSON stdout: {trimmed}")

    async def collect_events(self) -> list[AgentEvent]:
        """Collect all events until the process exits.

        Returns:
            List of all agent events received.
        """
        events: list[AgentEvent] = []
        original_on_event = self._on_event

        def capture(event: AgentEvent) -> None:
            events.append(event)
            if original_on_event:
                original_on_event(event)

        self._on_event = capture

        try:
            await self.wait()
        finally:
            self._on_event = original_on_event

        return events

    async def wait(self) -> int:
        """Wait for the agent process to exit.

        Returns:
            The process exit code.
        """
        try:
            async for message in self._ws:
                if isinstance(message, str):
                    continue
                if len(message) < 1:
                    continue

                stream_id = message[0]
                payload = message[1:]

                if stream_id == 0x01:  # stdout — JSON lines
                    self._parse_lines(payload.decode("utf-8", errors="replace"))
                elif stream_id == 0x02:  # stderr
                    if self._on_error:
                        self._on_error(payload.decode("utf-8", errors="replace"))
                elif stream_id == 0x03:  # exit
                    if len(payload) >= 4:
                        self._exit_code = struct.unpack(">i", payload[:4])[0]
                    else:
                        self._exit_code = 0
                    break
                elif stream_id == 0x04:  # scrollback_end
                    pass
        except websockets.ConnectionClosed:
            if self._exit_code is None:
                self._exit_code = -1

        # Flush remaining line buffer
        if self._line_buf.strip():
            try:
                data = json.loads(self._line_buf.strip())
                event_type = data.pop("type", "unknown")
                if self._on_event:
                    self._on_event(AgentEvent(type=event_type, data=data))
            except json.JSONDecodeError:
                if self._on_error:
                    self._on_error(f"non-JSON stdout: {self._line_buf.strip()}")
            self._line_buf = ""

        return self._exit_code if self._exit_code is not None else -1

    def send_prompt(self, text: str) -> None:
        """Send a prompt to the agent."""
        self._send_stdin(json.dumps({"type": "prompt", "text": text}) + "\n")

    def interrupt(self) -> None:
        """Interrupt the current agent operation."""
        self._send_stdin(json.dumps({"type": "interrupt"}) + "\n")

    def configure(
        self,
        model: str | None = None,
        system_prompt: str | None = None,
        allowed_tools: list[str] | None = None,
        permission_mode: str | None = None,
        max_turns: int | None = None,
        cwd: str | None = None,
    ) -> None:
        """Reconfigure the agent between turns."""
        config: dict[str, Any] = {"type": "configure"}
        if model:
            config["model"] = model
        if system_prompt:
            config["systemPrompt"] = system_prompt
        if allowed_tools:
            config["allowedTools"] = allowed_tools
        if permission_mode:
            config["permissionMode"] = permission_mode
        if max_turns is not None:
            config["maxTurns"] = max_turns
        if cwd:
            config["cwd"] = cwd
        self._send_stdin(json.dumps(config) + "\n")

    async def kill(self, signal: int = 9) -> None:
        """Kill the agent session."""
        resp = await self._client.post(
            f"/sandboxes/{self.sandbox_id}/agent/{self.session_id}/kill",
            json={"signal": signal},
        )
        resp.raise_for_status()

    async def close(self) -> None:
        """Close the WebSocket connection."""
        await self._ws.close()

    def _send_stdin(self, data: str) -> None:
        """Send data to the agent process stdin via WebSocket."""
        payload = data.encode("utf-8")
        msg = bytes([0x00]) + payload
        # websockets library expects awaitable send, but for fire-and-forget
        # we use the sync path via ensure_future
        import asyncio
        asyncio.ensure_future(self._ws.send(msg))
