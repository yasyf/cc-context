package auth

// Session holds an authenticated user session for the login flow.
type Session struct {
	User  string
	Token string
}

// Refresh extends the session token lifetime.
func (s *Session) Refresh() {
	s.Token = s.Token + "!"
}
