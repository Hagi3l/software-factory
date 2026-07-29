package main

import "testing"

// parseBrokerEndpoint splits on the FIRST colon only so a vsock "<cid>:<port>" address and
// a unix path both survive intact, and rejects unknown transports and malformed forms.
func TestParseBrokerEndpoint(t *testing.T) {
	ok := []struct{ in, net, addr string }{
		{"unix:/run/harness/broker.sock", "unix", "/run/harness/broker.sock"},
		{"vsock:2:1024", "vsock", "2:1024"},
	}
	for _, tc := range ok {
		net, addr, err := parseBrokerEndpoint(tc.in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.in, err)
			continue
		}
		if net != tc.net || addr != tc.addr {
			t.Errorf("%q -> (%q, %q), want (%q, %q)", tc.in, net, addr, tc.net, tc.addr)
		}
	}
	for _, bad := range []string{"", "unix", ":/path", "unix:", "tcp:host:1", "http://x"} {
		if _, _, err := parseBrokerEndpoint(bad); err == nil {
			t.Errorf("%q: want error, got nil", bad)
		}
	}
}
