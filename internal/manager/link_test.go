package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mj41/s-w42-eu-manager/internal/upstreamsim"
)

// upstream runs an upstream manager (upstreamsim) with its own Raw data (end-to-end encrypted).
func upstream(t *testing.T) *upstreamsim.Upstream {
	t.Helper()
	up := upstreamsim.Start("sm.example", upstreamsim.App{ID: "raw", Name: "Raw data", URL: "wss://raw.example", Web: "https://raw.example", E2E: true})
	t.Cleanup(up.Close)
	return up
}

// linkHome links the home manager at homeBase to up (S3), as the owner's browser does: start at
// home, approve up there, back home with the code.
func linkHome(t *testing.T, homeBase string, up *upstreamsim.Upstream, mode string, share bool) {
	t.Helper()
	code, body := web(t, homeBase, "POST", "/api/link/start",
		`{"mode":"`+mode+`","share_local":`+map[bool]string{true: "true", false: "false"}[share]+`,"return":"`+homeBase+`"}`)
	if code != http.StatusOK {
		t.Fatalf("link start: %d %s", code, body)
	}
	var start struct{ URL string }
	json.Unmarshal([]byte(body), &start)
	if !strings.HasPrefix(start.URL, up.URL+"/link/approve?") {
		t.Fatalf("approve URL: %s", start.URL)
	}
	q, _ := url.Parse(start.URL)
	form := url.Values{"return": {q.Query().Get("return")}, "state": {q.Query().Get("state")}}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.PostForm(up.URL+"/link/approve", form)
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve: %v %v", err, resp.Status)
	}
	back := resp.Header.Get("Location")
	if !strings.HasPrefix(back, homeBase+"/link/done?") {
		t.Fatalf("back to %s", back)
	}
	resp, err = noFollow.Get(back)
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("link done: %v %v", err, resp.Status)
	}
	eventually(t, "the link up", func() bool { return linkOf(t, homeBase).Link["connected"] == true && up.Linked() })
}

type meLink struct {
	Link map[string]any
	Apps []appView
}

func linkOf(t *testing.T, base string) meLink {
	t.Helper()
	_, body := web(t, base, "GET", "/api/me", "")
	var me meLink
	json.Unmarshal([]byte(body), &me)
	return me
}

func request(t *testing.T, up *upstreamsim.Upstream, robot, kind string, body any) upstreamsim.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := up.Request(ctx, robot, kind, body)
	if err != nil {
		t.Fatalf("request %s: %v", kind, err)
	}
	return res
}

