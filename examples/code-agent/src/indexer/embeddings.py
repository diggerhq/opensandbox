"""Embeddings generation using OpenAI."""

import logging
from typing import List
from openai import OpenAI

from src.config import get_settings

logger = logging.getLogger(__name__)

# Embedding model - good balance of quality and cost
EMBEDDING_MODEL = "text-embedding-3-small"
EMBEDDING_DIMENSIONS = 1536


def get_embeddings_client() -> OpenAI:
    """Get OpenAI client for embeddings."""
    settings = get_settings()
    return OpenAI(api_key=settings.openai_api_key)


def embed_texts(texts: List[str], batch_size: int = 100) -> List[List[float]]:
    """
    Generate embeddings for a list of texts.
    
    Args:
        texts: List of text strings to embed
        batch_size: Number of texts to embed per API call
        
    Returns:
        List of embedding vectors
    """
    client = get_embeddings_client()
    all_embeddings = []
    
    # Process in batches
    for i in range(0, len(texts), batch_size):
        batch = texts[i:i + batch_size]
        logger.debug(f"Embedding batch {i // batch_size + 1}, size {len(batch)}")
        
        response = client.embeddings.create(
            model=EMBEDDING_MODEL,
            input=batch
        )
        
        # Extract embeddings in order
        batch_embeddings = [item.embedding for item in response.data]
        all_embeddings.extend(batch_embeddings)
    
    return all_embeddings


def embed_single(text: str) -> List[float]:
    """
    Generate embedding for a single text.
    
    Args:
        text: Text string to embed
        
    Returns:
        Embedding vector
    """
    client = get_embeddings_client()
    
    response = client.embeddings.create(
        model=EMBEDDING_MODEL,
        input=text
    )
    
    return response.data[0].embedding
