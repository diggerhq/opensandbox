"""Abstract base class for sandbox providers."""

from abc import ABC, abstractmethod
from typing import Optional
from src.models import CommandResult


class BaseSandbox(ABC):
    """
    Abstract interface for sandbox providers.
    
    Implementations must provide methods to:
    - Create and destroy sandbox instances
    - Execute commands in the sandbox
    - Read and write files in the sandbox
    """
    
    @property
    @abstractmethod
    def sandbox_id(self) -> Optional[str]:
        """Get the current sandbox ID, or None if not created."""
        pass
    
    @property
    @abstractmethod
    def is_active(self) -> bool:
        """Check if the sandbox is currently active."""
        pass
    
    @abstractmethod
    async def create(self, timeout: int = 1800) -> str:
        """
        Create a new sandbox instance.
        
        Args:
            timeout: Sandbox timeout in seconds (default 30 minutes)
            
        Returns:
            The sandbox ID
        """
        pass
    
    @abstractmethod
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
            workdir: Working directory (optional)
            env: Additional environment variables (optional)
            
        Returns:
            CommandResult with stdout, stderr, and exit_code
        """
        pass
    
    @abstractmethod
    async def read_file(self, path: str) -> str:
        """
        Read a file from the sandbox.
        
        Args:
            path: Path to the file in the sandbox
            
        Returns:
            File contents as string
            
        Raises:
            FileNotFoundError: If file doesn't exist
        """
        pass
    
    @abstractmethod
    async def write_file(self, path: str, content: str) -> None:
        """
        Write content to a file in the sandbox.
        
        Args:
            path: Path to the file in the sandbox
            content: Content to write
        """
        pass
    
    @abstractmethod
    async def list_files(self, path: str = ".") -> list[str]:
        """
        List files in a directory.
        
        Args:
            path: Directory path (default current directory)
            
        Returns:
            List of file/directory names
        """
        pass
    
    @abstractmethod
    async def destroy(self) -> None:
        """
        Destroy the sandbox instance and clean up resources.
        """
        pass
    
    async def __aenter__(self) -> "BaseSandbox":
        """Async context manager entry."""
        await self.create()
        return self
    
    async def __aexit__(self, exc_type, exc_val, exc_tb) -> None:
        """Async context manager exit - always cleanup."""
        await self.destroy()
