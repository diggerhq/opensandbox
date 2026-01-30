"""Streamlit UI for the coding agent."""

import streamlit as st
import httpx
import time
from datetime import datetime

# Configuration
API_URL = "http://localhost:8000"

st.set_page_config(
    page_title="Code Agent",
    page_icon="🤖",
    layout="wide",
)

# Custom CSS
st.markdown("""
<style>
    .stTextArea textarea {
        font-family: monospace;
    }
    .log-container {
        background-color: #1e1e1e;
        color: #d4d4d4;
        padding: 1rem;
        border-radius: 0.5rem;
        font-family: monospace;
        font-size: 0.85rem;
        max-height: 400px;
        overflow-y: auto;
    }
    .status-badge {
        padding: 0.25rem 0.5rem;
        border-radius: 0.25rem;
        font-weight: bold;
    }
    .status-pending { background-color: #ffc107; color: black; }
    .status-running { background-color: #17a2b8; color: white; }
    .status-completed { background-color: #28a745; color: white; }
    .status-failed { background-color: #dc3545; color: white; }
    .repo-card {
        border: 1px solid #ddd;
        border-radius: 8px;
        padding: 12px;
        margin-bottom: 8px;
    }
</style>
""", unsafe_allow_html=True)

# Header
st.title("🤖 Code Agent")
st.markdown("*LangGraph-powered coding agent with sandboxed execution*")

# Create tabs for different sections
tab1, tab2 = st.tabs(["📝 Tasks", "📂 Repositories"])

# Import repo manager for the repositories tab
try:
    from src.indexer.repo_manager import RepoManager
    repo_manager = RepoManager()
    REPO_MANAGER_AVAILABLE = True
except ImportError:
    REPO_MANAGER_AVAILABLE = False
    repo_manager = None


# Sidebar for configuration
with st.sidebar:
    st.header("⚙️ Configuration")
    
    llm_provider = st.selectbox(
        "LLM Provider",
        ["anthropic", "openai"],
        index=0,
        help="Select the LLM provider to use"
    )
    
    # Model selection based on provider
    model_options = {
        "anthropic": [
            ("", "Default (Claude Sonnet 4.5)"),
            ("claude-sonnet-4-5-20250929", "Claude Sonnet 4.5 - Best for coding"),
            ("claude-haiku-4-5-20251001", "Claude Haiku 4.5 - Fastest"),
            ("claude-opus-4-5-20251101", "Claude Opus 4.5 - Most intelligent"),
        ],
        "openai": [
            ("", "Default (GPT-5.2)"),
            ("gpt-5.2", "GPT-5.2 - Latest, best for coding"),
            ("gpt-5-mini", "GPT-5 mini - Fast, cost-efficient"),
            ("gpt-5-nano", "GPT-5 nano - Fastest, cheapest"),
            ("gpt-4.1", "GPT-4.1 - Smart non-reasoning"),
            ("gpt-4o", "GPT-4o - Fast, flexible"),
            ("gpt-4o-mini", "GPT-4o mini - Affordable"),
        ],
    }
    
    available_models = model_options.get(llm_provider, [])
    model_display = [m[1] for m in available_models]
    model_values = [m[0] for m in available_models]
    
    selected_model_idx = st.selectbox(
        "Model",
        range(len(model_display)),
        format_func=lambda i: model_display[i],
        help="Select specific model (or use provider default)"
    )
    selected_model = model_values[selected_model_idx] if selected_model_idx else ""
    
    st.divider()
    
    # Sandbox provider selection
    sandbox_provider = st.selectbox(
        "Sandbox Provider",
        ["opensandbox", "e2b"],
        index=0,
        help="OpenSandbox (local) or E2B (cloud)"
    )
    
    if sandbox_provider == "opensandbox":
        st.caption("🖥️ Using local OpenSandbox (localhost:8080)")
    else:
        st.caption("☁️ Using E2B cloud sandbox")
    
    st.divider()
    
    # Iterations slider
    max_iterations = st.slider(
        "Max Iterations",
        min_value=10,
        max_value=200,
        value=50,
        step=10,
        help="Maximum number of LLM tool-calling iterations"
    )
    
    st.divider()
    
    st.header("📋 Recent Tasks")
    
    # Fetch recent tasks
    try:
        response = httpx.get(f"{API_URL}/tasks", timeout=5)
        if response.status_code == 200:
            recent_tasks = response.json()
            for task in recent_tasks[:5]:
                status = task["status"]
                status_color = {
                    "pending": "🟡",
                    "running": "🔵",
                    "completed": "🟢",
                    "failed": "🔴",
                    "cancelled": "⚪"
                }.get(status, "⚪")
                
                with st.expander(f"{status_color} {task['task'][:30]}..."):
                    st.text(f"ID: {task['task_id'][:8]}...")
                    st.text(f"Repo: {task['repo_url']}")
                    if task.get("pr_url"):
                        st.markdown(f"[View PR]({task['pr_url']})")
        else:
            st.info("No recent tasks")
    except Exception:
        st.warning("API not available")


