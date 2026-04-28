"""Session-based exec API for running commands inside a sandbox."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from typing import Any, Callable
from urllib.parse import quote

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


@dataclass
class ExecSession:
    """A live exec session attached over WebSocket.

    Mirrors the TypeScript SDK: `done` resolves with the exit code, and
    `send_stdin` / `kill` / `close` provide the same control surface.
    """

    session_id: str
    sandbox_id: str
    _ws: Any = field(repr=False, default=None)
    _exit_future: asyncio.Future = field(repr=False, default=None)  # type: ignore[assignment]
    _reader_task: asyncio.Task | None = field(repr=False, default=None)
    _kill_fn: Callable[[str, int], Any] | None = field(repr=False, default=None)

    @property
    def done(self) -> asyncio.Future:
        """Future resolving to the process exit code."""
        return self._exit_future

    async def send_stdin(self, data: str | bytes) -> None:
        """Write to the process stdin. No-op if the connection is closed."""
        if self._ws is None:
            return
        payload = data.encode() if isinstance(data, str) else data
        try:
            await self._ws.send(b"\x00" + payload)
        except Exception:
            pass

    async def kill(self, signal: int = 9) -> None:
        """Kill the underlying process."""
        if self._kill_fn is None:
            return
        await self._kill_fn(self.session_id, signal)

    async def close(self) -> None:
        """Detach from the session. Does not kill the process."""
        if self._ws is not None:
            try:
                await self._ws.close()
            except Exception:
                pass
        if self._reader_task is not None and not self._reader_task.done():
            self._reader_task.cancel()


class Exec:
    """Session-based command execution for a sandbox."""

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
        body: dict[str, Any] = {"cmd": "sh", "args": ["-c", command], "timeout": timeout}
        if env:
            body["envs"] = env
        if cwd:
            body["cwd"] = cwd

        resp = await self._client.post(
            f"/sandboxes/{self._sandbox_id}/exec/run",
            json=body,
        )
        resp.raise_for_status()
        data = resp.json()

        return ProcessResult(
            exit_code=data["exitCode"],
            stdout=data.get("stdout", ""),
            stderr=data.get("stderr", ""),
        )

    async def start(
        self,
        command: str,
        args: list[str] | None = None,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
        timeout: int | None = None,
        max_run_after_disconnect: int | None = None,
        on_stdout: Callable[[bytes], None] | None = None,
        on_stderr: Callable[[bytes], None] | None = None,
        on_exit: Callable[[int], None] | None = None,
        on_scrollback_end: Callable[[], None] | None = None,
    ) -> ExecSession:
        """Start a long-running command and attach for streaming I/O.

        Returns an `ExecSession` with `done` (future resolving to exit code),
        `send_stdin`, `kill`, and `close`. Mirrors the TypeScript SDK.
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
        if max_run_after_disconnect is not None:
            body["maxRunAfterDisconnect"] = max_run_after_disconnect

        resp = await self._client.post(
            f"/sandboxes/{self._sandbox_id}/exec",
            json=body,
        )
        resp.raise_for_status()
        session_id = resp.json()["sessionID"]

        return await self.attach(
            session_id,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_exit=on_exit,
            on_scrollback_end=on_scrollback_end,
        )

    async def background(
        self,
        command: str,
        args: list[str] | None = None,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
        timeout: int | None = None,
        max_run_after_disconnect: int | None = None,
        on_stdout: Callable[[bytes], None] | None = None,
        on_stderr: Callable[[bytes], None] | None = None,
        on_exit: Callable[[int], None] | None = None,
    ) -> ExecSession:
        """Alias for `start`. Use when the intent is "run in the background
        and observe" rather than the more ambiguous "start".
        """
        return await self.start(
            command,
            args=args,
            env=env,
            cwd=cwd,
            timeout=timeout,
            max_run_after_disconnect=max_run_after_disconnect,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_exit=on_exit,
        )

    async def attach(
        self,
        session_id: str,
        on_stdout: Callable[[bytes], None] | None = None,
        on_stderr: Callable[[bytes], None] | None = None,
        on_exit: Callable[[int], None] | None = None,
        on_scrollback_end: Callable[[], None] | None = None,
    ) -> ExecSession:
        """Attach to an existing exec session over WebSocket and route output."""
        ws_base = self._connect_url.replace("http://", "ws://").replace("https://", "wss://")
        if self._token:
            auth = f"?token={quote(self._token)}"
        elif self._api_key:
            auth = f"?api_key={quote(self._api_key)}"
        else:
            auth = ""
        ws_url = f"{ws_base}/sandboxes/{self._sandbox_id}/exec/{session_id}{auth}"

        ws = await websockets.connect(ws_url)
        loop = asyncio.get_event_loop()
        exit_future: asyncio.Future = loop.create_future()
        got_exit = False

        async def reader() -> None:
            nonlocal got_exit
            try:
                async for msg in ws:
                    if not isinstance(msg, (bytes, bytearray)) or len(msg) < 1:
                        continue
                    stream_id = msg[0]
                    payload = bytes(msg[1:])
                    if stream_id == 0x01:
                        if on_stdout:
                            on_stdout(payload)
                    elif stream_id == 0x02:
                        if on_stderr:
                            on_stderr(payload)
                    elif stream_id == 0x03:
                        code = int.from_bytes(payload[:4], "big", signed=True) if len(payload) >= 4 else 0
                        got_exit = True
                        if on_exit:
                            on_exit(code)
                        if not exit_future.done():
                            exit_future.set_result(code)
                    elif stream_id == 0x04:
                        if on_scrollback_end:
                            on_scrollback_end()
            except (websockets.ConnectionClosed, asyncio.CancelledError):
                pass
            except Exception:
                pass
            finally:
                if not got_exit and not exit_future.done():
                    if on_exit:
                        on_exit(-1)
                    exit_future.set_result(-1)

        task = asyncio.create_task(reader())

        return ExecSession(
            session_id=session_id,
            sandbox_id=self._sandbox_id,
            _ws=ws,
            _exit_future=exit_future,
            _reader_task=task,
            _kill_fn=self.kill,
        )

    async def shell(
        self,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
    ):
        """Open a stateful shell session.

        Subsequent `.run()` calls share the same bash process, so cwd,
        exported env vars, and shell functions persist — the ergonomics of
        a terminal tab. Foreground-only: concurrent `.run()` rejects. If
        the user command calls `exit`, the shell closes (same as closing a
        terminal tab) and subsequent `.run()` rejects.
        """
        # Late import to avoid circularity.
        from opencomputer.shell import Shell

        shell_ref: dict[str, Any] = {}

        def _on_stdout(chunk: bytes) -> None:
            sh = shell_ref.get("s")
            if sh is not None:
                sh._on_stdout(chunk)

        def _on_stderr(chunk: bytes) -> None:
            sh = shell_ref.get("s")
            if sh is not None:
                sh._on_stderr(chunk)

        def _on_scrollback_end() -> None:
            sh = shell_ref.get("s")
            if sh is not None:
                sh._on_scrollback_end()

        session = await self.start(
            "bash",
            args=["--noprofile", "--norc", "+m"],
            env=env,
            cwd=cwd,
            on_stdout=_on_stdout,
            on_stderr=_on_stderr,
            on_scrollback_end=_on_scrollback_end,
        )
        shell = Shell(session)
        shell_ref["s"] = shell
        return shell

    async def reattach_shell(self, session_id: str):
        """Re-attach to a shell session opened earlier via `shell()`.

        Useful for revisiting a long-lived terminal tab from a different
        process invocation. Assumes the shell is idle; if another client
        has a run in flight, output interleaves and results are undefined.
        """
        from opencomputer.shell import Shell

        shell_ref: dict[str, Any] = {}

        def _on_stdout(chunk: bytes) -> None:
            sh = shell_ref.get("s")
            if sh is not None:
                sh._on_stdout(chunk)

        def _on_stderr(chunk: bytes) -> None:
            sh = shell_ref.get("s")
            if sh is not None:
                sh._on_stderr(chunk)

        def _on_scrollback_end() -> None:
            sh = shell_ref.get("s")
            if sh is not None:
                sh._on_scrollback_end()

        session = await self.attach(
            session_id,
            on_stdout=_on_stdout,
            on_stderr=_on_stderr,
            on_scrollback_end=_on_scrollback_end,
        )
        shell = Shell(session)
        shell_ref["s"] = shell
        return shell

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
