"""Sandbox abstraction layer."""

from .base import BaseSandbox
from .e2b import E2BSandbox
from .opensandbox import OpenSandbox


def get_sandbox(provider: str = "opensandbox", **kwargs) -> BaseSandbox:
    """
    Factory function to get a sandbox instance.
    
    Args:
        provider: The sandbox provider to use ("e2b" or "opensandbox")
        **kwargs: Additional arguments passed to the sandbox constructor
        
    Returns:
        A sandbox instance implementing BaseSandbox
        
    Raises:
        ValueError: If provider is not supported
    """
    if provider == "e2b":
        return E2BSandbox()
    elif provider == "opensandbox":
        base_url = kwargs.get("base_url", "http://localhost:8080")
        return OpenSandbox(base_url=base_url)
    raise ValueError(f"Unknown sandbox provider: {provider}. Supported: ['e2b', 'opensandbox']")


__all__ = ["BaseSandbox", "E2BSandbox", "OpenSandbox", "get_sandbox"]
