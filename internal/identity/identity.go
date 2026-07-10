package identity

import (
	"regexp"
	"strings"
)

const DefaultName = "BucketGit Client"

var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

type Value struct {
	Name, Email string
	UsesDefault bool
}

func DefaultEmail(username string) string {
	username = strings.ToLower(strings.TrimSpace(username))
	var clean strings.Builder
	for _, char := range username {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-' {
			clean.WriteRune(char)
		}
	}
	if clean.Len() == 0 {
		return "username@bucketgit.com"
	}
	return clean.String() + "@bucketgit.com"
}

func Effective(name, email, defaultEmail string) Value {
	if strings.TrimSpace(name) == "" {
		name = DefaultName
	}
	if strings.TrimSpace(email) == "" {
		email = defaultEmail
	}
	return Value{Name: strings.TrimSpace(name), Email: strings.TrimSpace(email), UsesDefault: name == DefaultName || email == defaultEmail}
}

func ValidEmail(value string) bool { return emailPattern.MatchString(strings.TrimSpace(value)) }
