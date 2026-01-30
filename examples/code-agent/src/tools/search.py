"""Semantic search tools using FAISS indexes."""

import logging
from typing import Optional
from langchain_core.tools import tool

from src.indexer.repo_manager import RepoManager

logger = logging.getLogger(__name__)

# Global repo manager instance
_repo_manager: Optional[RepoManager] = None


def get_repo_manager() -> RepoManager:
    """Get or create the repo manager instance."""
    global _repo_manager
    if _repo_manager is None:
        _repo_manager = RepoManager()
    return _repo_manager


def create_search_tools(repo_name: str):
    """
    Create semantic search tools for a specific repository.
    
    Args:
        repo_name: Repository name (owner/repo format)
        
    Returns:
        List of LangChain tools for semantic search
    """
    manager = get_repo_manager()
    repo = manager.get_repo_by_name(repo_name)
    
    if not repo:
        logger.warning(f"Repository {repo_name} not found in index")
        return []
    
    if repo.index_status != "indexed":
        logger.warning(f"Repository {repo_name} is not indexed (status: {repo.index_status})")
        return []
    
    @tool
    def semantic_search(query: str, top_k: int = 10) -> str:
        """
        Search the codebase semantically to find relevant code.
        
        This is much faster than grep for finding conceptually related code.
        Use this to find code related to a feature, bug, or concept.
        
        Args:
            query: Natural language description of what you're looking for
            top_k: Number of results to return (default 10)
            
        Returns:
            Relevant code snippets with file paths and line numbers
        """
        results = manager.search(repo.id, query, top_k)
        
        if not results:
            return "No relevant code found."
        
        output = []
        for i, result in enumerate(results, 1):
            output.append(f"### Result {i} (score: {result['score']:.3f})")
            output.append(f"**File:** {result['file_path']}")
            output.append(f"**Lines:** {result['start_line']}-{result['end_line']}")
            output.append(f"```{result['language']}")
            output.append(result['content'])
            output.append("```\n")
        
        return "\n".join(output)
    
    @tool
    def find_similar_code(file_path: str, description: str) -> str:
        """
        Find code similar to a description or concept.
        
        Useful for finding implementations of similar features,
        patterns, or understanding how things are done elsewhere.
        
        Args:
            file_path: File to use as context (or "none" to skip)
            description: Description of the code pattern to find
            
        Returns:
            Similar code snippets from the codebase
        """
        # Combine file context with description if provided
        if file_path and file_path.lower() != "none":
            query = f"In {file_path}: {description}"
        else:
            query = description
        
        results = manager.search(repo.id, query, top_k=5)
        
        if not results:
            return "No similar code found."
        
        output = ["Found similar code patterns:\n"]
        for result in results:
            output.append(f"- **{result['file_path']}** (lines {result['start_line']}-{result['end_line']})")
            # Show just first few lines
            preview = result['content'].split('\n')[:5]
            output.append("  ```")
            output.append("  " + "\n  ".join(preview))
            if len(result['content'].split('\n')) > 5:
                output.append("  ...")
            output.append("  ```")
        
        return "\n".join(output)
    
    return [semantic_search, find_similar_code]


def check_index_available(repo_name: str) -> bool:
    """Check if an index is available for a repository."""
    manager = get_repo_manager()
    repo = manager.get_repo_by_name(repo_name)
    return repo is not None and repo.index_status == "indexed"
