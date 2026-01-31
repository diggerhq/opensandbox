"""Simplified sandbox interfaces for benchmarking."""

import os
import sys
import time
import httpx
from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import Optional

# Add SDK path for local development
SDK_PATH = os.path.join(os.path.dirname(__file__), "..", "sdk", "python")
if os.path.exists(SDK_PATH):
    sys.path.insert(0, SDK_PATH)


@dataclass
class CommandResult:
    """Result of a command execution."""
    stdout: str
    stderr: str
    exit_code: int
    duration_ms: float = 0.0


class BaseSandbox(ABC):
    """Base sandbox interface for benchmarking."""

    @property
    @abstractmethod
    def name(self) -> str:
        """Provider name."""
        pass

    @abstractmethod
    def create(self, timeout: int = 300) -> str:
        """Create a sandbox, return session ID."""
        pass

    @abstractmethod
    def run_command(self, command: str) -> CommandResult:
        """Run a command in the sandbox."""
        pass

    @abstractmethod
    def write_file(self, path: str, content: str) -> float:
        """Write a file, return duration in ms."""
        pass

    @abstractmethod
    def read_file(self, path: str) -> tuple[str, float]:
        """Read a file, return (content, duration_ms)."""
        pass

    @abstractmethod
    def destroy(self) -> None:
        """Destroy the sandbox."""
        pass


class OpenSandboxClient(BaseSandbox):
    """OpenSandbox HTTP client for benchmarking (legacy, uses HTTP only)."""

    def __init__(self, base_url: str = "http://localhost:8080"):
        self.base_url = base_url
        self.session_id: Optional[str] = None
        self.client = httpx.Client(timeout=300)

    @property
    def name(self) -> str:
        return "opensandbox-http"

    def create(self, timeout: int = 300) -> str:
        response = self.client.post(
            f"{self.base_url}/sessions",
            json={"env": {}}
        )
        response.raise_for_status()
        self.session_id = response.json()["session_id"]
        return self.session_id

    def run_command(self, command: str) -> CommandResult:
        if not self.session_id:
            raise RuntimeError("Session not created")

        start = time.perf_counter()
        response = self.client.post(
            f"{self.base_url}/sessions/{self.session_id}/run",
            json={
                "command": ["/bin/sh", "-c", command],
                "time": 300000,
                "mem": 2097152,
            }
        )
        duration_ms = (time.perf_counter() - start) * 1000

        if response.status_code != 200:
            return CommandResult(
                stdout="",
                stderr=response.text,
                exit_code=1,
                duration_ms=duration_ms
            )

        data = response.json()
        exit_code = data.get("exit_code", 0)
        if data.get("signal"):
            exit_code = 128 + data["signal"]

        return CommandResult(
            stdout=data.get("stdout", ""),
            stderr=data.get("stderr", ""),
            exit_code=exit_code,
            duration_ms=duration_ms
        )

    def write_file(self, path: str, content: str) -> float:
        import base64
        b64_content = base64.b64encode(content.encode()).decode()
        result = self.run_command(f"echo '{b64_content}' | base64 -d > {path}")
        return result.duration_ms

    def read_file(self, path: str) -> tuple[str, float]:
        result = self.run_command(f"cat {path}")
        return result.stdout, result.duration_ms

    def destroy(self) -> None:
        if self.session_id:
            try:
                self.client.delete(f"{self.base_url}/sessions/{self.session_id}")
            except Exception:
                pass
            self.session_id = None
        self.client.close()


