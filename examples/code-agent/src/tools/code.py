"""Code manipulation tools for the agent."""

import logging
from langchain_core.tools import tool

from src.sandbox.base import BaseSandbox

logger = logging.getLogger(__name__)


class CodeTools:
    """
    Code manipulation operations that run in a sandbox.
    
    Provides tools for reading, writing, and exploring code files.
    """
    
    def __init__(self, sandbox: BaseSandbox, workdir: str = "/home/user/repo"):
        self.sandbox = sandbox
        self.workdir = workdir
    
    async def read_file(self, path: str) -> str:
        """
        Read a file's contents.
        
        Args:
            path: Path relative to workdir, or absolute path
            
        Returns:
            File contents
        """
        full_path = self._resolve_path(path)
        logger.debug(f"Reading file: {full_path}")
        return await self.sandbox.read_file(full_path)
    
    async def write_file(self, path: str, content: str) -> None:
        """
        Write content to a file.
        
        Args:
            path: Path relative to workdir, or absolute path
            content: Content to write
        """
        full_path = self._resolve_path(path)
        logger.debug(f"Writing file: {full_path}")
        
        # Ensure parent directory exists
        parent = "/".join(full_path.split("/")[:-1])
        await self.sandbox.run_command(f"mkdir -p {parent}")
        
        await self.sandbox.write_file(full_path, content)
    
    async def list_directory(self, path: str = ".") -> list[str]:
        """
        List contents of a directory.
        
        Args:
            path: Path relative to workdir, or absolute path
            
        Returns:
            List of file/directory names
        """
        full_path = self._resolve_path(path)
        result = await self.sandbox.run_command(f"ls -la {full_path}")
        return result.stdout
    
    async def find_files(self, pattern: str, path: str = ".") -> str:
        """
        Find files matching a pattern.
        
        Args:
            pattern: Glob pattern (e.g., "*.py", "**/*.ts")
            path: Starting directory
            
        Returns:
            List of matching file paths
        """
        full_path = self._resolve_path(path)
        result = await self.sandbox.run_command(
            f'find {full_path} -name "{pattern}" -type f 2>/dev/null | head -100'
        )
        return result.stdout
    
    async def search_in_files(self, pattern: str, file_pattern: str = "*", path: str = ".") -> str:
        """
        Search for a pattern in files (like grep).
        
        Args:
            pattern: Text/regex pattern to search for
            file_pattern: File glob pattern to search in
            path: Starting directory
            
        Returns:
            Matching lines with file paths
        """
        full_path = self._resolve_path(path)
        result = await self.sandbox.run_command(
            f'grep -rn "{pattern}" {full_path} --include="{file_pattern}" 2>/dev/null | head -100'
        )
        return result.stdout
    
    async def run_command(self, command: str) -> str:
        """
        Run an arbitrary shell command in the repo directory.
        
        Args:
            command: Shell command to run
            
        Returns:
            Command output (stdout + stderr)
        """
        result = await self.sandbox.run_command(command, workdir=self.workdir)
        output = result.stdout
        if result.stderr:
            output += f"\nSTDERR:\n{result.stderr}"
        if result.exit_code != 0:
            output += f"\n(exit code: {result.exit_code})"
        return output
    
    async def get_file_tree(self, max_depth: int = 3) -> str:
        """
        Get a tree view of the repository structure.
        
        Args:
            max_depth: Maximum depth to traverse
            
        Returns:
            Tree-formatted directory structure
        """
        result = await self.sandbox.run_command(
            f"find . -maxdepth {max_depth} -type f | head -200 | sort",
            workdir=self.workdir
        )
        return result.stdout
    
    def _resolve_path(self, path: str) -> str:
        """Resolve a path relative to workdir."""
        if path.startswith("/"):
            return path
        return f"{self.workdir}/{path}"


def create_code_tools(sandbox: BaseSandbox, workdir: str = "/home/user/repo"):
    """
    Create LangChain tools for code operations.
    
    Returns a list of tools that can be bound to an LLM.
    """
    code = CodeTools(sandbox, workdir)
    
    @tool
    async def read_file(path: str) -> str:
        """Read the contents of a file. Path is relative to the repo root."""
        try:
            return await code.read_file(path)
        except FileNotFoundError:
            return f"Error: File not found: {path}"
    
    @tool
    async def write_file(path: str, content: str) -> str:
        """Write content to a file. Path is relative to the repo root. Creates parent directories if needed."""
        await code.write_file(path, content)
        return f"Successfully wrote to {path}"
    
    @tool
    async def list_directory(path: str = ".") -> str:
        """List contents of a directory. Path is relative to the repo root."""
        return await code.list_directory(path)
    
    @tool
    async def find_files(pattern: str, path: str = ".") -> str:
        """Find files matching a glob pattern (e.g., '*.py', '*.ts'). Returns list of file paths."""
        return await code.find_files(pattern, path)
    
    @tool
    async def search_code(pattern: str, file_pattern: str = "*") -> str:
        """Search for a text pattern in files (like grep). Returns matching lines with file paths."""
        return await code.search_in_files(pattern, file_pattern)
    
    @tool
    async def run_shell_command(command: str) -> str:
        """Run a shell command in the repo directory. Use for running tests, builds, etc."""
        return await code.run_command(command)
    
    @tool
    async def get_repo_structure(max_depth: int = 3) -> str:
        """Get a tree view of the repository structure up to the specified depth."""
        return await code.get_file_tree(max_depth)
    
    return [read_file, write_file, list_directory, find_files, search_code, run_shell_command, get_repo_structure], code
