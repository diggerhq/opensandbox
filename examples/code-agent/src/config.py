"""Configuration management for the coding agent."""

from pydantic_settings import BaseSettings
from pydantic import Field
from functools import lru_cache


class Settings(BaseSettings):
    """Application settings loaded from environment variables."""
    
    # API Keys
    openai_api_key: str = Field(default="", alias="OPENAI_API_KEY")
    anthropic_api_key: str = Field(default="", alias="ANTHROPIC_API_KEY")
    e2b_api_key: str = Field(default="", alias="E2B_API_KEY")
    
    # LLM Configuration
    default_llm_provider: str = Field(default="anthropic", alias="DEFAULT_LLM_PROVIDER")
    anthropic_model: str = Field(default="claude-sonnet-4-5-20250929", alias="ANTHROPIC_MODEL")
    openai_model: str = Field(default="gpt-5.2", alias="OPENAI_MODEL")
    
    # Sandbox Configuration
    sandbox_provider: str = Field(default="opensandbox", alias="SANDBOX_PROVIDER")
    sandbox_timeout: int = Field(default=1800, alias="SANDBOX_TIMEOUT")  # 30 minutes
    opensandbox_url: str = Field(default="http://localhost:8080", alias="OPENSANDBOX_URL")
    
    # Agent Configuration
    max_iterations: int = Field(default=50, alias="MAX_ITERATIONS")  # Max LLM tool-calling iterations
    
    # Server Configuration
    api_host: str = Field(default="0.0.0.0", alias="API_HOST")
    api_port: int = Field(default=8000, alias="API_PORT")
    
    class Config:
        env_file = ".env"
        env_file_encoding = "utf-8"
        extra = "ignore"


@lru_cache
def get_settings() -> Settings:
    """Get cached settings instance."""
    return Settings()
