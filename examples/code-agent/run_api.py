#!/usr/bin/env python3
"""Entry point to run the API server."""

import uvicorn
from src.config import get_settings


def main():
    settings = get_settings()
    uvicorn.run(
        "src.api.server:app",
        host=settings.api_host,
        port=settings.api_port,
        reload=True,
    )


if __name__ == "__main__":
    main()
