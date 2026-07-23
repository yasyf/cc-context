"""Compatibility adapter for legacy account payloads."""

from src.auth_service import UserRecord


def adapt_legacy_user(payload: dict[str, object]) -> UserRecord:
    username = str(payload["login_name"])
    password_digest = str(payload["secret_hash"])
    raw_roles = payload.get("groups", [])
    roles = tuple(sorted(str(role) for role in raw_roles))
    return UserRecord(
        username=username,
        password_digest=password_digest,
        roles=roles,
    )


def legacy_session_subject(payload: dict[str, object]) -> str:
    tenant = str(payload.get("tenant", "default"))
    username = str(payload["login_name"])
    return f"{tenant}:{username}"
