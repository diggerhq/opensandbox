"""Code chunking for indexing."""

import os
import logging
from typing import List, Dict, Any
from pathlib import Path
from dataclasses import dataclass

logger = logging.getLogger(__name__)

# File extensions to index
CODE_EXTENSIONS = {
    ".py", ".js", ".ts", ".tsx", ".jsx", ".go", ".rs", ".java", ".c", ".cpp", 
    ".h", ".hpp", ".cs", ".rb", ".php", ".swift", ".kt", ".scala", ".sh",
    ".yaml", ".yml", ".json", ".toml", ".md", ".txt", ".sql", ".graphql"
}

# Files/directories to skip
SKIP_PATTERNS = {
    "node_modules", "vendor", "dist", "build", ".git", "__pycache__",
    ".venv", "venv", "env", ".env", ".idea", ".vscode", "target",
    "coverage", ".next", ".nuxt", "*.min.js", "*.min.css",
    "package-lock.json", "yarn.lock", "Cargo.lock", "go.sum"
}

# Target chunk size in characters (roughly 500-800 tokens)
TARGET_CHUNK_SIZE = 2000
CHUNK_OVERLAP = 200


@dataclass
class CodeChunk:
    """A chunk of code with metadata."""
    content: str
    file_path: str
    start_line: int
    end_line: int
    language: str
    
    def to_dict(self) -> Dict[str, Any]:
        return {
            "content": self.content,
            "file_path": self.file_path,
            "start_line": self.start_line,
            "end_line": self.end_line,
            "language": self.language,
        }
    
    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "CodeChunk":
        return cls(**data)


def should_skip(path: Path) -> bool:
    """Check if a path should be skipped."""
    name = path.name
    
    for pattern in SKIP_PATTERNS:
        if pattern.startswith("*"):
            if name.endswith(pattern[1:]):
                return True
        elif name == pattern or pattern in str(path):
            return True
    
    return False


def get_language(file_path: str) -> str:
    """Get language from file extension."""
    ext = Path(file_path).suffix.lower()
    
    lang_map = {
        ".py": "python",
        ".js": "javascript",
        ".ts": "typescript",
        ".tsx": "typescript",
        ".jsx": "javascript",
        ".go": "go",
        ".rs": "rust",
        ".java": "java",
        ".c": "c",
        ".cpp": "cpp",
        ".h": "c",
        ".hpp": "cpp",
        ".cs": "csharp",
        ".rb": "ruby",
        ".php": "php",
        ".swift": "swift",
        ".kt": "kotlin",
        ".scala": "scala",
        ".sh": "bash",
        ".yaml": "yaml",
        ".yml": "yaml",
        ".json": "json",
        ".toml": "toml",
        ".md": "markdown",
        ".sql": "sql",
    }
    
    return lang_map.get(ext, "text")


def chunk_file(file_path: str, content: str) -> List[CodeChunk]:
    """
    Chunk a single file into smaller pieces.
    
    Uses a simple line-based chunking strategy that tries to break
    at logical boundaries (blank lines, function definitions).
    """
    lines = content.split("\n")
    chunks = []
    language = get_language(file_path)
    
    current_chunk_lines = []
    current_start_line = 1
    current_size = 0
    
    for i, line in enumerate(lines, 1):
        line_size = len(line) + 1  # +1 for newline
        
        # Check if adding this line would exceed target size
        if current_size + line_size > TARGET_CHUNK_SIZE and current_chunk_lines:
            # Save current chunk
            chunk_content = "\n".join(current_chunk_lines)
            chunks.append(CodeChunk(
                content=chunk_content,
                file_path=file_path,
                start_line=current_start_line,
                end_line=i - 1,
                language=language,
            ))
            
            # Start new chunk with overlap
            overlap_lines = current_chunk_lines[-3:] if len(current_chunk_lines) > 3 else []
            current_chunk_lines = overlap_lines + [line]
            current_start_line = max(1, i - len(overlap_lines))
            current_size = sum(len(l) + 1 for l in current_chunk_lines)
        else:
            current_chunk_lines.append(line)
            current_size += line_size
    
    # Don't forget the last chunk
    if current_chunk_lines:
        chunk_content = "\n".join(current_chunk_lines)
        chunks.append(CodeChunk(
            content=chunk_content,
            file_path=file_path,
            start_line=current_start_line,
            end_line=len(lines),
            language=language,
        ))
    
    return chunks


def chunk_codebase(repo_path: str) -> List[CodeChunk]:
    """
    Chunk all files in a codebase.
    
    Args:
        repo_path: Path to the repository root
        
    Returns:
        List of CodeChunk objects
    """
    repo_path = Path(repo_path)
    all_chunks = []
    files_processed = 0
    
    logger.info(f"Chunking codebase at {repo_path}")
    
    for root, dirs, files in os.walk(repo_path):
        root_path = Path(root)
        
        # Skip directories
        dirs[:] = [d for d in dirs if not should_skip(root_path / d)]
        
        for file in files:
            file_path = root_path / file
            
            # Skip non-code files
            if file_path.suffix.lower() not in CODE_EXTENSIONS:
                continue
            
            if should_skip(file_path):
                continue
            
            try:
                content = file_path.read_text(encoding="utf-8", errors="ignore")
                
                # Skip very large files
                if len(content) > 500000:  # 500KB
                    logger.warning(f"Skipping large file: {file_path}")
                    continue
                
                # Get relative path for storage
                relative_path = str(file_path.relative_to(repo_path))
                
                chunks = chunk_file(relative_path, content)
                all_chunks.extend(chunks)
                files_processed += 1
                
            except Exception as e:
                logger.warning(f"Error processing {file_path}: {e}")
    
    logger.info(f"Processed {files_processed} files, created {len(all_chunks)} chunks")
    return all_chunks
