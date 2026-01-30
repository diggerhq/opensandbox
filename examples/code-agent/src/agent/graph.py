"""LangGraph workflow definition."""

import uuid
from langgraph.graph import StateGraph, END

from src.agent.state import AgentState
from src.agent.nodes import (
    setup_node,
    execute_node,
    create_pr_node,
    cleanup_node,
    should_continue,
)


def create_agent_graph():
    """
    Create the LangGraph workflow for the coding agent.
    
    Workflow:
    1. Setup: Create sandbox, clone repo, create branch
    2. Execute: LLM agent makes code changes
    3. Create PR: Push and create pull request
    4. Cleanup: Destroy sandbox
    
    Returns:
        Compiled LangGraph workflow
    """
    # Create the graph
    workflow = StateGraph(AgentState)
    
    # Add nodes
    workflow.add_node("setup", setup_node)
    workflow.add_node("execute", execute_node)
    workflow.add_node("create_pr", create_pr_node)
    workflow.add_node("cleanup", cleanup_node)
    
    # Set entry point
    workflow.set_entry_point("setup")
    
    # Add edges
    workflow.add_conditional_edges(
        "setup",
        should_continue,
        {
            "execute": "execute",
            "cleanup": "cleanup",
        }
    )
    
    workflow.add_conditional_edges(
        "execute",
        should_continue,
        {
            "create_pr": "create_pr",
            "cleanup": "cleanup",
        }
    )
    
    workflow.add_conditional_edges(
        "create_pr",
        should_continue,
        {
            "cleanup": "cleanup",
        }
    )
    
    # Cleanup always ends
    workflow.add_edge("cleanup", END)
    
    # Compile the graph
    return workflow.compile()


async def run_agent(
    repo_url: str,
    task: str,
    github_token: str,
    branch_name: str | None = None,
    base_branch: str | None = None,
    llm_provider: str = "anthropic",
    model: str | None = None,
    max_iterations: int | None = None,
    sandbox_provider: str | None = None,
) -> dict:
    """
    Run the coding agent.
    
    Args:
        repo_url: GitHub repository URL
        task: Task description
        github_token: GitHub personal access token
        branch_name: Optional custom branch name
        base_branch: Base branch for PR (None = auto-detect from repo)
        llm_provider: LLM provider to use ("anthropic" or "openai")
        model: Specific model to use (None = use provider default)
        max_iterations: Max LLM iterations (None = use config default)
        sandbox_provider: Sandbox provider ("opensandbox" or "e2b", None = use config default)
        
    Returns:
        Final state dict with results
    """
    graph = create_agent_graph()
    
    # Create initial state
    initial_state: AgentState = {
        "messages": [],
        "task": task,
        "repo_url": repo_url,
        "branch_name": branch_name or f"agent/{uuid.uuid4().hex[:8]}",
        "base_branch": base_branch,  # None means auto-detect
        "github_token": github_token,
        "llm_provider": llm_provider,
        "model": model,
        "max_iterations": max_iterations,
        "sandbox_provider": sandbox_provider,  # None means use config default
        "sandbox_id": None,
        "workdir": "/home/user/repo",
        "status": "pending",
        "current_step": "setup",
        "pr_url": None,
        "error": None,
        "logs": [],
    }
    
    # Run the graph
    final_state = await graph.ainvoke(initial_state)
    
    return final_state