// S3, S4, S8 (an upstream app), S10, S14: the home's robots as a read-only shadow upstream; an
// upstream app for a home robot; its paired browsers removed at home; the link down and back.
func TestLinkShadow(t *testing.T) {
	up := upstream(t)
	home, homeBase := serve(t, Config{UpstreamURL: up.URL, Name: "home on laptop"})
	const id = "stackchan-000000000010"
	r := setUpSim(t, homeBase, id, `{"apps":["raw","pet"],"start":"pet"}`)
	linkHome(t, homeBase, up, "read-only", false)

	// S3: the home and its robot upstream, live, read-only, no local address.
	eventually(t, "the shadow", func() bool { v := up.Robot(id); return v != nil && v["last_app"] == "pet" })
	h := up.Home()
	if h["name"] != "home on laptop" || h["mode"] != "read-only" || h["local_url"] != nil {
		t.Fatalf("home: %+v", h)
	}
	if apps, _ := h["apps"].([]any); len(apps) == 0 || apps[0].(map[string]any)["web"] != "" {
		t.Fatalf("home's apps: %+v", h["apps"])
	}
	r.SwitchOnScreen("wss://raw.example")
	eventually(t, "the shadow follows", func() bool { return up.Robot(id)["last_app"] == "raw" })
	if res := request(t, up, id, "switch", map[string]string{"app": "pet"}); res.Code != http.StatusForbidden {
		t.Errorf("a switch from a read-only link: %+v", res)
	}

	// S4: the upstream's catalog at home; one of its apps for the robot: a token from there, in
	// the signed list.
	eventually(t, "the upstream's catalog at home", func() bool {
		return slices.ContainsFunc(linkOf(t, homeBase).Apps, func(a appView) bool { return a.ID == "up:raw" && a.Upstream })
	})
	if code, body := web(t, homeBase, "POST", "/api/my/robots/"+id+"/apps", `{"apps":["raw","pet","up:raw"],"start":"pet"}`); code != http.StatusOK {
		t.Fatalf("add up:raw: %d %s", code, body)
	}
	eventually(t, "the upstream app on the robot", func() bool { return len(r.Snapshot().Apps) == 3 })
	upApp := r.Snapshot().Apps[2]
	if upApp.Name != "Raw data" || upApp.URL != "wss://raw.example" || upApp.Token != up.Granted(id, "raw") || !upApp.E2E {
		t.Fatalf("upstream app: %+v", upApp)
	}
	// S8 for an upstream app: it reports a browser; the home removes it; the upstream unpairs.
	up.Pairings(id, "raw", []map[string]string{{"id": "cccccccccccc", "device": "Safari on iOS", "e2e": "8899aabbccddeeff"}})
	eventually(t, "the pairing at home", func() bool {
		return slices.ContainsFunc(pageRobot(t, homeBase, id).Pairings, func(p pairingView) bool { return p.App == "up:raw" && p.Device == "Safari on iOS" })
	})
	if code, body := web(t, homeBase, "POST", "/api/my/robots/"+id+"/pairings/remove", `{"app":"up:raw","id":"cccccccccccc"}`); code != http.StatusOK {
		t.Fatalf("remove at home: %d %s", code, body)
	}
	eventually(t, "unpaired upstream", func() bool { return slices.Equal(up.Unpaired(id, "raw"), []string{"cccccccccccc"}) })
	eventually(t, "forgotten by the robot", func() bool { return slices.Contains(r.Snapshot().Forgot, "8899aabbccddeeff") })
	// Removing the app at home revokes it upstream.
	web(t, homeBase, "POST", "/api/my/robots/"+id+"/apps", `{"apps":["raw","pet"],"start":"pet"}`)
	eventually(t, "revoked upstream", func() bool { return up.Granted(id, "raw") == "" })

	// S14: the local address shared.
	if code, _ := web(t, homeBase, "POST", "/api/link/settings", `{"mode":"read-only","share_local":true}`); code != http.StatusNoContent {
		t.Fatalf("settings: %d", code)
	}
	eventually(t, "the local address upstream", func() bool { return up.Home()["local_url"] == homeBase })

	// S10: the upstream unreachable: home works on; back, it catches up.
	up.Drop()
	eventually(t, "home: not connected", func() bool { return linkOf(t, homeBase).Link["connected"] == false })
	r.SwitchOnScreen("wss://pet.example")
	eventually(t, "home still live", func() bool { return pageRobot(t, homeBase, id).LastApp == "pet" })
	home.wakeLink()
	eventually(t, "back, caught up", func() bool { return up.Linked() && up.Robot(id)["last_app"] == "pet" })

	// Unlinked at home: the link closes, the page offers the link again.
	if code, _ := web(t, homeBase, "POST", "/api/link/unlink", ""); code != http.StatusNoContent {
		t.Fatalf("unlink: %d", code)
	}
	eventually(t, "unlinked", func() bool { return !up.Linked() && linkOf(t, homeBase).Link["linked"] == false })
}

