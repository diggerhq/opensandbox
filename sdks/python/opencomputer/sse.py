"""Shared SSE stream parser for build log streaming."""

from __future__ import annotations

import json
from typing import Any, Callable

import httpx


async def parse_sse_stream(
    resp: httpx.Response,
    on_log: Callable[[str], None],
) -> dict[str, Any]:
    """Parse an SSE stream from the server.

    The server emits three event types:
      - build_log: streamed during image build, contains { step, type, message }
      - error: build failed, contains { error: string }
      - result: final result JSON

    Args:
        resp: httpx streaming response with content-type text/event-stream
        on_log: Callback for build log messages

    Returns:
        The parsed result dict from the "result" event

    Raises:
        RuntimeError: If the build fails or no result is received
    """
    result: dict[str, Any] | None = None
    buffer = ""

    async for chunk in resp.aiter_text():
        buffer += chunk
        events = buffer.split("\n\n")
        buffer = events[-1]

        for event in events[:-1]:
            if not event.strip():
                continue

            event_type = ""
            data = ""
            for line in event.split("\n"):
                if line.startswith("event: "):
                    event_type = line[7:]
                elif line.startswith("data: "):
                    data = line[6:]

            if not data:
                continue

            if event_type == "build_log":
                try:
                    parsed = json.loads(data)
                    on_log(parsed.get("message", data))
                except (json.JSONDecodeError, TypeError):
                    on_log(data)
            elif event_type == "error":
                msg = data
                try:
                    msg = json.loads(data).get("error", data)
                except (json.JSONDecodeError, TypeError):
                    pass
                raise RuntimeError(f"Build failed: {msg}")
            elif event_type == "result":
                result = json.loads(data)

    if not result:
        raise RuntimeError("No result received from build stream")
    return result
