"""OpenSandbox implementation using HTTP API."""

import logging
import uuid
import httpx
from typing import Optional

from .base import BaseSandbox
from src.models import CommandResult
from src.config import get_settings

logger = logging.getLogger(__name__)


class OpenSandbox(BaseSandbox):
    """
    OpenSandbox implementation.
    
    Uses a local OpenSandbox server for Linux-based sandboxed execution.
    Communicates via HTTP API.
    """
    
    def __init__(self, base_url: str = "http://localhost:8080"):
        self._base_url = base_url
        self._session_id: Optional[str] = None
        self._client = httpx.Client(timeout=300)  # 5 minute timeout
        self._workdir = "/home/user/repo"
    
    @property
    def sandbox_id(self) -> Optional[str]:
        """Get the current session ID."""
        return self._session_id
    
    @property
    def is_active(self) -> bool:
        """Check if session is active."""
        return self._session_id is not None
    
    async def create(self, timeout: int = 1800) -> str:
        """
        Create a new OpenSandbox session.
        
        Args:
            timeout: Session timeout (not directly used, sessions auto-expire)
            
        Returns:
            The session ID
        """
        logger.info("Creating OpenSandbox session...")
        
        # Create session with initial environment
        response = self._client.post(
            f"{self._base_url}/sessions",
            json={"env": {}}
        )
        response.raise_for_status()
        
        data = response.json()
        self._session_id = data["session_id"]
        
        logger.info(f"Created session: {self._session_id}")
        
        # Set up the sandbox environment
        await self._setup_sandbox()
        
        return self._session_id
    
    async def _setup_sandbox(self) -> None:
        """Set up the sandbox with required directories."""
        if not self._session_id:
            return
        
        logger.info("Setting up sandbox environment...")
        
        # Create parent directory for repo (but not the repo dir itself - clone will create it)
        await self.run_command("mkdir -p /home/user")
        
        # Set initial working directory to /home/user
        response = self._client.post(
            f"{self._base_url}/sessions/{self._session_id}/cwd",
            json={"cwd": "/home/user"}
        )
        
        # Check git and gh are available
        result = await self.run_command("git --version")
        logger.info(f"Git version: {result.stdout.strip()}")
        
        result = await self.run_command("gh --version")
        logger.info(f"GitHub CLI: {result.stdout.split(chr(10))[0] if result.exit_code == 0 else 'not available'}")
        
        logger.info("Sandbox setup complete")
    
    async def run_command(
        self, 
        command: str, 
        workdir: Optional[str] = None,
        env: Optional[dict[str, str]] = None
    ) -> CommandResult:
        """
        Execute a command in the sandbox.
        
        Args:
            command: The command to execute
            workdir: Working directory
            env: Additional environment variables
            
        Returns:
            CommandResult with stdout, stderr, exit_code
        """
        if not self._session_id:
            raise RuntimeError("Session not created. Call create() first.")
        
        logger.debug(f"Running command: {command}")
        
        # Set environment variables if provided
        if env:
            self._client.post(
                f"{self._base_url}/sessions/{self._session_id}/env",
                json={"env": env}
            )
        
        # Set working directory if provided
        if workdir:
            self._client.post(
                f"{self._base_url}/sessions/{self._session_id}/cwd",
                json={"cwd": workdir}
            )
        
        # Run the command via shell
        # OpenSandbox expects command as array, wrap in shell for string commands
        response = self._client.post(
            f"{self._base_url}/sessions/{self._session_id}/run",
            json={
                "command": ["/bin/sh", "-c", command],
                "time": 300000,   # 5 minute timeout in ms
                "mem": 4194304,   # 4GB memory limit in KB
                "fsize": 102400,  # 100MB max file size in KB
                "nofile": 1024,   # 1024 open files (git clone needs many)
            }
        )
        
        if response.status_code != 200:
            error_text = response.text
            logger.error(f"Command failed: {error_text}")
            return CommandResult(
                stdout="",
                stderr=f"HTTP error: {error_text}",
                exit_code=1
            )
        
        data = response.json()
        
        exit_code = data.get("exit_code", 0)
        if data.get("signal"):
            # Process was killed by signal
            exit_code = 128 + data["signal"]
        
        return CommandResult(
            stdout=data.get("stdout", ""),
            stderr=data.get("stderr", ""),
            exit_code=exit_code
        )
    
    async def read_file(self, path: str) -> str:
        """
        Read a file from the sandbox.
        
        Args:
            path: Path to the file
            
        Returns:
            File contents
        """
        if not self._session_id:
            raise RuntimeError("Session not created. Call create() first.")
        
        result = await self.run_command(f"cat {path}")
        if result.exit_code != 0:
            raise FileNotFoundError(f"File not found: {path}")
        
        return result.stdout
    
    async def write_file(self, path: str, content: str) -> None:
        """
        Write content to a file in the sandbox.
        
        Args:
            path: Path to the file
            content: Content to write
        """
        if not self._session_id:
            raise RuntimeError("Session not created. Call create() first.")
        
        # Ensure parent directory exists
        dir_path = "/".join(path.rsplit("/", 1)[:-1]) if "/" in path else "."
        if dir_path and dir_path != ".":
            await self.run_command(f"mkdir -p {dir_path}")
        
        # Use heredoc to write file content safely
        # Escape any backticks and dollar signs in content
        escaped_content = content.replace("\\", "\\\\").replace("$", "\\$").replace("`", "\\`")
        
        # Write via base64 to handle special characters
        import base64
        b64_content = base64.b64encode(content.encode()).decode()
        result = await self.run_command(f"echo '{b64_content}' | base64 -d > {path}")
        
        if result.exit_code != 0:
            raise RuntimeError(f"Failed to write file: {result.stderr}")
        
        logger.debug(f"Wrote file: {path}")
    
    async def list_files(self, path: str = ".") -> list[str]:
        """
        List files in a directory.
        
        Args:
            path: Directory path
            
        Returns:
            List of file/directory names
        """
        if not self._session_id:
            raise RuntimeError("Session not created. Call create() first.")
        
        result = await self.run_command(f"ls -la {path}")
        if result.exit_code != 0:
            raise FileNotFoundError(f"Directory not found: {path}")
        
        # Parse ls output (skip total line and . / ..)
        lines = result.stdout.strip().split("\n")
        files = []
        for line in lines[1:]:  # Skip "total" line
            parts = line.split()
            if len(parts) >= 9:
                name = parts[-1]
                if name not in [".", ".."]:
                    files.append(name)
        return files
    
    async def destroy(self) -> None:
        """Destroy the session."""
        if self._session_id:
            logger.info(f"Destroying session: {self._session_id}")
            try:
                self._client.delete(f"{self._base_url}/sessions/{self._session_id}")
            except Exception as e:
                logger.warning(f"Error destroying session: {e}")
            finally:
                self._session_id = None
        
        self._client.close()
