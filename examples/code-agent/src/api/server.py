"""FastAPI server for the coding agent."""

import asyncio
import uuid
import logging
from datetime import datetime
from contextlib import asynccontextmanager
from fastapi import FastAPI, HTTPException, BackgroundTasks
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import StreamingResponse

from src.models import TaskRequest, TaskResponse, TaskStatusResponse, TaskStatus
from src.agent.graph import run_agent
from src.config import get_settings

# Configure logging
logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

# In-memory task storage (use Redis/DB for production)
tasks: dict[str, dict] = {}


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Application lifespan handler."""
    logger.info("Starting Code Agent API...")
    yield
    logger.info("Shutting down Code Agent API...")


app = FastAPI(
    title="Code Agent API",
    description="LangGraph-powered coding agent with E2B sandbox execution",
    version="0.1.0",
    lifespan=lifespan,
)

# Add CORS middleware
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],  # Configure appropriately for production
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


async def execute_task(task_id: str, request: TaskRequest):
    """Background task to execute the agent."""
    try:
        tasks[task_id]["status"] = TaskStatus.RUNNING
        tasks[task_id]["updated_at"] = datetime.now()
        
        # Run the agent
        result = await run_agent(
            repo_url=request.repo_url,
            task=request.task,
            github_token=request.github_token,
            branch_name=request.branch_name,
            base_branch=request.base_branch,
            llm_provider=request.llm_provider.value,
            model=request.model,
            max_iterations=request.max_iterations,
            sandbox_provider=request.sandbox_provider.value,
        )
        
        # Update task with results
        tasks[task_id]["status"] = TaskStatus(result.get("status", "failed"))
        tasks[task_id]["pr_url"] = result.get("pr_url")
        tasks[task_id]["error"] = result.get("error")
        tasks[task_id]["logs"] = result.get("logs", [])
        tasks[task_id]["branch_name"] = result.get("branch_name")
        tasks[task_id]["updated_at"] = datetime.now()
        
    except Exception as e:
        logger.exception(f"Task {task_id} failed")
        tasks[task_id]["status"] = TaskStatus.FAILED
        tasks[task_id]["error"] = str(e)
        tasks[task_id]["updated_at"] = datetime.now()


@app.post("/task", response_model=TaskResponse)
async def create_task(request: TaskRequest, background_tasks: BackgroundTasks):
    """
    Create a new coding task.
    
    The task will be executed in the background. Use the task_id to check status.
    """
    task_id = str(uuid.uuid4())
    
    # Store task info
    tasks[task_id] = {
        "task_id": task_id,
        "status": TaskStatus.PENDING,
        "created_at": datetime.now(),
        "updated_at": datetime.now(),
        "repo_url": request.repo_url,
        "task": request.task,
        "branch_name": request.branch_name,
        "pr_url": None,
        "error": None,
        "logs": [],
    }
    
    # Start background execution
    background_tasks.add_task(execute_task, task_id, request)
    
    return TaskResponse(
        task_id=task_id,
        status=TaskStatus.PENDING,
        message="Task created and queued for execution"
    )


@app.get("/task/{task_id}", response_model=TaskStatusResponse)
async def get_task_status(task_id: str):
    """Get the status of a task."""
    if task_id not in tasks:
        raise HTTPException(status_code=404, detail="Task not found")
    
    task = tasks[task_id]
    return TaskStatusResponse(
        task_id=task["task_id"],
        status=task["status"],
        created_at=task["created_at"],
        updated_at=task["updated_at"],
        repo_url=task["repo_url"],
        task=task["task"],
        branch_name=task.get("branch_name"),
        pr_url=task.get("pr_url"),
        error=task.get("error"),
        logs=task.get("logs", []),
    )


@app.get("/task/{task_id}/logs")
async def stream_task_logs(task_id: str):
    """Stream task logs as server-sent events."""
    if task_id not in tasks:
        raise HTTPException(status_code=404, detail="Task not found")
    
    async def log_generator():
        last_log_count = 0
        while True:
            if task_id not in tasks:
                break
                
            task = tasks[task_id]
            logs = task.get("logs", [])
            
            # Send new logs
            if len(logs) > last_log_count:
                for log in logs[last_log_count:]:
                    yield f"data: {log}\n\n"
                last_log_count = len(logs)
            
            # Check if task is done
            if task["status"] in [TaskStatus.COMPLETED, TaskStatus.FAILED, TaskStatus.CANCELLED]:
                yield f"data: [DONE] Status: {task['status'].value}\n\n"
                break
            
            await asyncio.sleep(1)
    
    return StreamingResponse(
        log_generator(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
        }
    )


@app.post("/task/{task_id}/cancel")
async def cancel_task(task_id: str):
    """Cancel a running task."""
    if task_id not in tasks:
        raise HTTPException(status_code=404, detail="Task not found")
    
    task = tasks[task_id]
    if task["status"] == TaskStatus.RUNNING:
        task["status"] = TaskStatus.CANCELLED
        task["updated_at"] = datetime.now()
        return {"message": "Task cancellation requested"}
    else:
        return {"message": f"Task is not running (status: {task['status'].value})"}


@app.get("/tasks")
async def list_tasks(limit: int = 10):
    """List recent tasks."""
    sorted_tasks = sorted(
        tasks.values(),
        key=lambda t: t["created_at"],
        reverse=True
    )[:limit]
    
    return [
        {
            "task_id": t["task_id"],
            "status": t["status"].value,
            "repo_url": t["repo_url"],
            "task": t["task"][:100],
            "created_at": t["created_at"].isoformat(),
            "pr_url": t.get("pr_url"),
        }
        for t in sorted_tasks
    ]


@app.get("/health")
async def health_check():
    """Health check endpoint."""
    return {"status": "healthy"}


def start_server():
    """Start the API server."""
    import uvicorn
    settings = get_settings()
    uvicorn.run(
        "src.api.server:app",
        host=settings.api_host,
        port=settings.api_port,
        reload=True,
    )


if __name__ == "__main__":
    start_server()
