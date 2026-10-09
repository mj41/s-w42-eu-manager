package manager

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// The Wi-Fi this computer uses, for the setup page on this computer (localSetup): the robot
// usually belongs in the same network. The password only where the system gives it without
// asking (NetworkManager on Linux, netsh on Windows); on macOS only the name.
type hostWifi struct {
	SSID     string `json:"ssid"`
	Password string `json:"password,omitempty"`
}

func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func currentWifi() hostWifi {
	switch runtime.GOOS {
	case "linux":
		out, err := run("nmcli", "-t", "-f", "NAME,TYPE,DEVICE", "connection", "show", "--active")
		if err != nil {
			return hostWifi{}
		}
		name := nmcliActiveWifi(out)
		if name == "" {
			return hostWifi{}
		}
		w := hostWifi{SSID: name}
		if ssid, err := run("nmcli", "-g", "802-11-wireless.ssid", "connection", "show", "id", name); err == nil && strings.TrimSpace(ssid) != "" {
			w.SSID = strings.TrimSpace(ssid)
		}
		if psk, err := run("nmcli", "-s", "-g", "802-11-wireless-security.psk", "connection", "show", "id", name); err == nil {
			w.Password = strings.TrimRight(psk, "\r\n")
		}
		return w
	case "windows":
		out, err := run("netsh", "wlan", "show", "interfaces")
		if err != nil {
			return hostWifi{}
		}
		w := hostWifi{SSID: netshField(out, "SSID")}
		if profile := netshField(out, "Profile"); profile != "" {
			if p, err := run("netsh", "wlan", "show", "profile", "name="+profile, "key=clear"); err == nil {
				w.Password = netshField(p, "Key Content")
			}
		}
		return w
	case "darwin":
		out, err := run("networksetup", "-getairportnetwork", "en0")
		if err != nil {
			return hostWifi{}
		}
		if _, ssid, ok := strings.Cut(strings.TrimSpace(out), "Current Wi-Fi Network: "); ok {
			return hostWifi{SSID: ssid}
		}
	}
	return hostWifi{}
}

// nmcliActiveWifi: the first active Wi-Fi connection in `nmcli -t -f NAME,TYPE,DEVICE`
// output (":" separates fields, "\:" is a colon in a name).
func nmcliActiveWifi(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := splitTerse(line)
		if len(fields) >= 2 && fields[1] == "802-11-wireless" {
			return fields[0]
		}
	}
	return ""
}

func splitTerse(line string) []string {
	var fields []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case line[i] == ':':
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(fields, cur.String())
}

// netshField: the value of "    Name   : value" in netsh output (not "BSSID" for "SSID").
func netshField(out, name string) string {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*:\s*(.*?)\s*$`)
	if m := re.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}
