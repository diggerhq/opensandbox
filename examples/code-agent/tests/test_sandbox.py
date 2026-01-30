"""Tests for sandbox functionality."""

import pytest
from src.sandbox import get_sandbox, BaseSandbox, E2BSandbox


def test_get_sandbox_e2b():
    """Test that get_sandbox returns E2BSandbox for 'e2b' provider."""
    sandbox = get_sandbox("e2b")
    assert isinstance(sandbox, E2BSandbox)
    assert isinstance(sandbox, BaseSandbox)


def test_get_sandbox_unknown_provider():
    """Test that get_sandbox raises for unknown provider."""
    with pytest.raises(ValueError, match="Unknown sandbox provider"):
        get_sandbox("unknown")


def test_sandbox_not_active_initially():
    """Test that sandbox is not active before create() is called."""
    sandbox = E2BSandbox()
    assert not sandbox.is_active
    assert sandbox.sandbox_id is None


# Integration tests (require E2B API key)
@pytest.mark.integration
@pytest.mark.asyncio
async def test_sandbox_create_and_destroy():
    """Test creating and destroying a sandbox."""
    sandbox = E2BSandbox()
    
    try:
        sandbox_id = await sandbox.create(timeout=300)
        assert sandbox_id is not None
        assert sandbox.is_active
        
        # Test running a command
        result = await sandbox.run_command("echo 'hello world'")
        assert result.exit_code == 0
        assert "hello world" in result.stdout
        
    finally:
        await sandbox.destroy()
        assert not sandbox.is_active


@pytest.mark.integration
@pytest.mark.asyncio
async def test_sandbox_file_operations():
    """Test file read/write in sandbox."""
    sandbox = E2BSandbox()
    
    try:
        await sandbox.create(timeout=300)
        
        # Write a file
        test_content = "Hello, World!"
        await sandbox.write_file("/tmp/test.txt", test_content)
        
        # Read it back
        content = await sandbox.read_file("/tmp/test.txt")
        assert content == test_content
        
    finally:
        await sandbox.destroy()
