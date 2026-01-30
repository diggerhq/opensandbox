"""FAISS vector store for code search."""

import json
import logging
import numpy as np
from pathlib import Path
from typing import List, Dict, Any, Optional, Tuple

import faiss

from .chunker import CodeChunk
from .embeddings import embed_texts, embed_single, EMBEDDING_DIMENSIONS

logger = logging.getLogger(__name__)


class FAISSStore:
    """
    FAISS-based vector store for code chunks.
    
    Stores embeddings in a FAISS index and metadata in a JSON file.
    """
    
    def __init__(self, index_path: str):
        """
        Initialize the FAISS store.
        
        Args:
            index_path: Directory to store the index files
        """
        self.index_path = Path(index_path)
        self.index_path.mkdir(parents=True, exist_ok=True)
        
        self.faiss_file = self.index_path / "index.faiss"
        self.metadata_file = self.index_path / "metadata.json"
        
        self.index: Optional[faiss.IndexFlatIP] = None
        self.chunks: List[CodeChunk] = []
    
    def build_index(self, chunks: List[CodeChunk], batch_size: int = 100) -> None:
        """
        Build a FAISS index from code chunks.
        
        Args:
            chunks: List of CodeChunk objects to index
            batch_size: Batch size for embedding generation
        """
        if not chunks:
            logger.warning("No chunks to index")
            return
        
        logger.info(f"Building index for {len(chunks)} chunks...")
        
        # Generate embeddings
        texts = [self._chunk_to_text(chunk) for chunk in chunks]
        embeddings = embed_texts(texts, batch_size=batch_size)
        
        # Convert to numpy array
        embeddings_array = np.array(embeddings, dtype=np.float32)
        
        # Normalize for cosine similarity (using inner product index)
        faiss.normalize_L2(embeddings_array)
        
        # Create FAISS index (Inner Product = cosine similarity for normalized vectors)
        self.index = faiss.IndexFlatIP(EMBEDDING_DIMENSIONS)
        self.index.add(embeddings_array)
        
        self.chunks = chunks
        
        logger.info(f"Index built with {self.index.ntotal} vectors")
    
    def _chunk_to_text(self, chunk: CodeChunk) -> str:
        """Convert a chunk to text for embedding."""
        # Include file path in the text for better context
        return f"File: {chunk.file_path}\nLines {chunk.start_line}-{chunk.end_line}\n\n{chunk.content}"
    
    def search(self, query: str, top_k: int = 10) -> List[Tuple[CodeChunk, float]]:
        """
        Search for similar code chunks.
        
        Args:
            query: Search query
            top_k: Number of results to return
            
        Returns:
            List of (chunk, score) tuples
        """
        if self.index is None or self.index.ntotal == 0:
            logger.warning("Index is empty")
            return []
        
        # Embed query
        query_embedding = np.array([embed_single(query)], dtype=np.float32)
        faiss.normalize_L2(query_embedding)
        
        # Search
        scores, indices = self.index.search(query_embedding, min(top_k, self.index.ntotal))
        
        results = []
        for score, idx in zip(scores[0], indices[0]):
            if idx >= 0 and idx < len(self.chunks):
                results.append((self.chunks[idx], float(score)))
        
        return results
    
    def save(self) -> None:
        """Save the index and metadata to disk."""
        if self.index is None:
            logger.warning("No index to save")
            return
        
        # Save FAISS index
        faiss.write_index(self.index, str(self.faiss_file))
        
        # Save metadata (chunks)
        metadata = {
            "chunks": [chunk.to_dict() for chunk in self.chunks],
            "total_chunks": len(self.chunks),
        }
        
        with open(self.metadata_file, "w") as f:
            json.dump(metadata, f)
        
        logger.info(f"Index saved to {self.index_path}")
    
    def load(self) -> bool:
        """
        Load the index and metadata from disk.
        
        Returns:
            True if loaded successfully, False otherwise
        """
        if not self.faiss_file.exists() or not self.metadata_file.exists():
            logger.info("No existing index found")
            return False
        
        try:
            # Load FAISS index
            self.index = faiss.read_index(str(self.faiss_file))
            
            # Load metadata
            with open(self.metadata_file, "r") as f:
                metadata = json.load(f)
            
            self.chunks = [CodeChunk.from_dict(c) for c in metadata["chunks"]]
            
            logger.info(f"Loaded index with {self.index.ntotal} vectors")
            return True
            
        except Exception as e:
            logger.error(f"Error loading index: {e}")
            return False
    
    def get_stats(self) -> Dict[str, Any]:
        """Get index statistics."""
        if self.index is None:
            return {"status": "not_built", "chunks": 0, "vectors": 0}
        
        # Count files
        files = set(chunk.file_path for chunk in self.chunks)
        
        return {
            "status": "ready",
            "chunks": len(self.chunks),
            "vectors": self.index.ntotal,
            "files": len(files),
        }
