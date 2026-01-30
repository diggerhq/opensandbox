"""Git operations tools for the agent."""

import logging
from typing import Optional
from langchain_core.tools import tool

from src.sandbox.base import BaseSandbox

logger = logging.getLogger(__name__)


class GitTools:
    """
    Git operations that run in a sandbox.
    
    Provides tools for cloning repos, creating branches, committing, and PRs.
    """
    
    def __init__(self, sandbox: BaseSandbox, github_token: str, workdir: str = "/home/user/repo"):
        self.sandbox = sandbox
        self.github_token = github_token
        self.workdir = workdir
    
    async def setup_git_auth(self) -> None:
        """Configure git for commits. GH_TOKEN is passed to commands that need auth."""
        # Set up git config
        await self.sandbox.run_command('git config --global user.email "agent@code-agent.local"')
        await self.sandbox.run_command('git config --global user.name "Code Agent"')
        
        # Configure git to use gh as credential helper (uses GH_TOKEN)
        await self.sandbox.run_command(
            'gh auth setup-git',
            env={"GH_TOKEN": self.github_token}
        )
        
        logger.info("Git configured with GitHub authentication.")
    
    async def clone_repo(self, repo_url: str, shallow: bool = True) -> str:
        """
        Clone a repository into the sandbox.
        
        Args:
            repo_url: GitHub repository URL (https://github.com/owner/repo)
            shallow: Use shallow clone (--depth 1) to save disk space (default True)
            
        Returns:
            Path to the cloned repository
        """
        logger.info(f"Cloning repository: {repo_url} (shallow={shallow})")
        
        # Extract owner/repo from URL
        # https://github.com/owner/repo -> owner/repo
        if "github.com" in repo_url:
            parts = repo_url.rstrip("/").split("/")
            owner_repo = f"{parts[-2]}/{parts[-1]}"
        else:
            owner_repo = repo_url
        
        # Use git clone with token in URL for auth
        # This avoids issues with gh auth and allows --depth flag
        auth_url = f"https://{self.github_token}@github.com/{owner_repo}"
        
        depth_flag = "--depth 1" if shallow else ""
        result = await self.sandbox.run_command(
            f"git clone {depth_flag} {auth_url} {self.workdir}",
            env={"GH_TOKEN": self.github_token}
        )
        
        if result.exit_code != 0:
            raise RuntimeError(f"Failed to clone repository: {result.stderr}")
        
        # Remove token from remote URL for safety (use gh for push instead)
        await self.sandbox.run_command(
            f"git remote set-url origin https://github.com/{owner_repo}",
            workdir=self.workdir
        )
        
        logger.info(f"Repository cloned to {self.workdir}")
        return self.workdir
    
    async def checkout_branch(self, branch_name: str) -> None:
        """
        Checkout an existing branch.
        
        Args:
            branch_name: Name of the branch to checkout
        """
        logger.info(f"Checking out branch: {branch_name}")
        
        result = await self.sandbox.run_command(
            f"git checkout {branch_name}",
            workdir=self.workdir
        )
        
        if result.exit_code != 0:
            # Try fetching and checking out origin/branch
            result = await self.sandbox.run_command(
                f"git checkout -b {branch_name} origin/{branch_name}",
                workdir=self.workdir
            )
            if result.exit_code != 0:
                raise RuntimeError(f"Failed to checkout branch: {result.stderr}")
    
    async def create_branch(self, branch_name: str) -> None:
        """
        Create and checkout a new branch.
        
        Args:
            branch_name: Name for the new branch
        """
        logger.info(f"Creating branch: {branch_name}")
        
        result = await self.sandbox.run_command(
            f"git checkout -b {branch_name}",
            workdir=self.workdir
        )
        
        if result.exit_code != 0:
            raise RuntimeError(f"Failed to create branch: {result.stderr}")
    
    async def get_current_branch(self) -> str:
        """Get the current branch name."""
        result = await self.sandbox.run_command(
            "git branch --show-current",
            workdir=self.workdir
        )
        return result.stdout.strip()
    
    async def get_default_branch(self) -> str:
        """Get the repository's default branch (e.g., main, master, develop)."""
        # Try to get the default branch from the remote
        result = await self.sandbox.run_command(
            "git remote show origin | grep 'HEAD branch' | awk '{print $NF}'",
            workdir=self.workdir,
            env={"GH_TOKEN": self.github_token}
        )
        
        if result.exit_code == 0 and result.stdout.strip():
            default_branch = result.stdout.strip()
            logger.info(f"Detected default branch: {default_branch}")
            return default_branch
        
        # Fallback: check if common branches exist
        for branch in ["main", "master", "develop"]:
            result = await self.sandbox.run_command(
                f"git rev-parse --verify origin/{branch} 2>/dev/null",
                workdir=self.workdir
            )
            if result.exit_code == 0:
                logger.info(f"Using fallback default branch: {branch}")
                return branch
        
        # Last resort
        logger.warning("Could not detect default branch, using 'main'")
        return "main"
    
    async def stage_all(self) -> None:
        """Stage all changes."""
        await self.sandbox.run_command("git add -A", workdir=self.workdir)
    
    async def commit(self, message: str) -> None:
        """
        Commit staged changes.
        
        Args:
            message: Commit message
        """
        logger.info(f"Committing: {message}")
        
        await self.stage_all()
        
        result = await self.sandbox.run_command(
            f'git commit -m "{message}"',
            workdir=self.workdir
        )
        
        if result.exit_code != 0:
            if "nothing to commit" in result.stdout:
                logger.warning("Nothing to commit")
                return
            raise RuntimeError(f"Failed to commit: {result.stderr}")
    
    async def push(self, branch_name: str) -> None:
        """
        Push branch to remote.
        
        Args:
            branch_name: Branch to push
        """
        logger.info(f"Pushing branch: {branch_name}")
        
        # Get the remote URL and update it to use token auth
        result = await self.sandbox.run_command(
            "git remote get-url origin",
            workdir=self.workdir
        )
        remote_url = result.stdout.strip()
        
        # Convert to authenticated URL if it's a GitHub HTTPS URL
        if "github.com" in remote_url and not "@" in remote_url:
            # https://github.com/owner/repo -> https://token@github.com/owner/repo
            auth_url = remote_url.replace("https://github.com", f"https://{self.github_token}@github.com")
            await self.sandbox.run_command(
                f'git remote set-url origin "{auth_url}"',
                workdir=self.workdir
            )
        
        result = await self.sandbox.run_command(
            f"git push -u origin {branch_name}",
            workdir=self.workdir
        )
        
        # Restore original URL (don't leave token in git config)
        if "github.com" in remote_url:
            await self.sandbox.run_command(
                f'git remote set-url origin "{remote_url}"',
                workdir=self.workdir
            )
        
        if result.exit_code != 0:
            # Log more details for debugging
            logger.error(f"Push failed. Exit code: {result.exit_code}")
            logger.error(f"Push stderr: {result.stderr}")
            logger.error(f"Push stdout: {result.stdout}")
            
            # Check git status for debugging
            status_result = await self.sandbox.run_command("git status", workdir=self.workdir)
            logger.error(f"Git status: {status_result.stdout}")
            
            raise RuntimeError(f"Failed to push branch '{branch_name}': {result.stderr}")
    
    async def create_pr(self, title: str, body: str, base: str | None = None) -> str:
        """
        Create a pull request.
        
        Args:
            title: PR title
            body: PR description
            base: Base branch (None = auto-detect from repo)
            
        Returns:
            URL of the created PR
        """
        # Auto-detect base branch if not specified
        if base is None:
            base = await self.get_default_branch()
        
        # Get current branch name
        result = await self.sandbox.run_command(
            "git branch --show-current",
            workdir=self.workdir
        )
        current_branch = result.stdout.strip()
        
        logger.info(f"Creating PR: {title} (base: {base}, branch: {current_branch})")
        
        # Push the current branch first
        await self.push(current_branch)
        
        result = await self.sandbox.run_command(
            f'gh pr create --title "{title}" --body "{body}" --base {base}',
            workdir=self.workdir,
            env={"GH_TOKEN": self.github_token}
        )
        
        if result.exit_code != 0:
            raise RuntimeError(f"Failed to create PR: {result.stderr}")
        
        # Extract PR URL from output
        pr_url = result.stdout.strip().split("\n")[-1]
        logger.info(f"Created PR: {pr_url}")
        return pr_url
    
    async def get_diff(self) -> str:
        """Get the current diff."""
        result = await self.sandbox.run_command("git diff", workdir=self.workdir)
        return result.stdout
    
    async def get_status(self) -> str:
        """Get git status."""
        result = await self.sandbox.run_command("git status", workdir=self.workdir)
        return result.stdout
    
    async def fetch_issue(self, issue_number: int) -> str:
        """
        Fetch a GitHub issue's content.
        
        Args:
            issue_number: The issue number (e.g., 2552)
            
        Returns:
            Issue title, body, and comments
        """
        logger.info(f"Fetching issue #{issue_number}")
        
        # Get issue details
        result = await self.sandbox.run_command(
            f"gh issue view {issue_number} --json title,body,comments,state,labels",
            workdir=self.workdir,
            env={"GH_TOKEN": self.github_token}
        )
        
        if result.exit_code != 0:
            return f"Error fetching issue: {result.stderr}"
        
        return result.stdout
    
    async def fetch_pr(self, pr_number: int) -> str:
        """
        Fetch a GitHub PR's content.
        
        Args:
            pr_number: The PR number
            
        Returns:
            PR title, body, and diff
        """
        logger.info(f"Fetching PR #{pr_number}")
        
        # Get PR details
        result = await self.sandbox.run_command(
            f"gh pr view {pr_number} --json title,body,state,files",
            workdir=self.workdir,
            env={"GH_TOKEN": self.github_token}
        )
        
        if result.exit_code != 0:
            return f"Error fetching PR: {result.stderr}"
        
        return result.stdout
    
    async def run_gh_command(self, command: str) -> str:
        """
        Run an arbitrary gh CLI command with authentication.
        
        Args:
            command: The gh command (without 'gh' prefix)
            
        Returns:
            Command output
        """
        result = await self.sandbox.run_command(
            f"gh {command}",
            workdir=self.workdir,
            env={"GH_TOKEN": self.github_token}
        )
        
        output = result.stdout
        if result.stderr:
            output += f"\nSTDERR: {result.stderr}"
        if result.exit_code != 0:
            output += f"\n(exit code: {result.exit_code})"
        return output


