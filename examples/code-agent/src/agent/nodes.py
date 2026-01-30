"""Node implementations for the LangGraph workflow."""

import logging
import uuid
from datetime import datetime
from langchain_core.messages import HumanMessage, SystemMessage

from src.agent.state import AgentState
from src.sandbox import get_sandbox
from src.sandbox.base import BaseSandbox
from src.tools.git import GitTools, create_git_tools
from src.tools.code import create_code_tools
from src.llm import get_llm
from src.config import get_settings

logger = logging.getLogger(__name__)

# Store sandbox instances by sandbox_id for cleanup
_sandboxes: dict[str, BaseSandbox] = {}


def add_log(state: AgentState, message: str) -> list[str]:
    """Add a timestamped log message."""
    timestamp = datetime.now().strftime("%H:%M:%S")
    return state["logs"] + [f"[{timestamp}] {message}"]


async def setup_node(state: AgentState) -> dict:
    """
    Setup node: Create sandbox and clone repository.
    """
    import time
    
    setup_start = time.time()
    logs = add_log(state, "Starting setup...")
    
    try:
        # Create sandbox using configured provider
        settings = get_settings()
        provider = state.get("sandbox_provider") or settings.sandbox_provider
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Creating {provider} sandbox..."]
        
        sandbox_kwargs = {}
        if provider == "opensandbox":
            sandbox_kwargs["base_url"] = settings.opensandbox_url
        
        sandbox_start = time.time()
        sandbox = get_sandbox(provider, **sandbox_kwargs)
        sandbox_id = await sandbox.create(timeout=settings.sandbox_timeout)
        _sandboxes[sandbox_id] = sandbox
        sandbox_time = time.time() - sandbox_start
        
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Sandbox created: {sandbox_id} ({sandbox_time:.1f}s)"]
        
        # Setup git authentication
        auth_start = time.time()
        git = GitTools(sandbox, state["github_token"], state["workdir"])
        await git.setup_git_auth()
        auth_time = time.time() - auth_start
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] GitHub authentication configured ({auth_time:.1f}s)"]
        
        # Clone repository
        clone_start = time.time()
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Cloning {state['repo_url']}..."]
        await git.clone_repo(state["repo_url"])
        clone_time = time.time() - clone_start
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Repository cloned ({clone_time:.1f}s)"]
        
        # Determine base branch and checkout
        base_branch = state.get("base_branch")
        if not base_branch:
            base_branch = await git.get_default_branch()
            logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Auto-detected base branch: {base_branch}"]
        
        # Checkout base branch before creating feature branch
        await git.checkout_branch(base_branch)
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Checked out base branch: {base_branch}"]
        
        # Create feature branch from base
        branch_name = state["branch_name"] or f"agent/{uuid.uuid4().hex[:8]}"
        await git.create_branch(branch_name)
        
        total_setup_time = time.time() - setup_start
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Created branch: {branch_name} (from {base_branch})"]
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] ✅ Setup complete in {total_setup_time:.1f}s"]
        
        return {
            "sandbox_id": sandbox_id,
            "branch_name": branch_name,
            "base_branch": base_branch,  # Store detected base branch
            "status": "running",
            "current_step": "execute",
            "logs": logs
        }
        
    except Exception as e:
        logger.exception("Setup failed")
        return {
            "status": "failed",
            "error": str(e),
            "current_step": "cleanup",
            "logs": logs + [f"[{datetime.now().strftime('%H:%M:%S')}] ERROR: {str(e)}"]
        }


