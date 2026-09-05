"""Official dependency-free Python SDK for AgentsPool."""

from .client import (
    APIError,
    AgentsPoolClient,
    AgentsPoolError,
    TaskTerminalError,
    TransportError,
)

__all__ = [
    "APIError",
    "AgentsPoolClient",
    "AgentsPoolError",
    "TaskTerminalError",
    "TransportError",
]
__version__ = "0.4.0a4"