# Tab 1: Tasks
with tab1:
    col1, col2 = st.columns([1, 1])

    with col1:
        st.header("📝 New Task")
        
        # Get list of indexed repos for dropdown
        indexed_repos = []
        if REPO_MANAGER_AVAILABLE and repo_manager:
            repos = repo_manager.list_repos()
            indexed_repos = [r for r in repos if r.index_status == "indexed"]
        
        # Option to use indexed repo or enter URL
        use_indexed_repo = st.checkbox(
            "Use indexed repository",
            value=len(indexed_repos) > 0,
            help="Select from pre-indexed repos for faster search"
        )
        
        if use_indexed_repo and indexed_repos:
            repo_options = {r.name: r.url for r in indexed_repos}
            selected_repo_name = st.selectbox(
                "Repository",
                options=list(repo_options.keys()),
                help="Select a pre-indexed repository"
            )
            repo_url = repo_options.get(selected_repo_name, "")
            st.caption(f"✅ Semantic search enabled ({indexed_repos[0].chunk_count} chunks indexed)")
        else:
            repo_url = st.text_input(
                "Repository URL",
                placeholder="https://github.com/owner/repo",
                help="GitHub repository URL to work on"
            )
            if repo_url:
                st.caption("⚠️ No index - agent will use grep-based search")
        
        task_description = st.text_area(
            "Task Description",
            placeholder="Describe what you want the agent to do...\n\nExamples:\n- Fix issue #123\n- Add unit tests for the auth module\n- Refactor the database connection code",
            height=150,
            help="Describe the task for the agent"
        )
        
        with st.expander("🔐 Authentication & Branch Settings"):
            github_token = st.text_input(
                "GitHub Token",
                type="password",
                help="Personal access token with repo permissions"
            )
            
            branch_name = st.text_input(
                "Branch Name (optional)",
                placeholder="agent/my-feature",
                help="Custom branch name (auto-generated if empty)"
            )
            
            base_branch = st.text_input(
                "Base Branch (optional)",
                placeholder="main, develop, master...",
                help="Branch to base PR against (auto-detected from repo if empty)"
            )
        
        submit_col1, submit_col2 = st.columns([1, 1])
        
        with submit_col1:
            submit_button = st.button("🚀 Start Task", type="primary", use_container_width=True)
        
        with submit_col2:
            if st.button("🔄 Check API", use_container_width=True):
                try:
                    response = httpx.get(f"{API_URL}/health", timeout=5)
                    if response.status_code == 200:
                        st.success("API is healthy!")
                    else:
                        st.error("API returned error")
                except Exception as e:
                    st.error(f"Cannot connect to API: {e}")

    with col2:
        st.header("📊 Task Status")
        
        # Task ID input for checking status
        check_task_id = st.text_input(
            "Task ID",
            placeholder="Enter task ID to check status",
            key="check_task_id"
        )
        
        # Session state for current task
        if "current_task_id" not in st.session_state:
            st.session_state.current_task_id = None
        
        # Use either the input task ID or the current session task
        active_task_id = check_task_id or st.session_state.current_task_id
        
        if active_task_id:
            try:
                response = httpx.get(f"{API_URL}/task/{active_task_id}", timeout=10)
                if response.status_code == 200:
                    task_data = response.json()
                    
                    # Status display
                    status = task_data["status"]
                    status_emoji = {
                        "pending": "🟡",
                        "running": "🔵",
                        "completed": "🟢",
                        "failed": "🔴",
                        "cancelled": "⚪"
                    }.get(status, "⚪")
                    
                    st.markdown(f"### Status: {status_emoji} {status.upper()}")
                    
                    # Extract sandbox provider from logs
                    logs = task_data.get("logs", [])
                    sandbox_used = "unknown"
                    for log in logs:
                        if "Creating opensandbox" in log:
                            sandbox_used = "opensandbox"
                            break
                        elif "Creating e2b" in log:
                            sandbox_used = "e2b"
                            break
                    
                    info_cols = st.columns(3)
                    with info_cols[0]:
                        st.text(f"Repository: {task_data['repo_url']}")
                    with info_cols[1]:
                        st.text(f"Branch: {task_data.get('branch_name', 'N/A')}")
                    with info_cols[2]:
                        sandbox_icon = "🖥️" if sandbox_used == "opensandbox" else "☁️"
                        st.text(f"Sandbox: {sandbox_icon} {sandbox_used}")
                    
                    if task_data.get("pr_url"):
                        st.success(f"✅ PR Created!")
                        st.markdown(f"[🔗 View Pull Request]({task_data['pr_url']})")
                    
                    if task_data.get("error"):
                        st.error(f"Error: {task_data['error']}")
                    
                    # Parse timing metrics from logs (logs already fetched above)
                    import re
                    timings = {}
                    for log in logs:
                        # Parse "Sandbox created: abc (1.2s)"
                        if "Sandbox created:" in log:
                            match = re.search(r'\((\d+\.?\d*)s\)', log)
                            if match:
                                timings["sandbox"] = float(match.group(1))
                        # Parse "GitHub authentication configured (0.3s)"
                        elif "authentication configured" in log:
                            match = re.search(r'\((\d+\.?\d*)s\)', log)
                            if match:
                                timings["auth"] = float(match.group(1))
                        # Parse "Repository cloned (14.2s)"
                        elif "Repository cloned" in log:
                            match = re.search(r'\((\d+\.?\d*)s\)', log)
                            if match:
                                timings["clone"] = float(match.group(1))
                        # Parse "Setup complete in 15.8s"
                        elif "Setup complete in" in log:
                            match = re.search(r'in (\d+\.?\d*)s', log)
                            if match:
                                timings["total_setup"] = float(match.group(1))
                    
                    # Display timing metrics if available
                    if timings:
                        st.subheader("⏱️ Performance Metrics")
                        metric_cols = st.columns(4)
                        with metric_cols[0]:
                            if "sandbox" in timings:
                                st.metric("Sandbox Spin-up", f"{timings['sandbox']:.1f}s")
                        with metric_cols[1]:
                            if "auth" in timings:
                                st.metric("Git Auth", f"{timings['auth']:.1f}s")
                        with metric_cols[2]:
                            if "clone" in timings:
                                st.metric("Clone", f"{timings['clone']:.1f}s")
                        with metric_cols[3]:
                            if "total_setup" in timings:
                                st.metric("Total Setup", f"{timings['total_setup']:.1f}s")
                    
                    # Logs
                    st.subheader("📜 Logs")
                    if logs:
                        log_text = "\n".join(logs)
                        st.code(log_text, language="text")
                    else:
                        st.info("No logs yet...")
                    
                    # Auto-refresh for running tasks
                    if status == "running":
                        time.sleep(2)
                        st.rerun()
                        
                elif response.status_code == 404:
                    st.warning("Task not found")
                else:
                    st.error(f"Error fetching task: {response.status_code}")
                    
            except Exception as e:
                st.error(f"Error: {e}")
        else:
            st.info("Submit a task or enter a task ID to see status")

    # Handle task submission
    if submit_button:
        if not repo_url:
            st.error("Please enter a repository URL")
        elif not task_description:
            st.error("Please enter a task description")
        elif not github_token:
            st.error("Please enter your GitHub token")
        else:
            try:
                payload = {
                    "repo_url": repo_url,
                    "task": task_description,
                    "github_token": github_token,
                    "llm_provider": llm_provider,
                    "sandbox_provider": sandbox_provider,
                    "max_iterations": max_iterations,
                }
                if selected_model:
                    payload["model"] = selected_model
                if branch_name:
                    payload["branch_name"] = branch_name
                if base_branch:
                    payload["base_branch"] = base_branch
                
                response = httpx.post(
                    f"{API_URL}/task",
                    json=payload,
                    timeout=30
                )
                
                if response.status_code == 200:
                    result = response.json()
                    st.session_state.current_task_id = result["task_id"]
                    st.success(f"Task created! ID: {result['task_id']}")
                    st.rerun()
                else:
                    st.error(f"Failed to create task: {response.text}")
                    
            except Exception as e:
                st.error(f"Error submitting task: {e}")


