// Command s-w42-eu-usb sets up a Stackchan over its USB cable: it reads the robot id and
// writes a server, its token, autostart and Wi-Fi into the robot's settings (firmware with
// Embody Mode's USB setup; protocol in its usb_setup.h). The same thing the manager's page
// does in Chrome, for developers, own servers and custom firmware.
//
//	s-w42-eu-usb hello
//	s-w42-eu-usb provision -url wss://raw.sa.w42.eu -name Raw -token-file token.txt -default -autostart
//	s-w42-eu-usb provision -wifi-ssid Home -wifi-password-file wifi.txt
//	s-w42-eu-usb restart
//	s-w42-eu-usb pair                   the pairing link the robot shows (to open in a browser)
//	s-w42-eu-usb status                 what Embody Mode does: its server, the connection, the QR
//	                                    screen, its apps (no tokens), the manager settings
//	s-w42-eu-usb log [-seconds 30] [-grep regexp] [-all]   the robot's log (by default without the
//	                                    periodic noise: memory, NFC polls, backlight)
//	s-w42-eu-usb setup [-manager http://localhost:8790] [-apps raw,pet] [-start raw] [-ask-pin=false] [-second]
//	                                    what the manager's page does with "Write apps": the apps from
//	                                    a home manager (no sign-in) with their tokens and its key
//
// With firmware built with automation, a program can also do what a person at the robot does
// (but never answer the robot's own questions, e.g. a new default server or turning the head):
//
//	s-w42-eu-usb screenshot -o screen.jpg   the screen as a JPEG
//	s-w42-eu-usb tap -x 160 -y 200 [-ms 800]  a tap (or a long press) on the screen
//	s-w42-eu-usb launch -app "Embody Mode"  restart into a launcher app ("launcher": none)
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

