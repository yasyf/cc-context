from python.auth import SessionTokenValidator


def test_validate_bearer_token_rejects_expired_claims() -> None:
    validator = SessionTokenValidator(expected_audience="ccx")
    claims = {"audience": "ccx", "expires_at": 100}
    assert not validator.validate_bearer_token(claims, now=200)
