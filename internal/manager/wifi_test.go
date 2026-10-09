package manager

import (
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestIsLocal(t *testing.T) {
	for _, c := range []struct {
		name, remote, host, xff string
		want                    bool
	}{
		{"this computer", "127.0.0.1:5000", "localhost:8790", "", true},
		{"this computer, ipv6", "[::1]:5000", "[::1]:8790", "", true},
		{"another computer", "192.168.1.20:5000", "192.168.1.10:8790", "", false},
		{"loopback, LAN host name", "127.0.0.1:5000", "192.168.1.10:8790", "", false},
		{"DNS rebinding", "127.0.0.1:5000", "evil.example:8790", "", false},
		{"local proxy", "127.0.0.1:5000", "localhost:8790", "203.0.113.5", false},
	} {
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.RemoteAddr, r.Host = c.remote, c.host
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := isLocal(r); got != c.want {
			t.Errorf("%s: isLocal = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWifiParsers(t *testing.T) {
	if got := nmcliActiveWifi("Wired:802-3-ethernet:eth0\nHome\\:5G:802-11-wireless:wlp2s0\n"); got != "Home:5G" {
		t.Errorf("nmcliActiveWifi = %q", got)
	}
	out := "    Name                   : Wi-Fi\n    SSID                   : Home Net\n    BSSID                  : aa:bb\n    Profile                : Home Net\n"
	if got := netshField(out, "SSID"); got != "Home Net" {
		t.Errorf("SSID = %q", got)
	}
	if got := netshField("    Key Content            : secret pass\n", "Key Content"); got != "secret pass" {
		t.Errorf("Key Content = %q", got)
	}
}

func regexpMatch(pattern, s string) bool {
	return regexp.MustCompile(regexp.QuoteMeta(pattern)).MatchString(s)
}
