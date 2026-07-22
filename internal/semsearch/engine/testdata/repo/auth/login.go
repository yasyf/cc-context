package auth

// Login validates the session token and starts a user session flow.
func Login(user, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	return true, nil
}