def create_git_tools(sandbox: BaseSandbox, github_token: str, workdir: str = "/home/user/repo"):
    """
    Create LangChain tools for git operations.
    
    Returns a list of tools that can be bound to an LLM.
    """
    git = GitTools(sandbox, github_token, workdir)
    
    @tool
    async def git_clone(repo_url: str) -> str:
        """Clone a GitHub repository. Returns the path to the cloned repo."""
        return await git.clone_repo(repo_url)
    
    @tool
    async def git_create_branch(branch_name: str) -> str:
        """Create and checkout a new git branch."""
        await git.create_branch(branch_name)
        return f"Created and checked out branch: {branch_name}"
    
    @tool
    async def git_commit(message: str) -> str:
        """Stage all changes and commit with the given message."""
        await git.commit(message)
        return f"Committed changes: {message}"
    
    @tool
    async def git_push(branch_name: str) -> str:
        """Push the branch to the remote repository."""
        await git.push(branch_name)
        return f"Pushed branch: {branch_name}"
    
    @tool
    async def git_create_pr(title: str, body: str, base: str = "main") -> str:
        """Create a pull request. Returns the PR URL."""
        return await git.create_pr(title, body, base)
    
    @tool
    async def git_diff() -> str:
        """Get the current git diff showing all changes."""
        return await git.get_diff()
    
    @tool
    async def git_status() -> str:
        """Get the current git status."""
        return await git.get_status()
    
    @tool
    async def fetch_github_issue(issue_number: int) -> str:
        """Fetch a GitHub issue's content including title, body, and comments. Use this to understand what an issue is asking for before making changes."""
        return await git.fetch_issue(issue_number)
    
    @tool
    async def fetch_github_pr(pr_number: int) -> str:
        """Fetch a GitHub PR's content including title, body, and changed files."""
        return await git.fetch_pr(pr_number)
    
    @tool
    async def run_gh_command(command: str) -> str:
        """Run a GitHub CLI (gh) command with authentication. Example: 'issue list', 'pr list', 'repo view'. Do NOT include 'gh' prefix."""
        return await git.run_gh_command(command)
    
    return [git_clone, git_create_branch, git_commit, git_push, git_create_pr, git_diff, git_status, fetch_github_issue, fetch_github_pr, run_gh_command], git
