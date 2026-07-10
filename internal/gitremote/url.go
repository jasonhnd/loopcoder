// Package gitremote normalizes Git remote URLs without retaining credentials.
package gitremote

import (
	"net"
	"net/url"
	"strings"
	"unicode"
)

// SanitizeDisplayURL returns a stable display URL that never includes URL
// userinfo, query parameters, or fragments. Ambiguous remotes fail closed.
func SanitizeDisplayURL(raw string) (string, bool) {
	normalized, _, _, ok := NormalizeURL(raw)
	return normalized, ok
}

// NormalizeURL returns loopcoder's credential-free remote identity URL.
func NormalizeURL(raw string) (normalized string, githubOwner string, githubName string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || containsControl(raw) {
		return "", "", "", false
	}
	if converted, ok := convertSCPStyle(raw); ok {
		raw = converted
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", "", "", false
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "git", "http", "https", "ssh":
	default:
		return "", "", "", false
	}
	host := strings.ToLower(u.Hostname())
	if !validHost(host) {
		return "", "", "", false
	}
	port := u.Port()
	if port != "" && !isDefaultPort(scheme, port) {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	remotePath, ok := cleanURLPath(u.EscapedPath())
	if !ok || remotePath == "" {
		return "", "", "", false
	}
	normalized = scheme + "://" + host + "/" + remotePath
	if strings.EqualFold(u.Hostname(), "github.com") {
		parts := strings.Split(remotePath, "/")
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			githubOwner = strings.ToLower(parts[0])
			githubName = strings.ToLower(parts[1])
		}
	}
	return normalized, githubOwner, githubName, true
}

func convertSCPStyle(raw string) (string, bool) {
	if strings.Contains(raw, "://") {
		return "", false
	}
	hostPart, remotePath, ok := strings.Cut(raw, ":")
	if !ok || strings.TrimSpace(hostPart) == "" || strings.TrimSpace(remotePath) == "" {
		return "", false
	}
	if strings.ContainsAny(hostPart, `/\`) || strings.Contains(hostPart, "[") || strings.Contains(hostPart, "]") {
		return "", false
	}
	if _, host, ok := strings.Cut(hostPart, "@"); ok {
		hostPart = host
	}
	if !validHost(hostPart) {
		return "", false
	}
	return "ssh://" + hostPart + "/" + remotePath, true
}

func cleanURLPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	path, err := url.PathUnescape(path)
	if err != nil || containsControl(path) {
		return "", false
	}
	path = strings.ReplaceAll(path, `\`, "/")
	parts := make([]string, 0)
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." || strings.ContainsAny(part, "\r\n\t") {
			return "", false
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "", false
	}
	last := parts[len(parts)-1]
	if strings.HasSuffix(strings.ToLower(last), ".git") {
		parts[len(parts)-1] = last[:len(last)-4]
	}
	return strings.Join(parts, "/"), true
}

func validHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || containsControl(host) || strings.ContainsAny(host, " /\\@") {
		return false
	}
	return true
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func isDefaultPort(scheme, port string) bool {
	switch strings.ToLower(scheme) {
	case "http":
		return port == "80"
	case "https":
		return port == "443"
	case "ssh":
		return port == "22"
	default:
		return false
	}
}
