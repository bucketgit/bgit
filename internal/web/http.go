package web

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

func NewCSRFToken() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func IsMutationRoute(route, method string) bool {
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return false
	}
	return strings.HasPrefix(route, "api/actions/") || route == "api/user/profile"
}

// ValidateLocalMutation verifies the per-process CSRF token and rejects
// browser mutations originating outside the loopback web application.
func ValidateLocalMutation(token string, request *http.Request) error {
	if request.Header.Get("X-Bgit-CSRF") != token {
		return errors.New("invalid CSRF token")
	}
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return errors.New("invalid origin")
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		host = parsed.Hostname()
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if parsed.Scheme != "http" || (host != "localhost" && host != "127.0.0.1" && host != "::1") {
		return errors.New("foreign origin rejected")
	}
	return nil
}

func ServeAsset(response http.ResponseWriter, path string) {
	data, err := ReadAsset(path)
	if err != nil {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	if contentType := mime.TypeByExtension(filepath.Ext(path)); contentType != "" {
		response.Header().Set("Content-Type", contentType)
	}
	response.Header().Set("Cache-Control", "public, max-age=86400")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(data)
}
