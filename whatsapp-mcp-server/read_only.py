"""Startup-only access policy shared by the MCP server and its Python helpers."""

import os


def parse_read_only(value: str | None) -> bool:
    """Keep the existing default; reject invalid settings instead of enabling writes."""
    if value is None:
        return False
    normalized = value.strip().lower()
    if normalized in {"true", "1", "yes", "on"}:
        return True
    if normalized in {"false", "0", "no", "off"}:
        return False
    raise ValueError(
        "invalid WHATSAPP_READ_ONLY: expected true/1/yes/on or false/0/no/off"
    )


READ_ONLY = parse_read_only(os.environ.get("WHATSAPP_READ_ONLY"))
READ_ONLY_MESSAGE = "WhatsApp writes are disabled (WHATSAPP_READ_ONLY=true)"