async def execute_node(state: AgentState) -> dict:
    """
    Execute node: Use LLM with tools to complete the task.
    """
    logs = add_log(state, "Starting task execution...")
    
    sandbox_id = state["sandbox_id"]
    if not sandbox_id or sandbox_id not in _sandboxes:
        return {
            "status": "failed",
            "error": "Sandbox not found",
            "current_step": "cleanup",
            "logs": logs + [f"[{datetime.now().strftime('%H:%M:%S')}] ERROR: Sandbox not found"]
        }
    
    sandbox = _sandboxes[sandbox_id]
    workdir = state["workdir"]
    
    try:
        # Create tools
        git_tools, _ = create_git_tools(sandbox, state["github_token"], workdir)
        code_tools, _ = create_code_tools(sandbox, workdir)
        all_tools = git_tools + code_tools
        
        # Check if semantic search index is available
        from src.tools.search import create_search_tools, check_index_available
        repo_url = state.get("repo_url", "")
        
        # Extract repo name from URL (e.g., "https://github.com/owner/repo" -> "owner/repo")
        repo_name = None
        if "github.com" in repo_url:
            parts = repo_url.rstrip("/").split("/")
            if len(parts) >= 2:
                repo_name = f"{parts[-2]}/{parts[-1]}"
        
        has_index = False
        if repo_name and check_index_available(repo_name):
            search_tools = create_search_tools(repo_name)
            all_tools = search_tools + all_tools  # Prioritize search tools
            has_index = True
            logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Semantic search index available for {repo_name}"]
        else:
            logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] No semantic search index (using grep-based search)"]
        
        # Get LLM with specified provider and model
        llm_provider = state.get("llm_provider", "anthropic")
        model = state.get("model")  # None uses provider default
        llm = get_llm(provider=llm_provider, model=model)
        llm_with_tools = llm.bind_tools(all_tools)
        
        model_name = model or f"{llm_provider} default"
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Using model: {model_name}"]
        
        # Build system prompt based on available tools
        search_tools_section = ""
        search_workflow = ""
        
        if has_index:
            search_tools_section = """
**Semantic Search Tools (FAST - use these first for finding code!):**
- semantic_search: Search the codebase by meaning/concept. Much faster than grep!
- find_similar_code: Find code patterns similar to a description

"""
            search_workflow = """
2. Use semantic_search to quickly find relevant code (e.g., "terragrunt configuration handling")
"""
        
        # System prompt for the coding agent
        system_prompt = f"""You are an expert coding agent. You have been given a task to complete in a GitHub repository.

You have access to the following tools:

**GitHub Tools (use these first!):**
- fetch_github_issue: Fetch issue content (title, body, comments). ALWAYS use this first if task mentions an issue number!
- fetch_github_pr: Fetch PR content (title, body, files)
- run_gh_command: Run any gh CLI command (e.g., 'issue list', 'pr list')
{search_tools_section}
**Code Tools:**
- read_file: Read file contents
- write_file: Write/modify files
- list_directory: List directory contents
- find_files: Find files by pattern
- search_code: Search for text in files (grep)
- run_shell_command: Run shell commands (tests, builds, etc.)
- get_repo_structure: Get directory tree

**Git Tools:**
- git_diff: See current changes
- git_status: Check git status
- git_commit: Commit changes
- git_push: Push branch

CRITICAL WORKFLOW:
1. **If the task mentions an issue number (e.g., "fix issue #123", "implement #456"):**
   - FIRST use fetch_github_issue to get the actual issue content
   - Read and understand what the issue is asking for
   - Do NOT guess based on code patterns alone
{search_workflow}
3. Explore the repository structure to understand the codebase
4. Find relevant files for the task
5. Read and understand the existing code
6. Make necessary changes to complete the task
7. Run any tests if applicable
8. Commit your changes with a descriptive message
9. Push the branch

Be thorough but efficient. Make sure your changes are correct and follow the project's coding style.
When you have completed all necessary changes and pushed the branch, respond with TASK_COMPLETE."""

        # Build messages
        messages = [
            SystemMessage(content=system_prompt),
            HumanMessage(content=f"""Task: {state['task']}

Repository: {state['repo_url']}
Working directory: {workdir}
Branch: {state['branch_name']}

Please complete this task. Start by exploring the repository structure.""")
        ]
        
        # Add any previous messages from state
        messages.extend(state.get("messages", []))
        
        # Agentic loop - configurable iterations for complex tasks
        from src.config import get_settings
        max_iterations = state.get("max_iterations") or get_settings().max_iterations
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Max iterations: {max_iterations}"]
        iteration = 0
        
        while iteration < max_iterations:
            iteration += 1
            logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] LLM iteration {iteration}"]
            
            # Get LLM response
            response = await llm_with_tools.ainvoke(messages)
            messages.append(response)
            
            # Check if done
            if "TASK_COMPLETE" in str(response.content):
                logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Task marked complete by agent"]
                break
            
            # Process tool calls
            if response.tool_calls:
                from langchain_core.messages import ToolMessage
                
                for tool_call in response.tool_calls:
                    tool_name = tool_call["name"]
                    tool_args = tool_call["args"]
                    
                    # Log tool call with arguments for visibility
                    args_summary = ", ".join(f"{k}={repr(v)[:50]}" for k, v in tool_args.items())
                    logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Tool: {tool_name}({args_summary})"]
                    
                    # Find and execute the tool
                    tool_fn = next((t for t in all_tools if t.name == tool_name), None)
                    if tool_fn:
                        try:
                            result = await tool_fn.ainvoke(tool_args)
                            # Log brief result summary
                            result_preview = str(result)[:200].replace('\n', ' ')
                            if len(str(result)) > 200:
                                result_preview += "..."
                            logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}]   → {result_preview}"]
                            # Truncate long results for LLM context
                            if len(str(result)) > 5000:
                                result = str(result)[:5000] + "\n... (truncated)"
                        except Exception as e:
                            result = f"Error: {str(e)}"
                            logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}]   → ERROR: {str(e)}"]
                    else:
                        result = f"Unknown tool: {tool_name}"
                    
                    messages.append(ToolMessage(content=str(result), tool_call_id=tool_call["id"]))
            else:
                # No tool calls and not complete - might be stuck
                if iteration > 5:
                    logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] No tool calls, prompting agent"]
                    messages.append(HumanMessage(content="Continue with the task. Use the available tools to make progress."))
        
        if iteration >= max_iterations:
            logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] WARNING: Max iterations reached"]
        
        return {
            "messages": messages,
            "status": "running",
            "current_step": "create_pr",
            "logs": logs
        }
        
    except Exception as e:
        logger.exception("Execution failed")
        return {
            "status": "failed",
            "error": str(e),
            "current_step": "cleanup",
            "logs": logs + [f"[{datetime.now().strftime('%H:%M:%S')}] ERROR: {str(e)}"]
        }


