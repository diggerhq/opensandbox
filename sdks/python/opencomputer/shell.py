"""Stateful shell sessions for a sandbox — the ergonomics of a terminal tab.

See the TypeScript SDK's `Shell` for the companion implementation. The two
SDKs share the same sentinel protocol: bash is run with `--noprofile
--norc`, commands are piped via stdin, and each command is framed by
per-call DONE/EXIT markers (stdout + stderr respectively) so we can resolve
only after both pipes have drained.
"""

from __future__ import annotations

import asyncio
import uuid
from typing import Callable

from opencomputer.exec import ExecSession, ProcessResult


class ShellBusyError(Exception):
    """Raised when `shell.run()` is called while another run is in flight."""

    def __init__(self) -> None:
        super().__init__(
            "shell is already running a command; shell.run is foreground-only"
        )


class ShellClosedError(Exception):
    """Raised when `shell.run()` is called after the shell has exited.

    This also fires if the user command calls `exit` — same as closing a
    terminal tab.
    """


class Shell:
    """A long-lived bash process with state preserved across `.run()` calls."""

    def __init__(self, session: ExecSession):
        self._session = session
        self.session_id = session.session_id
        self._state = "idle"  # idle | running | closed
        self._token = ""
        self._stdout_done_marker = ""
        self._stderr_exit_prefix = ""
        self._stdout_buf = ""
        self._stderr_buf = ""
        self._stdout_scan_idx = 0
        self._stderr_scan_idx = 0
        self._stdout_done_idx: int | None = None
        self._stderr_exit_code: int | None = None
        self._stderr_exit_idx: int | None = None
        self._current_on_stdout: Callable[[bytes], None] | None = None
        self._current_on_stderr: Callable[[bytes], None] | None = None
        self._current_future: asyncio.Future | None = None
        # Drop inbound chunks until the server sends `0x04` (scrollback_end).
        # Without this, reattaching to a shell with prior runs would feed
        # old sentinels into the parser.
        self._scrollback_done = False

        # If the bash process exits while a run is pending, fail that run.
        def _on_session_done(fut: asyncio.Future) -> None:
            prev = self._state
            self._state = "closed"
            if self._current_future is not None and not self._current_future.done():
                try:
                    code = fut.result() if not fut.cancelled() else -1
                except Exception:
                    code = -1
                if prev != "idle":
                    self._current_future.set_exception(
                        ShellClosedError(f"bash exited with code {code}")
                    )

        session.done.add_done_callback(_on_session_done)

    def _on_scrollback_end(self) -> None:
        self._scrollback_done = True

    def _on_stdout(self, chunk: bytes) -> None:
        if not self._scrollback_done:
            return
        if self._state != "running":
            return
        self._stdout_buf += chunk.decode("utf-8", errors="replace")
        self._scan_stdout()

    def _on_stderr(self, chunk: bytes) -> None:
        if not self._scrollback_done:
            return
        if self._state != "running":
            return
        self._stderr_buf += chunk.decode("utf-8", errors="replace")
        self._scan_stderr()

    def _scan_stdout(self) -> None:
        if self._current_future is None or self._stdout_done_idx is not None:
            return

        marker = self._stdout_done_marker
        idx = self._stdout_buf.find(marker, self._stdout_scan_idx)

        if idx == -1:
            hold = len(marker) - 1
            safe = max(self._stdout_scan_idx, len(self._stdout_buf) - hold)
            if safe > self._stdout_scan_idx and self._current_on_stdout is not None:
                chunk = self._stdout_buf[self._stdout_scan_idx : safe]
                self._current_on_stdout(chunk.encode("utf-8"))
            if safe > self._stdout_scan_idx:
                self._stdout_scan_idx = safe
            return

        if idx > self._stdout_scan_idx and self._current_on_stdout is not None:
            chunk = self._stdout_buf[self._stdout_scan_idx : idx]
            self._current_on_stdout(chunk.encode("utf-8"))
        if idx > self._stdout_scan_idx:
            self._stdout_scan_idx = idx

        self._stdout_done_idx = idx
        self._try_complete()

    def _scan_stderr(self) -> None:
        if self._current_future is None or self._stderr_exit_code is not None:
            return

        prefix = self._stderr_exit_prefix
        idx = self._stderr_buf.find(prefix, self._stderr_scan_idx)

        if idx == -1:
            hold = len(prefix) - 1
            safe = max(self._stderr_scan_idx, len(self._stderr_buf) - hold)
            if safe > self._stderr_scan_idx and self._current_on_stderr is not None:
                chunk = self._stderr_buf[self._stderr_scan_idx : safe]
                self._current_on_stderr(chunk.encode("utf-8"))
            if safe > self._stderr_scan_idx:
                self._stderr_scan_idx = safe
            return

        if idx > self._stderr_scan_idx and self._current_on_stderr is not None:
            chunk = self._stderr_buf[self._stderr_scan_idx : idx]
            self._current_on_stderr(chunk.encode("utf-8"))
        if idx > self._stderr_scan_idx:
            self._stderr_scan_idx = idx

        after_prefix = idx + len(prefix)
        close_idx = self._stderr_buf.find("__", after_prefix)
        if close_idx == -1:
            return  # wait for more bytes

        exit_str = self._stderr_buf[after_prefix:close_idx]
        try:
            exit_code = int(exit_str)
        except ValueError:
            fut = self._current_future
            self._state = "closed"
            self._current_future = None
            if fut is not None and not fut.done():
                fut.set_exception(ShellClosedError(f"corrupt sentinel: {exit_str!r}"))
            return

        self._stderr_exit_code = exit_code
        self._stderr_exit_idx = idx
        self._try_complete()

    def _try_complete(self) -> None:
        fut = self._current_future
        if fut is None:
            return
        if self._stdout_done_idx is None or self._stderr_exit_code is None:
            return

        result = ProcessResult(
            exit_code=self._stderr_exit_code,
            stdout=self._stdout_buf[: self._stdout_done_idx],
            stderr=self._stderr_buf[
                : self._stderr_exit_idx
                if self._stderr_exit_idx is not None
                else len(self._stderr_buf)
            ],
        )

        self._current_future = None
        self._current_on_stdout = None
        self._current_on_stderr = None
        self._stdout_buf = ""
        self._stderr_buf = ""
        self._stdout_scan_idx = 0
        self._stderr_scan_idx = 0
        self._stdout_done_idx = None
        self._stderr_exit_code = None
        self._stderr_exit_idx = None
        self._token = ""
        self._stdout_done_marker = ""
        self._stderr_exit_prefix = ""
        if self._state != "closed":
            self._state = "idle"

        if not fut.done():
            fut.set_result(result)

    async def run(
        self,
        cmd: str,
        on_stdout: Callable[[bytes], None] | None = None,
        on_stderr: Callable[[bytes], None] | None = None,
    ) -> ProcessResult:
        """Run a command inside the shell and wait for it to complete.

        Per-call cwd/env/timeout are intentionally not supported in v1 — use
        inline shell syntax (`cd /x && cmd`, `FOO=bar cmd`). Timeouts will
        land once we have a "signal foreground job" primitive.
        """
        if self._state == "closed":
            raise ShellClosedError("shell is closed")
        if self._state == "running":
            raise ShellBusyError()

        token = uuid.uuid4().hex
        self._token = token
        self._stdout_done_marker = f"\n__OC_{token}_DONE__\n"
        self._stderr_exit_prefix = f"\n__OC_{token}_EXIT_"

        inner = cmd.strip() or ":"
        # `{ cmd\n}` groups so `$?` captures the user's exit. Newline before
        # `}` is required — `{ cmd\n; }` is a bash syntax error. DONE goes
        # to stdout, EXIT to stderr; we resolve only when both are seen to
        # avoid a race where the stderr sentinel arrives before the user's
        # stdout is drained (stdout/stderr are read on separate goroutines
        # on the agent, so wire ordering isn't guaranteed).
        wrapped = (
            f"{{ {inner}\n}} ; __oc_ec=$? ; "
            f"printf '\\n__OC_%s_DONE__\\n' '{token}' ; "
            f"printf '\\n__OC_%s_EXIT_%d__\\n' '{token}' \"$__oc_ec\" >&2\n"
        )

        self._state = "running"
        self._stdout_buf = ""
        self._stderr_buf = ""
        self._stdout_scan_idx = 0
        self._stderr_scan_idx = 0
        self._stdout_done_idx = None
        self._stderr_exit_code = None
        self._stderr_exit_idx = None
        self._current_on_stdout = on_stdout
        self._current_on_stderr = on_stderr

        loop = asyncio.get_event_loop()
        self._current_future = loop.create_future()

        try:
            await self._session.send_stdin(wrapped)
        except Exception:
            self._state = "closed"
            fut = self._current_future
            self._current_future = None
            if fut is not None and not fut.done():
                fut.cancel()
            raise

        return await self._current_future

    async def close(self) -> None:
        """Write `exit` to bash and wait for it to terminate."""
        if self._state == "closed":
            return
        self._state = "closed"
        try:
            await self._session.send_stdin("exit\n")
        except Exception:
            pass
        try:
            await self._session.done
        except Exception:
            pass
        await self._session.close()
