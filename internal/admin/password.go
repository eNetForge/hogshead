package admin

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// MinPasswordLength applies to web UI passwords.
const MinPasswordLength = 8

// HashPassword returns the bcrypt hash stored for a user's web UI password.
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", errors.New("password must be at least 8 characters")
	}
	if len(password) > 1024 {
		return "", errors.New("password is too long")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return "", err
	}
	return string(h), nil
}
