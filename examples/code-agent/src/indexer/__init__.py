"""Codebase indexer for semantic search."""

from .embeddings import get_embeddings_client, embed_texts, embed_single
from .chunker import chunk_file, chunk_codebase
from .faiss_store import FAISSStore
from .repo_manager import RepoManager

__all__ = [
    "get_embeddings_client",
    "embed_texts",
    "embed_single",
    "chunk_file",
    "chunk_codebase",
    "FAISSStore",
    "RepoManager",
]
