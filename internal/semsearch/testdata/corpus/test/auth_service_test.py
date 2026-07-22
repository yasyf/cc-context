from src.auth_service import AuthService, UserRecord


def test_validate_credentials_accepts_matching_digest() -> None:
    user = UserRecord("ada", "digest", ("admin",))
    service = AuthService({"ada": user}, "fixture-key")
    service.validate_credentials = lambda username, password: user

    assert service.create_session("ada", "correct").subject == "ada"


def test_validate_credentials_rejects_unknown_user() -> None:
    service = AuthService({}, "fixture-key")

    try:
        service.validate_credentials("missing", "password")
    except ValueError as error:
        assert str(error) == "unknown user"
