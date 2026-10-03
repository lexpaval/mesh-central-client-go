package cmd

import "testing"

func TestParseBindAddress(t *testing.T) {
	for _, tc := range []struct {
		spec, bind, target string
		local, remote      int
	}{
		{"80", "", "", 0, 80},
		{"8080:80", "", "", 8080, 80},
		{"0:80", "", "", 0, 80},
		{"192.168.1.1:80", "", "192.168.1.1", 0, 80},
		{"host.example:80", "", "host.example", 0, 80},
		{"8080:192.168.1.1:80", "", "192.168.1.1", 8080, 80},
		{"8080:127.0.0.1:80", "", "", 8080, 80},
		{"8080::80", "", "", 8080, 80},
		{"0.0.0.0:8080:127.0.0.1:80", "0.0.0.0", "", 8080, 80},
		{"localhost:8080:host.example:80", "localhost", "host.example", 8080, 80},
		{"[::1]:8080:127.0.0.1:80", "::1", "", 8080, 80},
		{"[::]:8080:[::1]:80", "::", "::1", 8080, 80},
		{"[::1]:0:[2001:db8::10]:80", "::1", "2001:db8::10", 0, 80},
		{"8080:[2001:db8::10]:80", "", "2001:db8::10", 8080, 80},
		{"[2001:db8::10]:80", "", "2001:db8::10", 0, 80},
		{"[::1]:22", "", "::1", 0, 22},
		{"[fe80::1%eth0]:8080:[fe80::2%eth1]:80", "fe80::1%eth0", "fe80::2%eth1", 8080, 80},
		{"8080:[::ffff:192.0.2.1]:80", "", "::ffff:192.0.2.1", 8080, 80},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			bind, local, target, remote, err := parseBindAddress(tc.spec)
			if err != nil || bind != tc.bind || local != tc.local || target != tc.target || remote != tc.remote {
				t.Fatalf("got (%q, %d, %q, %d), %v; want (%q, %d, %q, %d)", bind, local, target, remote, err, tc.bind, tc.local, tc.target, tc.remote)
			}
		})
	}
}

func TestParseBindAddressRejectsMalformed(t *testing.T) {
	for _, spec := range []string{
		"", "abc", "8080:", ":8080:host:80", "a:b:c:d:e",
		"[::1", "[::1]", "[]:80", "[[::1]]:80", "[::1]]:80",
		"[::1]extra:80", "prefix[::1]:80", "::1:80",
		"[not-an-ip]:80", "[127.0.0.1]:80", "[2001:db8::xyz]:80",
		"8080:[::1:80", "8080:::1:80", "8080:[::1]:http",
		"[::1]:8080:[::1]:80:extra", "[::1]:[::2]:[::3]:80",
	} {
		t.Run(spec, func(t *testing.T) {
			if _, _, _, _, err := parseBindAddress(spec); err == nil {
				t.Fatal("accepted malformed route specification")
			}
		})
	}
}
