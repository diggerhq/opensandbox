#!/usr/bin/env python3
"""
Real-time chat interface for Claude Agent SDK with OpenSandbox integration.

This provides a WebSocket-based chat interface where you can interact with
Claude in real-time, with code execution happening in an isolated sandbox.
"""

import asyncio
import json
import os
from pathlib import Path
from typing import Optional

from fastapi import FastAPI, WebSocket, WebSocketDisconnect
from fastapi.responses import HTMLResponse
from fastapi.staticfiles import StaticFiles

app = FastAPI(title="Claude Chat with OpenSandbox")

# Store active sandbox sessions per WebSocket connection
active_sessions: dict[str, any] = {}


@app.get("/", response_class=HTMLResponse)
async def get_chat_page():
    """Serve the chat interface."""
    html_path = Path(__file__).parent / "static" / "index.html"
    return HTMLResponse(content=html_path.read_text())


@app.get("/health")
async def health():
    """Health check endpoint."""
    return {"status": "ok"}


async def create_sandbox():
    """Create an OpenSandbox session."""
    try:
        from opensandbox import OpenSandbox

        url = os.environ.get("OPENSANDBOX_URL", "http://localhost:8080")
        grpc_port = int(os.environ.get("OPENSANDBOX_GRPC_PORT", "50051"))

        client = OpenSandbox(url, grpc_port=grpc_port)
        await client._ensure_connected()

        sandbox = await client.create(
            env={
                "ANTHROPIC_API_KEY": os.environ.get("ANTHROPIC_API_KEY", ""),
            }
        )

        return client, sandbox
    except Exception as e:
        print(f"Failed to create sandbox: {e}")
        return None, None


async def run_claude_query(prompt: str, websocket: WebSocket, sandbox=None):
    """Run a Claude Agent SDK query and stream results via WebSocket."""
    try:
        from claude_code_sdk import query, ClaudeCodeOptions

        # Send start message
        await websocket.send_json({
            "type": "start",
            "message": "Processing your request..."
        })

        # Configure options with timeout
        options = ClaudeCodeOptions(
            max_turns=10,
            timeout_ms=300000,  # 5 minute timeout for entire conversation
        )

        # Stream responses
        full_response = ""
        async for message in query(prompt=prompt, options=options):
            # Handle different message types
            if hasattr(message, 'content'):
                # Text content
                for block in message.content if isinstance(message.content, list) else [message.content]:
                    if hasattr(block, 'text'):
                        text = block.text
                        full_response += text
                        await websocket.send_json({
                            "type": "text",
                            "content": text
                        })
                    elif hasattr(block, 'type') and block.type == 'tool_use':
                        await websocket.send_json({
                            "type": "tool_use",
                            "tool": block.name if hasattr(block, 'name') else "unknown",
                            "input": str(block.input) if hasattr(block, 'input') else ""
                        })

            elif hasattr(message, 'result'):
                await websocket.send_json({
                    "type": "result",
                    "content": str(message.result)
                })

        # Send completion message
        await websocket.send_json({
            "type": "complete",
            "message": "Request completed"
        })

    except Exception as e:
        await websocket.send_json({
            "type": "error",
            "message": str(e)
        })


async def run_in_sandbox(command: str, sandbox) -> dict:
    """Execute a command in the sandbox."""
    try:
        result = await sandbox.run(command, timeout_ms=60000)
        return {
            "stdout": result.stdout,
            "stderr": result.stderr,
            "exit_code": result.exit_code
        }
    except Exception as e:
        return {
            "error": str(e)
        }


@app.websocket("/ws/chat")
async def websocket_chat(websocket: WebSocket):
    """WebSocket endpoint for real-time chat."""
    await websocket.accept()

    client = None
    sandbox = None

    try:
        # Send welcome message
        await websocket.send_json({
            "type": "system",
            "message": "Connected to Claude Chat. Type a message to start!"
        })

        # Create sandbox session
        await websocket.send_json({
            "type": "system",
            "message": "Creating sandbox session..."
        })

        client, sandbox = await create_sandbox()

        if sandbox:
            await websocket.send_json({
                "type": "system",
                "message": f"Sandbox ready: {sandbox.session_id}"
            })

            # Check available tools
            result = await sandbox.run("which node claude git")
            await websocket.send_json({
                "type": "system",
                "message": f"Available tools: {result.stdout.strip()}"
            })
        else:
            await websocket.send_json({
                "type": "system",
                "message": "Running without sandbox (OpenSandbox not available)"
            })

        # Main chat loop
        while True:
            # Receive message from client
            data = await websocket.receive_text()

            try:
                msg = json.loads(data)
                prompt = msg.get("message", data)
            except json.JSONDecodeError:
                prompt = data

            if not prompt.strip():
                continue

            # Check for special commands
            if prompt.startswith("/sandbox "):
                # Run command in sandbox
                if sandbox:
                    cmd = prompt[9:]
                    await websocket.send_json({
                        "type": "system",
                        "message": f"Running in sandbox: {cmd}"
                    })
                    result = await run_in_sandbox(cmd, sandbox)
                    await websocket.send_json({
                        "type": "sandbox_result",
                        "result": result
                    })
                else:
                    await websocket.send_json({
                        "type": "error",
                        "message": "Sandbox not available"
                    })
            elif prompt == "/help":
                await websocket.send_json({
                    "type": "system",
                    "message": """Available commands:
- Just type to chat with Claude
- /sandbox <command> - Run a command in the sandbox
- /help - Show this help message"""
                })
            else:
                # Regular chat with Claude
                await run_claude_query(prompt, websocket, sandbox)

    except WebSocketDisconnect:
        print("Client disconnected")
    except Exception as e:
        print(f"WebSocket error: {e}")
        try:
            await websocket.send_json({
                "type": "error",
                "message": str(e)
            })
        except:
            pass
    finally:
        # Cleanup sandbox
        if sandbox:
            try:
                await sandbox.destroy()
            except:
                pass
        if client:
            try:
                await client.close()
            except:
                pass


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8000)
