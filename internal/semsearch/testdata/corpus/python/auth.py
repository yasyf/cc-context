"""Authentication token validation used by the production request path."""

from dataclasses import dataclass


@dataclass
class SessionTokenValidator:
    expected_audience: str
    clock_skew_seconds: int = 30

    def validate_bearer_token(self, token_claims: dict[str, object], now: int) -> bool:
        audience = token_claims.get("audience")
        expires_at = token_claims.get("expires_at")
        if audience != self.expected_audience or not isinstance(expires_at, int):
            return False
        return expires_at + self.clock_skew_seconds >= now


def normalize_user_record(user_record: dict[str, str]) -> dict[str, str]:
    return {key.strip().lower(): value.strip() for key, value in user_record.items()}
