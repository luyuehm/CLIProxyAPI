package util

import "testing"

func TestIsLocalEndpointURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"local scheme", "local://127.0.0.1:8000", true},
		{"local scheme with vllm", "local://10.0.0.5:8000", true},
		{"local scheme with trailing slash", "local://host:8000/", true},
		{"plain http is not local", "http://api.openai.com/v1", false},
		{"plain https is not local", "https://api.openai.com/v1", false},
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"http localhost is not local scheme", "http://localhost:8000", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsLocalEndpointURL(tt.url); got != tt.want {
				t.Fatalf("IsLocalEndpointURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestNormalizeEndpointBaseURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"local scheme to http", "local://127.0.0.1:8000", "http://127.0.0.1:8000"},
		{"local scheme with path", "local://host:8000/v1", "http://host:8000/v1"},
		{"plain http unchanged", "http://api.openai.com/v1", "http://api.openai.com/v1"},
		{"plain https unchanged", "https://api.openai.com/v1", "https://api.openai.com/v1"},
		{"empty unchanged", "", ""},
		{"whitespace trimmed", "  local://h:8000  ", "http://h:8000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeEndpointBaseURL(tt.url); got != tt.want {
				t.Fatalf("NormalizeEndpointBaseURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}
