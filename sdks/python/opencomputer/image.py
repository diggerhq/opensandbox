"""Declarative image builder for OpenSandbox."""

from __future__ import annotations

import base64
import hashlib
import json
import os
from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class ImageStep:
    type: str
    args: dict[str, Any]

    def to_dict(self) -> dict[str, Any]:
        return {"type": self.type, "args": self.args}


@dataclass(frozen=True)
class Image:
    """Declarative image builder.

    Defines a reproducible sandbox environment via a fluent API.
    Under the hood, the manifest is sent to the server which boots a base sandbox,
    executes each step, checkpoints the result, and caches it by content hash.

    Example::

        image = (
            Image.base()
            .apt_install(["curl", "git"])
            .pip_install(["requests", "pandas"])
            .add_file("/workspace/config.json", '{"key": "value"}')
            .env({"PROJECT_ROOT": "/workspace"})
            .workdir("/workspace")
        )

        # On-demand: cached by content hash
        sandbox = await Sandbox.create(image=image)

        # Pre-built snapshot
        await Snapshot.create(name="data-science", image=image)
    """

    _base: str = "base"
    _steps: tuple[ImageStep, ...] = field(default_factory=tuple)

    @classmethod
    def base(cls) -> Image:
        """Create a new image starting from the default OpenSandbox environment.

        The base includes Ubuntu 22.04 with Python, Node.js, build tools, and
        common utilities. Customize by chaining steps like .apt_install(),
        .pip_install(), .run_commands(), etc.
        """
        return cls()

    def apt_install(self, packages: list[str]) -> Image:
        """Install system packages via apt-get."""
        return Image(
            _base=self._base,
            _steps=(*self._steps, ImageStep("apt_install", {"packages": packages})),
        )

    def pip_install(self, packages: list[str]) -> Image:
        """Install Python packages via pip."""
        return Image(
            _base=self._base,
            _steps=(*self._steps, ImageStep("pip_install", {"packages": packages})),
        )

    def run_commands(self, *commands: str) -> Image:
        """Run one or more shell commands."""
        return Image(
            _base=self._base,
            _steps=(*self._steps, ImageStep("run", {"commands": list(commands)})),
        )

    def env(self, vars: dict[str, str]) -> Image:
        """Set environment variables (written to /etc/environment)."""
        return Image(
            _base=self._base,
            _steps=(*self._steps, ImageStep("env", {"vars": vars})),
        )

    def workdir(self, path: str) -> Image:
        """Set the default working directory."""
        return Image(
            _base=self._base,
            _steps=(*self._steps, ImageStep("workdir", {"path": path})),
        )

    def add_file(self, remote_path: str, content: str) -> Image:
        """Add a file with inline content to the image.

        Args:
            remote_path: Absolute path inside the sandbox where the file will be written.
            content: String content of the file.
        """
        encoded = base64.b64encode(content.encode()).decode()
        return Image(
            _base=self._base,
            _steps=(*self._steps, ImageStep("add_file", {
                "path": remote_path,
                "content": encoded,
                "encoding": "base64",
            })),
        )

    def add_local_file(self, local_path: str, remote_path: str) -> Image:
        """Add a local file into the image.

        Reads the file from disk and embeds its content in the manifest.

        Args:
            local_path: Path to the file on the local machine.
            remote_path: Absolute path inside the sandbox where the file will be written.
        """
        with open(local_path, "rb") as f:
            encoded = base64.b64encode(f.read()).decode()
        return Image(
            _base=self._base,
            _steps=(*self._steps, ImageStep("add_file", {
                "path": remote_path,
                "content": encoded,
                "encoding": "base64",
            })),
        )

    def add_local_dir(self, local_path: str, remote_path: str) -> Image:
        """Add a local directory into the image.

        Recursively reads all files and embeds them in the manifest.

        Args:
            local_path: Path to the directory on the local machine.
            remote_path: Absolute path inside the sandbox where the directory will be created.
        """
        files: list[dict[str, str]] = []
        for root, _dirs, filenames in os.walk(local_path):
            for fname in filenames:
                full = os.path.join(root, fname)
                rel = os.path.relpath(full, local_path)
                with open(full, "rb") as f:
                    encoded = base64.b64encode(f.read()).decode()
                files.append({"relativePath": rel, "content": encoded})
        return Image(
            _base=self._base,
            _steps=(*self._steps, ImageStep("add_dir", {
                "path": remote_path,
                "files": files,
            })),
        )

    def to_dict(self) -> dict[str, Any]:
        """Returns the manifest as a plain dict (for JSON serialization)."""
        return {
            "base": self._base,
            "steps": [s.to_dict() for s in self._steps],
        }

    def cache_key(self) -> str:
        """Compute a deterministic content hash for caching."""
        canonical = json.dumps(self.to_dict(), sort_keys=True, separators=(",", ":"))
        return hashlib.sha256(canonical.encode()).hexdigest()