class OpenSandboxGrpcClient(BaseSandbox):
    """OpenSandbox gRPC client for benchmarking (uses gRPC for operations)."""

    def __init__(
        self,
        base_url: str = "http://localhost:8080",
        grpc_port: int = 50051,
    ):
        self.base_url = base_url
        self.grpc_port = grpc_port
        self.session_id: Optional[str] = None
        self.http_client = httpx.Client(timeout=300)
        self._grpc_channel = None
        self._stub = None

        # Parse host for gRPC
        from urllib.parse import urlparse
        parsed = urlparse(base_url)
        self._host = parsed.hostname or "localhost"
        self._is_secure = parsed.scheme == "https"

    def _ensure_grpc(self):
        """Ensure gRPC connection is established."""
        if self._grpc_channel is None:
            import grpc
            from opensandbox.proto import sandbox_pb2_grpc

            grpc_target = f"{self._host}:{self.grpc_port}"
            # Use insecure channel - Fly.io forwards raw TCP
            self._grpc_channel = grpc.insecure_channel(grpc_target)
            self._stub = sandbox_pb2_grpc.SandboxServiceStub(self._grpc_channel)

    @property
    def name(self) -> str:
        return "opensandbox-grpc"

    def create(self, timeout: int = 300) -> str:
        response = self.http_client.post(
            f"{self.base_url}/sessions",
            json={"env": {}}
        )
        response.raise_for_status()
        self.session_id = response.json()["session_id"]
        self._ensure_grpc()
        return self.session_id

    def run_command(self, command: str) -> CommandResult:
        if not self.session_id:
            raise RuntimeError("Session not created")

        from opensandbox.proto import sandbox_pb2

        request = sandbox_pb2.RunCommandRequest(
            session_id=self.session_id,
            command=["/bin/sh", "-c", command],
            time_ms=300000,
            mem_kb=2097152,
            fsize_kb=1048576,
            nofile=256,
        )

        start = time.perf_counter()
        response = self._stub.RunCommand(request)
        duration_ms = (time.perf_counter() - start) * 1000

        exit_code = response.exit_code
        if response.signal:
            exit_code = 128 + response.signal

        return CommandResult(
            stdout=response.stdout,
            stderr=response.stderr,
            exit_code=exit_code,
            duration_ms=duration_ms
        )

    def write_file(self, path: str, content: str) -> float:
        if not self.session_id:
            raise RuntimeError("Session not created")

        from opensandbox.proto import sandbox_pb2

        request = sandbox_pb2.WriteFileRequest(
            session_id=self.session_id,
            path=path,
            content=content.encode("utf-8"),
        )

        start = time.perf_counter()
        response = self._stub.WriteFile(request)
        duration_ms = (time.perf_counter() - start) * 1000

        if not response.success:
            raise RuntimeError(f"Write file failed: {response.error}")

        return duration_ms

    def read_file(self, path: str) -> tuple[str, float]:
        if not self.session_id:
            raise RuntimeError("Session not created")

        from opensandbox.proto import sandbox_pb2

        request = sandbox_pb2.ReadFileRequest(
            session_id=self.session_id,
            path=path,
        )

        start = time.perf_counter()
        response = self._stub.ReadFile(request)
        duration_ms = (time.perf_counter() - start) * 1000

        if response.error:
            raise RuntimeError(f"Read file failed: {response.error}")

        return response.content.decode("utf-8"), duration_ms

    def destroy(self) -> None:
        if self.session_id:
            try:
                self.http_client.delete(f"{self.base_url}/sessions/{self.session_id}")
            except Exception:
                pass
            self.session_id = None

        if self._grpc_channel:
            self._grpc_channel.close()
            self._grpc_channel = None
            self._stub = None

        self.http_client.close()


class E2BSandboxClient(BaseSandbox):
    """E2B sandbox client for benchmarking."""

    def __init__(self):
        self._sandbox = None
        self._sandbox_id: Optional[str] = None

    @property
    def name(self) -> str:
        return "e2b"

    def create(self, timeout: int = 300) -> str:
        from e2b import Sandbox
        self._sandbox = Sandbox.create(timeout=timeout)
        self._sandbox_id = self._sandbox.sandbox_id
        return self._sandbox_id

    def run_command(self, command: str) -> CommandResult:
        if not self._sandbox:
            raise RuntimeError("Sandbox not created")

        start = time.perf_counter()
        result = self._sandbox.commands.run(command, timeout=300)
        duration_ms = (time.perf_counter() - start) * 1000

        return CommandResult(
            stdout=result.stdout,
            stderr=result.stderr,
            exit_code=result.exit_code,
            duration_ms=duration_ms
        )

    def write_file(self, path: str, content: str) -> float:
        if not self._sandbox:
            raise RuntimeError("Sandbox not created")

        start = time.perf_counter()
        self._sandbox.files.write(path, content)
        duration_ms = (time.perf_counter() - start) * 1000
        return duration_ms

    def read_file(self, path: str) -> tuple[str, float]:
        if not self._sandbox:
            raise RuntimeError("Sandbox not created")

        start = time.perf_counter()
        content = self._sandbox.files.read(path)
        duration_ms = (time.perf_counter() - start) * 1000
        return content, duration_ms

    def destroy(self) -> None:
        if self._sandbox:
            try:
                self._sandbox.kill()
            except Exception:
                pass
            self._sandbox = None
            self._sandbox_id = None


def get_sandbox(provider: str, **kwargs) -> BaseSandbox:
    """Factory function to get a sandbox instance."""
    if provider == "opensandbox":
        # Default to gRPC for better performance
        base_url = kwargs.get("base_url", os.environ.get("OPENSANDBOX_URL", "https://opensandbox-test.fly.dev"))
        grpc_port = kwargs.get("grpc_port", int(os.environ.get("OPENSANDBOX_GRPC_PORT", "50051")))
        return OpenSandboxGrpcClient(base_url=base_url, grpc_port=grpc_port)
    elif provider == "opensandbox-http":
        # Legacy HTTP-only mode
        base_url = kwargs.get("base_url", os.environ.get("OPENSANDBOX_URL", "https://opensandbox-test.fly.dev"))
        return OpenSandboxClient(base_url=base_url)
    elif provider == "opensandbox-grpc":
        # Explicit gRPC mode
        base_url = kwargs.get("base_url", os.environ.get("OPENSANDBOX_URL", "https://opensandbox-test.fly.dev"))
        grpc_port = kwargs.get("grpc_port", int(os.environ.get("OPENSANDBOX_GRPC_PORT", "50051")))
        return OpenSandboxGrpcClient(base_url=base_url, grpc_port=grpc_port)
    elif provider == "e2b":
        return E2BSandboxClient()
    else:
        raise ValueError(f"Unknown provider: {provider}")
