# Code Agent

A LangGraph-powered coding agent that uses E2B sandboxes to safely clone repositories, modify code, and create pull requests.

## Features

- **Sandboxed Execution**: Code runs in isolated E2B sandboxes for safety
- **Git Integration**: Clones repos, creates branches, commits, and opens PRs via GitHub CLI
- **Multiple LLM Support**: Works with both Anthropic Claude and OpenAI GPT-4
- **REST API**: FastAPI server for programmatic access
- **Web UI**: Streamlit interface for easy interaction
- **Extensible**: Abstract sandbox interface allows adding new providers

## Quick Start

### 1. Install Dependencies

```bash
pip install -r requirements.txt
```

### 2. Configure Environment

Create a `.env` file with your API keys:

```env
OPENAI_API_KEY=sk-...
ANTHROPIC_API_KEY=sk-ant-...
E2B_API_KEY=e2b_...
```

### 3. Start the API Server

```bash
python -m src.api.server
```

The API will be available at `http://localhost:8000`.

### 4. Start the Web UI

In a new terminal:

```bash
streamlit run src/ui/app.py
```

The UI will be available at `http://localhost:8501`.

## Usage

### Web UI

1. Open `http://localhost:8501` in your browser
2. Enter a GitHub repository URL
3. Describe your task (e.g., "Fix issue #123", "Add unit tests for auth module")
4. Provide your GitHub personal access token
5. Click "Start Task" and watch the agent work

### API

Create a task:

```bash
curl -X POST http://localhost:8000/task \
  -H "Content-Type: application/json" \
  -d '{
    "repo_url": "https://github.com/owner/repo",
    "task": "Fix the bug in login.py",
    "github_token": "ghp_...",
    "llm_provider": "anthropic"
  }'
```

Check task status:

```bash
curl http://localhost:8000/task/{task_id}
```

Stream logs:

```bash
curl http://localhost:8000/task/{task_id}/logs
```

### Programmatic Usage

```python
import asyncio
from src.agent.graph import run_agent

async def main():
    result = await run_agent(
        repo_url="https://github.com/owner/repo",
        task="Add error handling to the API endpoints",
        github_token="ghp_...",
        llm_provider="anthropic",
    )
    
    print(f"Status: {result['status']}")
    print(f"PR URL: {result.get('pr_url')}")

asyncio.run(main())
```

## Architecture

```
code-agent/
├── src/
│   ├── config.py           # Configuration management
│   ├── models.py           # Pydantic models
│   ├── agent/
│   │   ├── graph.py        # LangGraph workflow
│   │   ├── state.py        # Agent state schema
│   │   └── nodes.py        # Workflow nodes
│   ├── sandbox/
│   │   ├── base.py         # Abstract sandbox interface
│   │   └── e2b.py          # E2B implementation
│   ├── tools/
│   │   ├── git.py          # Git operations
│   │   └── code.py         # Code manipulation
│   ├── llm/
│   │   └── provider.py     # LLM abstraction
│   ├── api/
│   │   └── server.py       # FastAPI server
│   └── ui/
│       └── app.py          # Streamlit interface
```

## Agent Workflow

1. **Setup**: Creates E2B sandbox, clones repository, creates feature branch
2. **Execute**: LLM agent explores code, makes changes, runs tests
3. **Create PR**: Pushes branch and creates pull request
4. **Cleanup**: Destroys sandbox

## Configuration

Environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `OPENAI_API_KEY` | OpenAI API key | - |
| `ANTHROPIC_API_KEY` | Anthropic API key | - |
| `E2B_API_KEY` | E2B API key | - |
| `DEFAULT_LLM_PROVIDER` | Default LLM | `anthropic` |
| `ANTHROPIC_MODEL` | Anthropic model | `claude-sonnet-4-20250514` |
| `OPENAI_MODEL` | OpenAI model | `gpt-4o` |
| `SANDBOX_TIMEOUT` | Sandbox timeout (seconds) | `1800` |
| `API_HOST` | API host | `0.0.0.0` |
| `API_PORT` | API port | `8000` |

## Adding New Sandbox Providers

Implement the `BaseSandbox` interface:

```python
from src.sandbox.base import BaseSandbox

class MySandbox(BaseSandbox):
    async def create(self, timeout: int = 1800) -> str:
        # Create sandbox, return ID
        pass
    
    async def run_command(self, command: str, workdir=None, env=None):
        # Execute command, return CommandResult
        pass
    
    async def read_file(self, path: str) -> str:
        # Read file contents
        pass
    
    async def write_file(self, path: str, content: str):
        # Write file
        pass
    
    async def list_files(self, path: str = ".") -> list[str]:
        # List directory
        pass
    
    async def destroy(self):
        # Cleanup
        pass
```

Then add it to `src/sandbox/__init__.py`.

## License

MIT
