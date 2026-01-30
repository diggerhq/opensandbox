"""E2B sandbox implementation."""

import logging
import os
from typing import Optional
from e2b import Sandbox

from .base import BaseSandbox
from src.models import CommandResult
from src.config import get_settings

logger = logging.getLogger(__name__)


class E2BSandbox(BaseSandbox):
    """
    E2B sandbox implementation.
    
    Uses E2B's cloud sandbox for secure code execution.
    """
    
    def __init__(self):
        self._sandbox: Optional[Sandbox] = None
        self._sandbox_id: Optional[str] = None
    
    @property
    def sandbox_id(self) -> Optional[str]:
        """Get the current sandbox ID."""
        return self._sandbox_id
    
    @property
    def is_active(self) -> bool:
        """Check if the sandbox is active."""
        return self._sandbox is not None
    
    async def create(self, timeout: int = 1800) -> str:
        """
        Create a new E2B sandbox.
        
        Args:
            timeout: Sandbox timeout in seconds
            
        Returns:
            The sandbox ID
        """
        settings = get_settings()
        
        # E2B SDK reads API key from environment variable
        os.environ["E2B_API_KEY"] = settings.e2b_api_key
        
        logger.info("Creating E2B sandbox...")
        
        # Create sandbox using the class method (constructor is deprecated)
        self._sandbox = Sandbox.create(timeout=timeout)
        self._sandbox_id = self._sandbox.sandbox_id
        
        logger.info(f"Created sandbox: {self._sandbox_id}")
        
        # Install GitHub CLI
        await self._setup_sandbox()
        
        return self._sandbox_id
    
    async def _setup_sandbox(self) -> None:
        """Set up the sandbox with required tools."""
        if not self._sandbox:
            return
            
        # Install gh CLI if not present
        logger.info("Setting up sandbox environment...")
        
        # Check if gh is installed, install if not
        result = self._sandbox.commands.run("which gh || echo 'not found'")
        if "not found" in result.stdout:
            logger.info("Installing GitHub CLI...")
            self._sandbox.commands.run(
                "curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | sudo dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg && "
                "echo 'deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main' | sudo tee /etc/apt/sources.list.d/github-cli.list > /dev/null && "
                "sudo apt update && sudo apt install gh -y",
                timeout=120
            )
        
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
        if not self._sandbox:
            raise RuntimeError("Sandbox not created. Call create() first.")
        
        logger.debug(f"Running command: {command}")
        
        result = self._sandbox.commands.run(
            command,
            cwd=workdir,
            envs=env or {},
            timeout=300  # 5 minute timeout per command
        )
        
        return CommandResult(
            stdout=result.stdout,
            stderr=result.stderr,
            exit_code=result.exit_code
        )
    
    async def read_file(self, path: str) -> str:
        """
        Read a file from the sandbox.
        
        Args:
            path: Path to the file
            
        Returns:
            File contents
        """
        if not self._sandbox:
            raise RuntimeError("Sandbox not created. Call create() first.")
        
        try:
            content = self._sandbox.files.read(path)
            return content
        except Exception as e:
            raise FileNotFoundError(f"File not found: {path}") from e
    
    async def write_file(self, path: str, content: str) -> None:
        """
        Write content to a file in the sandbox.
        
        Args:
            path: Path to the file
            content: Content to write
        """
        if not self._sandbox:
            raise RuntimeError("Sandbox not created. Call create() first.")
        
        self._sandbox.files.write(path, content)
        logger.debug(f"Wrote file: {path}")
    
    async def list_files(self, path: str = ".") -> list[str]:
        """
        List files in a directory.
        
        Args:
            path: Directory path
            
        Returns:
            List of file/directory names
        """
        if not self._sandbox:
            raise RuntimeError("Sandbox not created. Call create() first.")
        
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
        """Destroy the sandbox."""
        if self._sandbox:
            logger.info(f"Destroying sandbox: {self._sandbox_id}")
            try:
                self._sandbox.kill()
            except Exception as e:
                logger.warning(f"Error destroying sandbox: {e}")
            finally:
                self._sandbox = None
                self._sandbox_id = None