const prefix = "@stackchan "

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	op, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(op, flag.ExitOnError)
	port := fs.String("port", "", "serial port (default: the first Espressif USB device)")
	var req map[string]any
	out := ""
	switch op {
	case "screenshot":
		o := fs.String("o", "screen.jpg", "where to save the JPEG")
		top := fs.Bool("top", false, "the top layer: the robot's own questions")
		fs.Parse(args)
		req, out = map[string]any{"op": op}, *o
		if *top {
			req["layer"] = "top"
		}
	case "tap":
		x := fs.Int("x", -1, "x, 0..319 (left to right)")
		y := fs.Int("y", -1, "y, 0..239 (top to bottom)")
		ms := fs.Int("ms", 100, "how long to press, ms (a long press: 800)")
		fs.Parse(args)
		req = map[string]any{"op": op, "x": *x, "y": *y, "ms": *ms}
	case "launch":
		app := fs.String("app", "Embody Mode", `a launcher app's name ("AVATAR", "Embody Mode", ...) or "launcher"`)
		fs.Parse(args)
		req = map[string]any{"op": op, "app": *app}
	case "log":
		secs := fs.Int("seconds", 30, "how long to read (0: until interrupted)")
		grep := fs.String("grep", "", "only lines matching this regular expression")
		all := fs.Bool("all", false, "also the periodic lines (memory, NFC polls, backlight)")
		fs.Parse(args)
		name := *port
		if name == "" {
			var err error
			if name, err = findPort(); err != nil {
				fail("%v", err)
			}
		}
		if err := streamLog(name, time.Duration(*secs)*time.Second, *grep, *all); err != nil {
			fail("%v", err)
		}
		return
	case "setup":
		mgr := fs.String("manager", "http://localhost:8790", "a home manager (no sign-in), on this computer")
		apps := fs.String("apps", "", "comma-separated app ids (default: the robot's apps there, else all)")
		start := fs.String("start", "", "the app it starts with (default: as there, else the first)")
		remote := fs.Bool("remote", true, "let the manager change the robot's apps later")
		askPin := fs.Bool("ask-pin", true, "the robot asks on its screen before it switches apps or its start app changes")
		second := fs.Bool("second", false, "the manager's linked upstream (sm.w42.eu) may become the robot's manager on its screen")
		fs.Parse(args)
		name := *port
		if name == "" {
			var err error
			if name, err = findPort(); err != nil {
				fail("%v", err)
			}
		}
		if err := managerSetup(name, strings.TrimRight(*mgr, "/"), *apps, *start, *remote, *askPin, *second); err != nil {
			fail("%v", err)
		}
		return
	case "hello", "restart", "pair", "status":
		fs.Parse(args)
		req = map[string]any{"op": op}
	case "provision":
		url := fs.String("url", "", "server URL, ws:// or wss://, e.g. wss://raw.sa.w42.eu")
		name := fs.String("name", "", "the server's name on the robot (default: the URL)")
		tokenFile := fs.String("token-file", "", "file with the robot's token for that server")
		makeDefault := fs.Bool("default", false, "make it the server the robot connects to at start")
		autostart := fs.Bool("autostart", false, "open Embody Mode after every power-on (firmware with automation)")
		ssid := fs.String("wifi-ssid", "", "Wi-Fi network to add")
		wifiPassFile := fs.String("wifi-password-file", "", "file with the Wi-Fi password")
		jsonFile := fs.String("json", "", "a whole provision request from this JSON file (e.g. a manager's setup answer: servers, pin, manager, manager2)")
		fs.Parse(args)
		req = map[string]any{"op": "provision"}
		if *jsonFile != "" {
			b, err := os.ReadFile(*jsonFile)
			if err != nil || json.Unmarshal(b, &req) != nil {
				fail("json: %v", err)
			}
			req["op"] = "provision"
		}
		if *url != "" {
			token, err := readFile(*tokenFile)
			if err != nil {
				fail("token: %v", err)
			}
			req["server"] = map[string]string{"name": *name, "url": *url, "token": token}
			req["default"] = *makeDefault
		}
		if *autostart {
			req["autostart"] = true
		}
		if *ssid != "" {
			pass, err := readFile(*wifiPassFile)
			if err != nil && *wifiPassFile != "" {
				fail("wifi password: %v", err)
			}
			req["wifi"] = map[string]string{"ssid": *ssid, "password": pass}
		}
		if len(req) == 1 {
			fail("nothing to provision: give -url and -token-file, -wifi-ssid, or -json")
		}
	case "swipe":
		x0 := fs.Int("x0", 160, "from x")
		y0 := fs.Int("y0", 236, "from y (236: the bottom edge)")
		x1 := fs.Int("x1", 160, "to x")
		y1 := fs.Int("y1", 150, "to y")
		ms := fs.Int("ms", 300, "how long")
		fs.Parse(args)
		req = map[string]any{"op": "swipe", "x0": *x0, "y0": *y0, "x1": *x1, "y1": *y1, "ms": *ms}
	case "stall":
		seconds := fs.Int("seconds", 30, "how long the app loop stops (1..120)")
		fs.Parse(args)
		req = map[string]any{"op": "stall", "seconds": *seconds}
	case "command":
		args0 := fs.String("args", "{}", "the command's arguments (JSON)")
		fs.Parse(args)
		var a map[string]any
		if fs.NArg() != 1 || json.Unmarshal([]byte(*args0), &a) != nil {
			fail("usage: s-w42-eu-usb command [-args '{\"on\":true}'] <command> (test builds only)")
		}
		req = map[string]any{"op": "command", "command": fs.Arg(0), "args": a}
	default:
		usage()
	}

	name := *port
	if name == "" {
		var err error
		if name, err = findPort(); err != nil {
			fail("%v", err)
		}
	}
	res, err := talk(name, req)
	if err != nil {
		fail("%v", err)
	}
	if b64, ok := res["jpeg"].(string); ok && out != "" {
		jpeg, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			fail("screenshot: %v", err)
		}
		if err := os.WriteFile(out, jpeg, 0o644); err != nil {
			fail("%v", err)
		}
		delete(res, "jpeg")
		res["saved"] = fmt.Sprintf("%s (%d bytes)", out, len(jpeg))
	}
	pretty, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(pretty))
	if ok, _ := res["ok"].(bool); !ok {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: s-w42-eu-usb hello | status | log | provision [flags] | restart | pair | screenshot | tap | swipe | launch | stall | command   (-h for flags)")
	os.Exit(2)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "s-w42-eu-usb: "+format+"\n", a...)
	os.Exit(1)
}

func readFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("no file given")
	}
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

// findPort returns the first Espressif USB serial device (USB vendor 0x303A).
func findPort() (string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return "", err
	}
	for _, p := range ports {
		if p.IsUSB && strings.EqualFold(p.VID, "303A") {
			return p.Name, nil
		}
	}
	return "", errors.New("no Stackchan on USB: plug it in with a data cable (USB-C on the head)")
}

// talk sends one request and waits for its answer. Opening the port can restart the robot, so
// hello is repeated until the robot answers (it boots in a few seconds); then the request goes
// once: a provision that changes the default server waits for a tap on the robot's screen, and
// repeated copies would pile up in its USB buffer meanwhile.
func talk(name string, req map[string]any) (map[string]any, error) {
	p, err := serial.Open(name, &serial.Mode{BaudRate: 115200})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer p.Close()
	p.SetReadTimeout(500 * time.Millisecond)
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(p)
		sc.Buffer(make([]byte, 64<<10), 1<<20) // a screenshot is one ~40 KB line
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	send := func(r map[string]any) {
		line, _ := json.Marshal(r)
		p.Write([]byte(prefix + string(line) + "\n"))
	}
	answer := func(timeout time.Duration, resend func()) (map[string]any, error) {
		deadline := time.After(timeout)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					return nil, errors.New("the robot went away")
				}
				i := strings.Index(l, prefix)
				if i < 0 {
					continue // a log line
				}
				var res map[string]any
				if json.Unmarshal([]byte(l[i+len(prefix):]), &res) == nil {
					return res, nil
				}
			case <-tick.C:
				if resend != nil {
					resend()
				}
			case <-deadline:
				return nil, errors.New("no answer: is the robot on, with firmware that has USB setup (Embody Mode 2026-10 or newer)?")
			}
		}
	}
	hello := map[string]any{"op": "hello"}
	send(hello)
	first, err := answer(25*time.Second, func() { send(hello) })
	if err != nil || req["op"] == "hello" {
		return first, err
	}
	send(req)
	if req["op"] == "provision" {
		fmt.Fprintln(os.Stderr, "s-w42-eu-usb: if the robot asks on its screen, tap Yes to accept or No to refuse (within a minute)")
	}
	return answer(75*time.Second, nil)
}

// noise: the periodic log lines (memory, NFC polls, backlight), left out unless -all.
var noise = regexp.MustCompile(`SystemInfo: free sram|nfc: \d+ polls|Backlight: Set brightness`)
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// streamLog prints the robot's log lines for d (0: until interrupted). Opening the port does not
// restart the robot.
func streamLog(name string, d time.Duration, grep string, all bool) error {
	var only *regexp.Regexp
	if grep != "" {
		var err error
		if only, err = regexp.Compile(grep); err != nil {
			return err
		}
	}
	p, err := serial.Open(name, &serial.Mode{BaudRate: 115200})
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer p.Close()
	p.SetReadTimeout(500 * time.Millisecond)
	var deadline time.Time
	if d > 0 {
		deadline = time.Now().Add(d)
	}
	r := bufio.NewReader(p)
	for deadline.IsZero() || time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if line == "" && err != nil {
			continue // the read timed out: nothing for now
		}
		line = strings.TrimRight(ansi.ReplaceAllString(line, ""), "\r\n")
		if line == "" || strings.Contains(line, prefix) || (!all && noise.MatchString(line)) || (only != nil && !only.MatchString(line)) {
			continue
		}
		fmt.Println(line)
	}
	return nil
}

