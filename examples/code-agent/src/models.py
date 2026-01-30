"""Pydantic models for API requests and responses."""

from pydantic import BaseModel, Field
from typing import Optional
from enum import Enum
from datetime import datetime


class TaskStatus(str, Enum):
    """Status of a task."""
    PENDING = "pending"
    RUNNING = "running"
    COMPLETED = "completed"
    FAILED = "failed"
    CANCELLED = "cancelled"


class LLMProvider(str, Enum):
    """Supported LLM providers."""
    ANTHROPIC = "anthropic"
    OPENAI = "openai"


class SandboxProvider(str, Enum):
    """Supported sandbox providers."""
    OPENSANDBOX = "opensandbox"
    E2B = "e2b"


# Available models for each provider
AVAILABLE_MODELS = {
    "anthropic": [
        ("claude-sonnet-4-5-20250929", "Claude Sonnet 4.5 - Best for coding/agents"),
        ("claude-haiku-4-5-20251001", "Claude Haiku 4.5 - Fastest"),
        ("claude-opus-4-5-20251101", "Claude Opus 4.5 - Most intelligent"),
    ],
    "openai": [
        ("gpt-5.2", "GPT-5.2 - Latest, best for coding"),
        ("gpt-5-mini", "GPT-5 mini - Fast, cost-efficient"),
        ("gpt-5-nano", "GPT-5 nano - Fastest, cheapest"),
        ("gpt-4.1", "GPT-4.1 - Smart non-reasoning"),
        ("gpt-4o", "GPT-4o - Fast, flexible"),
        ("gpt-4o-mini", "GPT-4o mini - Fast, affordable"),
    ],
}


class TaskRequest(BaseModel):
    """Request to create a new task."""
    repo_url: str = Field(..., description="GitHub repository URL")
    task: str = Field(..., description="Task description (e.g., 'fix issue #123')")
    github_token: str = Field(..., description="GitHub personal access token")
    llm_provider: LLMProvider = Field(default=LLMProvider.ANTHROPIC, description="LLM provider to use")
    model: Optional[str] = Field(default=None, description="Specific model to use (uses provider default if not specified)")
    sandbox_provider: SandboxProvider = Field(default=SandboxProvider.OPENSANDBOX, description="Sandbox provider to use")
    branch_name: Optional[str] = Field(default=None, description="Custom branch name (auto-generated if not provided)")
    base_branch: Optional[str] = Field(default=None, description="Base branch for PR (auto-detected from repo default if not provided)")
    max_iterations: Optional[int] = Field(default=None, description="Max LLM iterations (uses config default if not specified)")


class TaskResponse(BaseModel):
    """Response after creating a task."""
    task_id: str
    status: TaskStatus
    message: str


class TaskStatusResponse(BaseModel):
    """Response for task status query."""
    task_id: str
    status: TaskStatus
    created_at: datetime
    updated_at: datetime
    repo_url: str
    task: str
    branch_name: Optional[str] = None
    pr_url: Optional[str] = None
    error: Optional[str] = None
    logs: list[str] = Field(default_factory=list)


class CommandResult(BaseModel):
    """Result of a command execution in sandbox."""
    stdout: str
    stderr: str
    exit_code: int
    
    @property
    def success(self) -> bool:
        return self.exit_code == 0
