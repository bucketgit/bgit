package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateLocalMutation(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		origin string
		ok     bool
	}{
		{"localhost", "secret", "http://localhost:8042", true},
		{"ipv4", "secret", "http://127.0.0.1:8042", true},
		{"ipv6", "secret", "http://[::1]:8042", true},
		{"foreign", "secret", "https://attacker.example", false},
		{"bad token", "wrong", "http://localhost:8042", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://localhost/api/actions/push", nil)
			request.Header.Set("X-Bgit-CSRF", test.token)
			request.Header.Set("Origin", test.origin)
			err := ValidateLocalMutation("secret", request)
			if (err == nil) != test.ok {
				t.Fatalf("error=%v ok=%t", err, test.ok)
			}
		})
	}
}
