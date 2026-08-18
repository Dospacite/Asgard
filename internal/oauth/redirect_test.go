package oauth

import "testing"

func TestRedirectRegistered(t *testing.T) {
	registered := []string{"http://localhost/callback", "http://127.0.0.1/callback", "https://app.example.com/cb"}
	cases := []struct {
		name      string
		requested string
		want      bool
	}{
		{"exact loopback", "http://localhost/callback", true},
		{"loopback with ephemeral port", "http://localhost:3118/callback", true},
		{"loopback ipv4 with port", "http://127.0.0.1:51234/callback", true},
		{"exact https", "https://app.example.com/cb", true},
		{"https port is not ignored", "https://app.example.com:8443/cb", false},
		{"different loopback path", "http://localhost:3118/steal", false},
		{"different loopback host", "http://[::1]:3118/callback", false},
		{"query must match", "http://localhost:3118/callback?next=evil", false},
		{"remote host is not loopback", "http://evil.example.com/callback", false},
		{"loopback prefix in hostname", "http://localhost.evil.com:3118/callback", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := redirectRegistered(registered, testCase.requested); got != testCase.want {
				t.Fatalf("redirectRegistered(%q) = %v, want %v", testCase.requested, got, testCase.want)
			}
		})
	}
}
