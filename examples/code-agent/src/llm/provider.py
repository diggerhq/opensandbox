"""LLM provider abstraction."""

from langchain_anthropic import ChatAnthropic
from langchain_openai import ChatOpenAI
from langchain_core.language_models.chat_models import BaseChatModel

from src.config import get_settings


def get_llm(provider: str | None = None, model: str | None = None) -> BaseChatModel:
    """
    Get an LLM instance based on the provider and model.
    
    Args:
        provider: "anthropic" or "openai". If None, uses default from settings.
        model: Specific model to use. If None, uses default for the provider.
        
    Returns:
        A LangChain chat model instance
        
    Raises:
        ValueError: If provider is not supported
    """
    settings = get_settings()
    provider = provider or settings.default_llm_provider
    
    if provider == "anthropic":
        model_name = model or settings.anthropic_model
        return ChatAnthropic(
            model=model_name,
            api_key=settings.anthropic_api_key,
            max_tokens=8192,
        )
    elif provider == "openai":
        model_name = model or settings.openai_model
        return ChatOpenAI(
            model=model_name,
            api_key=settings.openai_api_key,
        )
    else:
        raise ValueError(f"Unknown LLM provider: {provider}. Supported: ['anthropic', 'openai']")
