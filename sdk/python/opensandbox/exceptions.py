"""Custom exceptions for the OpenSandbox SDK."""


class OpenSandboxError(Exception):
    """Base exception for OpenSandbox errors."""
    pass


class SandboxNotFoundError(OpenSandboxError):
    """Raised when a sandbox session is not found."""
    pass


class SandboxConnectionError(OpenSandboxError):
    """Raised when connection to the sandbox server fails."""
    pass


class CommandExecutionError(OpenSandboxError):
    """Raised when a command execution fails."""

    def __init__(self, message: str, exit_code: int = 1, stdout: str = "", stderr: str = ""):
        super().__init__(message)
        self.exit_code = exit_code
        self.stdout = stdout
        self.stderr = stderr


class FileOperationError(OpenSandboxError):
    """Raised when a file operation fails."""
    pass