async def create_pr_node(state: AgentState) -> dict:
    """
    Create PR node: Push changes and create pull request.
    """
    logs = add_log(state, "Creating pull request...")
    
    sandbox_id = state["sandbox_id"]
    if not sandbox_id or sandbox_id not in _sandboxes:
        return {
            "status": "failed",
            "error": "Sandbox not found",
            "current_step": "cleanup",
            "logs": logs
        }
    
    sandbox = _sandboxes[sandbox_id]
    
    try:
        git = GitTools(sandbox, state["github_token"], state["workdir"])
        
        # Get base branch (auto-detect if not specified)
        base_branch = state.get("base_branch")
        if not base_branch:
            base_branch = await git.get_default_branch()
            logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Auto-detected base branch: {base_branch}"]
        
        # Create PR (this will push the branch automatically)
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Creating PR against {base_branch}..."]
        pr_title = f"[Agent] {state['task'][:50]}"
        pr_body = f"""## Summary
This PR was automatically created by the Code Agent.

**Task:** {state['task']}

**Branch:** {state['branch_name']} → {base_branch}

---
*Generated by Code Agent*
"""
        
        pr_url = await git.create_pr(pr_title, pr_body, base=base_branch)
        logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] PR created: {pr_url}"]
        
        return {
            "pr_url": pr_url,
            "status": "running",
            "current_step": "cleanup",
            "logs": logs
        }
        
    except Exception as e:
        logger.exception("PR creation failed")
        return {
            "status": "failed",
            "error": str(e),
            "current_step": "cleanup",
            "logs": logs + [f"[{datetime.now().strftime('%H:%M:%S')}] ERROR: {str(e)}"]
        }


async def cleanup_node(state: AgentState) -> dict:
    """
    Cleanup node: Destroy sandbox.
    """
    logs = add_log(state, "Cleaning up...")
    
    sandbox_id = state["sandbox_id"]
    if sandbox_id and sandbox_id in _sandboxes:
        try:
            sandbox = _sandboxes.pop(sandbox_id)
            await sandbox.destroy()
            logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Sandbox destroyed"]
        except Exception as e:
            logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Warning: Cleanup error: {str(e)}"]
    
    # Determine final status
    final_status = "completed" if state.get("pr_url") else state.get("status", "failed")
    if state.get("error"):
        final_status = "failed"
    
    logs = logs + [f"[{datetime.now().strftime('%H:%M:%S')}] Finished with status: {final_status}"]
    
    return {
        "sandbox_id": None,
        "status": final_status,
        "current_step": "done",
        "logs": logs
    }


def should_continue(state: AgentState) -> str:
    """Determine the next node based on current step."""
    step = state.get("current_step", "setup")
    status = state.get("status", "pending")
    
    if status == "failed":
        return "cleanup"
    
    return step
