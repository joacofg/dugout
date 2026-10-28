package wire

import "testing"

func TestInsecureServer(t *testing.T) {
	for url, want := range map[string]bool{
		"http://dugout.example.com":      true,
		"http://dugout.example.com:7457": true,
		"HTTP://10.0.0.5:7457":           true,
		"https://dugout.example.com":     false,
		"http://127.0.0.1:7457":          false,
		"http://127.1.2.3":               false,
		"http://localhost:7457":          false,
		"http://[::1]:7457":              false,
		"off":                            false,
		"":                               false,
		"::bad url::":                    false,
	} {
		if got := InsecureServer(url); got != want {
			t.Errorf("InsecureServer(%q) = %v, want %v", url, got, want)
		}
	}
}

// TestIsLoopbackURL: LOCALHOST counts as loopback, like InsecureServer already
// treated it.
func TestIsLoopbackURL(t *testing.T) {
	for url, want := range map[string]bool{
		"http://localhost":              true,
		"https://localhost":             true,
		"http://LOCALHOST:7457":         true,
		" http://localhost:7457 ":       true,
		"http://127.0.0.1:7457":         true,
		"http://127.0.0.2":              true,
		"http://[::1]:7457":             true,
		"https://[::1]":                 true,
		"http://127.0.0.1.nip.io":       false,
		"http://localhost@evil.example": false,
		"127.0.0.1:7457":                false,
		"localhost:7457":                false,
		"http://0.0.0.0:7457":           false,
		"http://dugout.example.com":     false,
		"off":                           false,
		"":                              false,
	} {
		if got := IsLoopbackURL(url); got != want {
			t.Errorf("IsLoopbackURL(%q) = %v, want %v", url, got, want)
		}
	}
}