# Tab 2: Repositories
with tab2:
    st.header("📂 Repository Management")
    st.markdown("Index repositories for faster semantic search during tasks.")
    
    if not REPO_MANAGER_AVAILABLE:
        st.error("Repository manager not available. Make sure dependencies are installed: `pip install faiss-cpu openai numpy`")
    else:
        # Add new repository
        st.subheader("➕ Add Repository")
        
        col1, col2 = st.columns([3, 1])
        with col1:
            new_repo_url = st.text_input(
                "Repository URL",
                placeholder="https://github.com/owner/repo or owner/repo",
                key="new_repo_url"
            )
        with col2:
            add_repo_button = st.button("Add Repository", type="primary", use_container_width=True)
        
        if add_repo_button and new_repo_url:
            with st.spinner("Cloning repository..."):
                try:
                    repo_info = repo_manager.add_repo(new_repo_url)
                    st.success(f"Added repository: {repo_info.name}")
                    st.rerun()
                except Exception as e:
                    st.error(f"Failed to add repository: {e}")
        
        st.divider()
        
        # List repositories
        st.subheader("📋 Indexed Repositories")
        
        repos = repo_manager.list_repos()
        
        if not repos:
            st.info("No repositories added yet. Add a repository above to get started.")
        else:
            for repo in repos:
                with st.container():
                    col1, col2, col3 = st.columns([3, 1, 1])
                    
                    with col1:
                        status_icon = {
                            "indexed": "✅",
                            "indexing": "⏳",
                            "not_indexed": "⚪",
                            "error": "❌"
                        }.get(repo.index_status, "❓")
                        
                        st.markdown(f"### {status_icon} {repo.name}")
                        st.caption(f"URL: {repo.url}")
                        
                        if repo.index_status == "indexed":
                            st.caption(f"📊 {repo.chunk_count} chunks indexed")
                            if repo.last_indexed:
                                st.caption(f"🕐 Last indexed: {repo.last_indexed.strftime('%Y-%m-%d %H:%M')}")
                        elif repo.index_status == "error":
                            st.caption("❌ Indexing failed")
                    
                    with col2:
                        if repo.index_status == "not_indexed":
                            if st.button("🔨 Build Index", key=f"build_{repo.id}", use_container_width=True):
                                with st.spinner(f"Building index for {repo.name}..."):
                                    try:
                                        repo_manager.build_index(repo.id)
                                        st.success("Index built successfully!")
                                        st.rerun()
                                    except Exception as e:
                                        st.error(f"Failed to build index: {e}")
                        elif repo.index_status == "indexed":
                            if st.button("🔄 Rebuild", key=f"rebuild_{repo.id}", use_container_width=True):
                                with st.spinner(f"Rebuilding index for {repo.name}..."):
                                    try:
                                        repo_manager.build_index(repo.id)
                                        st.success("Index rebuilt successfully!")
                                        st.rerun()
                                    except Exception as e:
                                        st.error(f"Failed to rebuild index: {e}")
                        elif repo.index_status == "indexing":
                            st.button("⏳ Indexing...", key=f"indexing_{repo.id}", disabled=True, use_container_width=True)
                        elif repo.index_status == "error":
                            if st.button("🔄 Retry", key=f"retry_{repo.id}", use_container_width=True):
                                with st.spinner(f"Retrying index for {repo.name}..."):
                                    try:
                                        repo_manager.build_index(repo.id)
                                        st.success("Index built successfully!")
                                        st.rerun()
                                    except Exception as e:
                                        st.error(f"Failed to build index: {e}")
                    
                    with col3:
                        if st.button("🗑️ Delete", key=f"delete_{repo.id}", use_container_width=True):
                            repo_manager.delete_repo(repo.id)
                            st.success(f"Deleted {repo.name}")
                            st.rerun()
                    
                    st.divider()
        
        # Search test section
        if repos:
            st.subheader("🔍 Test Semantic Search")
            
            indexed_repos = [r for r in repos if r.index_status == "indexed"]
            if indexed_repos:
                search_repo = st.selectbox(
                    "Repository",
                    options=[r.name for r in indexed_repos],
                    key="search_repo"
                )
                
                search_query = st.text_input(
                    "Search Query",
                    placeholder="e.g., authentication handling, database connection, error handling",
                    key="search_query"
                )
                
                if st.button("🔍 Search", use_container_width=False):
                    if search_query:
                        repo = next(r for r in indexed_repos if r.name == search_repo)
                        with st.spinner("Searching..."):
                            results = repo_manager.search(repo.id, search_query, top_k=5)
                            
                            if results:
                                st.success(f"Found {len(results)} results")
                                for i, result in enumerate(results, 1):
                                    with st.expander(f"Result {i}: {result['file_path']} (score: {result['score']:.3f})"):
                                        st.caption(f"Lines {result['start_line']}-{result['end_line']}")
                                        st.code(result['content'], language=result['language'])
                            else:
                                st.warning("No results found")
                    else:
                        st.warning("Enter a search query")
            else:
                st.info("No indexed repositories available. Build an index first.")


# Footer
st.divider()
st.markdown("""
<div style="text-align: center; color: #666;">
    <small>Code Agent v0.1.0 | Powered by LangGraph + E2B + FAISS</small>
</div>
""", unsafe_allow_html=True)
