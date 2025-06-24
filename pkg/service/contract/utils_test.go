package contract

import (
	"testing"
)

func TestExtractURLPath(t *testing.T) {
	testCases := []struct {
		name         string
		url          string
		expectedPath string
		expectedHost string
	}{
		{
			name:         "Simple URL",
			url:          "https://api.example.com/users",
			expectedPath: "/users",
			expectedHost: "api.example.com",
		},
		{
			name:         "URL with query parameters",
			url:          "https://api.example.com/users?page=1&limit=10",
			expectedPath: "/users",
			expectedHost: "api.example.com",
		},
		{
			name:         "URL with path parameters",
			url:          "https://api.example.com/users/123/profile",
			expectedPath: "/users/123/profile",
			expectedHost: "api.example.com",
		},
		{
			name:         "URL with trailing slash",
			url:          "https://api.example.com/users/",
			expectedPath: "/users/",
			expectedHost: "api.example.com",
		},
		{
			name:         "URL with port",
			url:          "http://localhost:8080/api/v1/users",
			expectedPath: "/api/v1/users",
			expectedHost: "localhost:8080",
		},
		{
			name:         "URL with no path",
			url:          "https://api.example.com",
			expectedPath: "",
			expectedHost: "api.example.com",
		},
		{
			name:         "Invalid URL",
			url:          "://invalid-url",
			expectedPath: "",
			expectedHost: "",
		},
		{
			name:         "Empty URL",
			url:          "",
			expectedPath: "",
			expectedHost: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			path, host := ExtractURLPath(tc.url)

			if path != tc.expectedPath {
				t.Errorf("Expected path %q, got %q", tc.expectedPath, path)
			}

			if host != tc.expectedHost {
				t.Errorf("Expected host %q, got %q", tc.expectedHost, host)
			}
		})
	}
}