// managerSetup writes the robot's apps from a home manager, as its page does ("Write apps"):
// hello, the manager's setup for this robot id (new tokens, its key), provision, restart.
func managerSetup(port, mgr, apps, start string, remote, askPin, second bool) error {
	hello, err := talk(port, map[string]any{"op": "hello"})
	if err != nil {
		return err
	}
	id, _ := hello["id"].(string)
	firmware, _ := hello["firmware"].(string)
	var me struct {
		Apps   []struct{ ID string }
		Robots []struct {
			ID    string
			Apps  []string
			Start string
		}
	}
	if err := getJSON(mgr+"/api/me", &me); err != nil {
		return err
	}
	var list []string
	if apps != "" {
		list = strings.Split(apps, ",")
	}
	for _, r := range me.Robots {
		if r.ID == id && len(list) == 0 {
			list = r.Apps
			if start == "" {
				start = r.Start
			}
		}
	}
	if len(list) == 0 {
		for _, a := range me.Apps {
			list = append(list, a.ID)
		}
	}
	body, _ := json.Marshal(map[string]any{"apps": list, "start": start, "remote_apps": remote, "ask_pin": askPin,
		"second": second, "firmware": firmware, "installed": false})
	req, _ := http.NewRequest("POST", mgr+"/api/my/robots/"+id+"/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", mgr) // the manager's own page, on this computer
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var setup struct {
		Servers []struct {
			Name, URL, Token string
			E2E              bool
		}
		Start    string
		Manager  map[string]any
		Manager2 map[string]any
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&setup); err != nil {
		return err
	}
	servers := []map[string]any{}
	names := []string{}
	for _, s := range setup.Servers {
		srv := map[string]any{"name": s.Name, "url": s.URL, "token": s.Token}
		if s.E2E {
			srv["e2e"] = true // the robot turns end-to-end encryption on for it
		}
		servers = append(servers, srv)
		names = append(names, s.Name)
	}
	prov := map[string]any{"op": "provision", "servers": servers, "pin": setup.Start, "tz": posixTZ(time.Now())}
	if setup.Manager != nil {
		prov["manager"] = setup.Manager
	}
	if setup.Manager2 != nil {
		prov["manager2"] = setup.Manager2
	}
	res, err := talk(port, prov)
	if err != nil {
		return err
	}
	if ok, _ := res["ok"].(bool); !ok {
		return fmt.Errorf("the robot refused: %v", res["error"])
	}
	talk(port, map[string]any{"op": "restart"})
	fmt.Printf("%s: %s written, starts with %s; restarting\n", id, strings.Join(names, ", "), setup.Start)
	return nil
}

func getJSON(url string, v any) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// posixTZ is this computer's time zone as a POSIX TZ string for the robot (the same rule as the
// setup page's usb.js posixTZ): the offsets of January and July, EU or US summer time rules.
func posixTZ(now time.Time) string {
	zone := os.Getenv("TZ")
	if zone == "" {
		if link, err := os.Readlink("/etc/localtime"); err == nil {
			_, zone, _ = strings.Cut(link, "zoneinfo/")
		}
	}
	_, jan := time.Date(now.Year(), 1, 1, 12, 0, 0, 0, time.Local).Zone()
	_, jul := time.Date(now.Year(), 7, 1, 12, 0, 0, 0, time.Local).Zone()
	off := func(east int) string { // POSIX: hours WEST of UTC
		m := -east / 60
		sign := ""
		if m < 0 {
			sign, m = "-", -m
		}
		if m%60 != 0 {
			return fmt.Sprintf("%s%d:%02d", sign, m/60, m%60)
		}
		return fmt.Sprintf("%s%d", sign, m/60)
	}
	std, dst := min(jan, jul), max(jan, jul)
	switch {
	case std == dst || jan > jul:
		return "UTC" + off(std)
	case strings.HasPrefix(zone, "Europe/"):
		return fmt.Sprintf("STD%sDST%s,M3.5.0/%d,M10.5.0/%d", off(std), off(dst), std/3600+1, dst/3600+1)
	case strings.HasPrefix(zone, "America/"):
		return fmt.Sprintf("STD%sDST%s,M3.2.0,M11.1.0", off(std), off(dst))
	}
	return "UTC" + off(std)
}
