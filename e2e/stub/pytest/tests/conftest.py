"""
Pytest configuration and fixtures for Keploy stub E2E tests.
"""
import os
import pytest
import httpx


@pytest.fixture(scope="session")
def base_url():
    """Get the base URL for API requests."""
    return os.environ.get("API_BASE_URL", "http://localhost:9000")


@pytest.fixture(scope="session")
def client(base_url):
    """Create an HTTP client for making API requests."""
    with httpx.Client(base_url=base_url, timeout=30.0) as client:
        yield client


@pytest.fixture(scope="session")
def async_client(base_url):
    """Create an async HTTP client for making API requests."""
    return httpx.AsyncClient(base_url=base_url, timeout=30.0)