// S15, S16: full control through the link; the robot makes the upstream its primary on its
// screen.
func TestLinkFullAndMoved(t *testing.T) {
	up := upstream(t)
	home, homeBase := serve(t, Config{UpstreamURL: up.URL, Name: "home on laptop"})
	linkHome(t, homeBase, up, "full", false)

	// S16 setup: the second manager comes from the upstream over the link.
	const id = "stackchan-000000000011"
	code, body := web(t, homeBase, "POST", "/api/my/robots/"+id+"/setup", `{"apps":["raw","pet"],"start":"raw","second":true}`)
	var setup struct{ Manager2 map[string]any }
	json.Unmarshal([]byte(body), &setup)
	if code != http.StatusOK || setup.Manager2["name"] != "sm.example" || setup.Manager2["may_primary"] != true {
		t.Fatalf("setup with a second manager: %d %s", code, body)
	}
	r := setUpSim(t, homeBase, id, `{"apps":["raw","pet"],"start":"raw"}`)
	eventually(t, "the shadow", func() bool { return up.Robot(id) != nil })

	// S15: a switch from upstream; the home signs it; the robot asks; Yes.
	r.Answer = func(string) bool { return true }
	if res := request(t, up, id, "switch", map[string]string{"app": "pet"}); res.Code != http.StatusOK {
		t.Fatalf("switch from upstream: %+v", res)
	}
	eventually(t, "switched, seen upstream", func() bool {
		return r.Snapshot().Current == "wss://pet.example" && up.Robot(id)["last_app"] == "pet"
	})
	if !slices.ContainsFunc(pageRobot(t, homeBase, id).History, func(e Event) bool { return e.What == "asked to switch to pet (from "+home.upstreamName()+")" }) {
		t.Errorf("home history: %+v", pageRobot(t, homeBase, id).History)
	}
	if res := request(t, up, id, "apps", map[string]any{"apps": []string{"pet"}, "start": "pet"}); res.Code != http.StatusOK {
		t.Fatalf("apps from upstream: %+v", res)
	}
	eventually(t, "one app", func() bool { return len(r.Snapshot().Apps) == 1 })
	if res := request(t, up, id, "restart", nil); res.Code != http.StatusOK {
		t.Fatalf("restart from upstream: %+v", res)
	}
	eventually(t, "restarted", func() bool { return r.Snapshot().Restarts == 1 })
	// The home turns it off: refused at once.
	web(t, homeBase, "POST", "/api/link/settings", `{"mode":"read-only"}`)
	eventually(t, "read-only upstream", func() bool { return up.Home()["mode"] == "read-only" })
	if res := request(t, up, id, "restart", nil); res.Code != http.StatusForbidden {
		t.Errorf("restart after read-only: %+v", res)
	}

	// S16: the robot made the upstream its primary: the home shows where it went.
	up.Moved(id)
	eventually(t, "home: moved", func() bool { return pageRobot(t, homeBase, id).Moved == home.upstreamName() })
}

// Without "second" at the setup the robot cannot change its manager on its screen.
func TestNoSecondManager(t *testing.T) {
	_, base := serve(t, Config{})
	r := setUpSim(t, base, "stackchan-000000000012", `{"apps":["raw"]}`)
	if r.UseSecond() {
		t.Error("changed its manager without the setting")
	}
}

// A USB setup without a linked app drops it there too (its token ends upstream).
func TestSetupDropsLinkedApp(t *testing.T) {
	up := upstream(t)
	_, homeBase := serve(t, Config{UpstreamURL: up.URL, Name: "home on laptop"})
	const id = "stackchan-000000000013"
	r := setUpSim(t, homeBase, id, `{"apps":["raw","pet"],"start":"raw"}`)
	linkHome(t, homeBase, up, "read-only", false)
	eventually(t, "catalog", func() bool {
		return slices.ContainsFunc(linkOf(t, homeBase).Apps, func(a appView) bool { return a.ID == "up:raw" })
	})
	web(t, homeBase, "POST", "/api/my/robots/"+id+"/apps", `{"apps":["raw","pet","up:raw"],"start":"raw"}`)
	eventually(t, "the upstream app on the robot", func() bool { return len(r.Snapshot().Apps) == 3 })
	if up.Granted(id, "raw") == "" {
		t.Fatal("no token upstream")
	}
	setUpSim(t, homeBase, id, `{"apps":["raw","pet"],"start":"raw"}`)
	if v := pageRobot(t, homeBase, id); slices.Contains(v.Apps, "up:raw") {
		t.Errorf("still has up:raw: %v", v.Apps)
	}
	eventually(t, "revoked upstream", func() bool { return up.Granted(id, "raw") == "" })
}
