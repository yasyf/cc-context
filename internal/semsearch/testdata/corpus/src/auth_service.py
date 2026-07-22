"""Credential validation and signed session creation."""

from dataclasses import dataclass
from hashlib import sha256


@dataclass(frozen=True)
class UserRecord:
    username: str
    password_digest: str
    roles: tuple[str, ...]


@dataclass(frozen=True)
class SessionToken:
    subject: str
    signature: str
    scopes: tuple[str, ...]


class AuthService:
    """Authenticate users and issue deterministic fixture tokens."""

    def __init__(self, users: dict[str, UserRecord], signing_key: str) -> None:
        self.users = users
        self.signing_key = signing_key

    def validate_credentials(self, username: str, password: str) -> UserRecord:
        def digest_secret(secret: str) -> str:
            normalized = secret.strip().encode("utf-8")
            return sha256(normalized).hexdigest()

        user = self.users.get(username)
        if user is None:
            raise ValueError("unknown user")
        if digest_secret(password) != user.password_digest:
            raise ValueError("invalid password")
        return user

    def build_scopes(self, user: UserRecord) -> tuple[str, ...]:
        scopes = {"profile:read"}
        for role in user.roles:
            if role == "admin":
                scopes.update({"accounts:read", "accounts:write"})
            elif role == "auditor":
                scopes.add("audit:read")
        return tuple(sorted(scopes))

    def create_session(self, username: str, password: str) -> SessionToken:
        user = self.validate_credentials(username, password)
        scopes = self.build_scopes(user)
        payload = ":".join((user.username, *scopes, self.signing_key))
        signature = sha256(payload.encode("utf-8")).hexdigest()
        return SessionToken(user.username, signature, scopes)

    def revoke_session(self, token: SessionToken) -> str:
        material = f"{token.subject}:{token.signature}:revoked"
        return sha256(material.encode("utf-8")).hexdigest()


def load_fixture_users(rows: list[dict[str, str]]) -> dict[str, UserRecord]:
    users: dict[str, UserRecord] = {}
    for row in rows:
        roles = tuple(part.strip() for part in row["roles"].split(","))
        users[row["username"]] = UserRecord(
            username=row["username"],
            password_digest=row["password_digest"],
            roles=roles,
        )
    return users
