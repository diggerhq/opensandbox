"""Agent state definition for LangGraph."""

from typing import Annotated, Optional
from typing_extensions import TypedDict
from langgraph.graph.message import add_messages


class AgentState(TypedDict):
    """
    State schema for the coding agent.
    
    This state is passed between nodes in the LangGraph workflow.
    """
    
    # Conversation history - automatically merged using add_messages
    messages: Annotated[list, add_messages]
    
    # Task information
    task: str                           # User's task description
    repo_url: str                       # GitHub repository URL
    branch_name: str                    # Working branch name
    base_branch: Optional[str]          # Base branch for PR (None = auto-detect)
    
    # Authentication
    github_token: str                   # GitHub PAT for gh CLI
    
    # LLM configuration
    llm_provider: str                   # "anthropic" or "openai"
    model: Optional[str]                # Specific model (None = use default)
    max_iterations: Optional[int]       # Max LLM iterations (None = use config default)
    
    # Sandbox configuration
    sandbox_provider: Optional[str]     # "opensandbox" or "e2b" (None = use config default)
    
    # Sandbox state
    sandbox_id: Optional[str]           # Sandbox/session instance ID
    workdir: str                        # Working directory in sandbox
    
    # Execution state
    status: str                         # Current status (pending, running, completed, failed)
    current_step: str                   # Current step in workflow
    
    # Results
    pr_url: Optional[str]               # URL of created PR
    error: Optional[str]                # Error message if failed
    
    # Logs for debugging/streaming
    logs: list[str]                     # Execution logs
