"""Repository manager with SQLite persistence."""

import os
import sqlite3
import logging
import subprocess
import shutil
from datetime import datetime
from pathlib import Path
from typing import List, Optional, Dict, Any
from dataclasses import dataclass

from .chunker import chunk_codebase
from .faiss_store import FAISSStore

logger = logging.getLogger(__name__)

# Default paths
DATA_DIR = Path("data")
REPOS_DIR = DATA_DIR / "repos"
INDEXES_DIR = DATA_DIR / "indexes"
DB_PATH = DATA_DIR / "repos.db"


@dataclass
class RepoInfo:
    """Information about a tracked repository."""
    id: int
    name: str  # owner/repo format
    url: str
    local_path: str
    index_status: str  # "not_indexed", "indexing", "indexed", "error"
    chunk_count: int
    last_indexed: Optional[datetime]
    created_at: datetime
    
    @property
    def display_name(self) -> str:
        return self.name
    
    @property
    def index_path(self) -> str:
        """Get the path to the index directory."""
        safe_name = self.name.replace("/", "_")
        return str(INDEXES_DIR / safe_name)


class RepoManager:
    """
    Manages repositories and their indexes.
    
    Uses SQLite for persistence and FAISS for vector search.
    """
    
    def __init__(self):
        """Initialize the repo manager."""
        # Ensure directories exist
        DATA_DIR.mkdir(exist_ok=True)
        REPOS_DIR.mkdir(exist_ok=True)
        INDEXES_DIR.mkdir(exist_ok=True)
        
        self._init_db()
    
    def _init_db(self):
        """Initialize the SQLite database."""
        conn = sqlite3.connect(DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            CREATE TABLE IF NOT EXISTS repos (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                name TEXT UNIQUE NOT NULL,
                url TEXT NOT NULL,
                local_path TEXT NOT NULL,
                index_status TEXT DEFAULT 'not_indexed',
                chunk_count INTEGER DEFAULT 0,
                last_indexed TIMESTAMP,
                created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
            )
        """)
        
        conn.commit()
        conn.close()
    
    def _get_conn(self) -> sqlite3.Connection:
        """Get a database connection."""
        conn = sqlite3.connect(DB_PATH)
        conn.row_factory = sqlite3.Row
        return conn
    
    def add_repo(self, url: str, clone: bool = True) -> RepoInfo:
        """
        Add a repository to track.
        
        Args:
            url: GitHub repository URL
            clone: Whether to clone the repo
            
        Returns:
            RepoInfo for the added repo
        """
        # Parse repo name from URL
        # https://github.com/owner/repo or owner/repo
        if "github.com" in url:
            parts = url.rstrip("/").split("/")
            name = f"{parts[-2]}/{parts[-1]}"
            if name.endswith(".git"):
                name = name[:-4]
        else:
            name = url
            url = f"https://github.com/{name}"
        
        # Local path for cloned repo
        safe_name = name.replace("/", "_")
        local_path = str(REPOS_DIR / safe_name)
        
        # Clone if needed
        if clone and not Path(local_path).exists():
            logger.info(f"Cloning {url} to {local_path}")
            result = subprocess.run(
                ["git", "clone", url, local_path],
                capture_output=True,
                text=True
            )
            if result.returncode != 0:
                raise RuntimeError(f"Failed to clone: {result.stderr}")
        
        # Add to database
        conn = self._get_conn()
        cursor = conn.cursor()
        
        try:
            cursor.execute(
                "INSERT INTO repos (name, url, local_path) VALUES (?, ?, ?)",
                (name, url, local_path)
            )
            conn.commit()
            repo_id = cursor.lastrowid
        except sqlite3.IntegrityError:
            # Repo already exists, get existing
            cursor.execute("SELECT id FROM repos WHERE name = ?", (name,))
            repo_id = cursor.fetchone()["id"]
        finally:
            conn.close()
        
        return self.get_repo(repo_id)
    
    def get_repo(self, repo_id: int) -> Optional[RepoInfo]:
        """Get a repository by ID."""
        conn = self._get_conn()
        cursor = conn.cursor()
        
        cursor.execute("SELECT * FROM repos WHERE id = ?", (repo_id,))
        row = cursor.fetchone()
        conn.close()
        
        if row:
            return self._row_to_repo_info(row)
        return None
    
    def get_repo_by_name(self, name: str) -> Optional[RepoInfo]:
        """Get a repository by name (owner/repo)."""
        conn = self._get_conn()
        cursor = conn.cursor()
        
        cursor.execute("SELECT * FROM repos WHERE name = ?", (name,))
        row = cursor.fetchone()
        conn.close()
        
        if row:
            return self._row_to_repo_info(row)
        return None
    
    def list_repos(self) -> List[RepoInfo]:
        """List all tracked repositories."""
        conn = self._get_conn()
        cursor = conn.cursor()
        
        cursor.execute("SELECT * FROM repos ORDER BY created_at DESC")
        rows = cursor.fetchall()
        conn.close()
        
        return [self._row_to_repo_info(row) for row in rows]
    
    def _row_to_repo_info(self, row: sqlite3.Row) -> RepoInfo:
        """Convert a database row to RepoInfo."""
        return RepoInfo(
            id=row["id"],
            name=row["name"],
            url=row["url"],
            local_path=row["local_path"],
            index_status=row["index_status"],
            chunk_count=row["chunk_count"],
            last_indexed=datetime.fromisoformat(row["last_indexed"]) if row["last_indexed"] else None,
            created_at=datetime.fromisoformat(row["created_at"]),
        )
    
    def build_index(self, repo_id: int) -> FAISSStore:
        """
        Build a FAISS index for a repository.
        
        Args:
            repo_id: Repository ID
            
        Returns:
            The FAISSStore with the built index
        """
        repo = self.get_repo(repo_id)
        if not repo:
            raise ValueError(f"Repo {repo_id} not found")
        
        # Update status
        self._update_status(repo_id, "indexing")
        
        try:
            # Pull latest changes
            logger.info(f"Pulling latest changes for {repo.name}")
            subprocess.run(
                ["git", "pull"],
                cwd=repo.local_path,
                capture_output=True
            )
            
            # Chunk the codebase
            logger.info(f"Chunking codebase for {repo.name}")
            chunks = chunk_codebase(repo.local_path)
            
            # Build FAISS index
            logger.info(f"Building FAISS index for {repo.name}")
            store = FAISSStore(repo.index_path)
            store.build_index(chunks)
            store.save()
            
            # Update database
            conn = self._get_conn()
            cursor = conn.cursor()
            cursor.execute(
                "UPDATE repos SET index_status = ?, chunk_count = ?, last_indexed = ? WHERE id = ?",
                ("indexed", len(chunks), datetime.now().isoformat(), repo_id)
            )
            conn.commit()
            conn.close()
            
            logger.info(f"Index built successfully for {repo.name}: {len(chunks)} chunks")
            return store
            
        except Exception as e:
            logger.error(f"Error building index: {e}")
            self._update_status(repo_id, "error")
            raise
    
    def get_index(self, repo_id: int) -> Optional[FAISSStore]:
        """
        Get the FAISS index for a repository.
        
        Returns None if index doesn't exist or isn't built.
        """
        repo = self.get_repo(repo_id)
        if not repo or repo.index_status != "indexed":
            return None
        
        store = FAISSStore(repo.index_path)
        if store.load():
            return store
        return None
    
    def delete_repo(self, repo_id: int, delete_files: bool = True) -> None:
        """Delete a repository and optionally its files."""
        repo = self.get_repo(repo_id)
        if not repo:
            return
        
        if delete_files:
            # Delete cloned repo
            if Path(repo.local_path).exists():
                shutil.rmtree(repo.local_path)
            
            # Delete index
            if Path(repo.index_path).exists():
                shutil.rmtree(repo.index_path)
        
        # Remove from database
        conn = self._get_conn()
        cursor = conn.cursor()
        cursor.execute("DELETE FROM repos WHERE id = ?", (repo_id,))
        conn.commit()
        conn.close()
    
    def _update_status(self, repo_id: int, status: str) -> None:
        """Update repository index status."""
        conn = self._get_conn()
        cursor = conn.cursor()
        cursor.execute(
            "UPDATE repos SET index_status = ? WHERE id = ?",
            (status, repo_id)
        )
        conn.commit()
        conn.close()
    
    def search(self, repo_id: int, query: str, top_k: int = 10) -> List[Dict[str, Any]]:
        """
        Search a repository's index.
        
        Args:
            repo_id: Repository ID
            query: Search query
            top_k: Number of results
            
        Returns:
            List of search results with chunk info and scores
        """
        store = self.get_index(repo_id)
        if not store:
            return []
        
        results = store.search(query, top_k)
        
        return [
            {
                "file_path": chunk.file_path,
                "start_line": chunk.start_line,
                "end_line": chunk.end_line,
                "content": chunk.content,
                "language": chunk.language,
                "score": score,
            }
            for chunk, score in results
        ]
