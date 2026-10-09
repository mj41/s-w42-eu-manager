package manager

// The browsers paired with a robot on each of its apps, as the apps report them (robot-auth
// "seen" pairings), for the owner to see and remove: the app then unpairs the browser (the
// robot-auth answer's "unpair"), and a browser with end-to-end encryption is also forgotten by the
// robot itself (a signed Forget on its channel), so it cannot read the robot even through another
// app.

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	maxPairings = 64 // per app and robot
	// An app reports a robot's pairings every 15 s while the robot is on it: "watching" older than
	// this is old news (the robot left the app, or the app stopped).
	watchingFresh = time.Minute
)

// Pairing is one browser paired with a robot on an app (as robotauth.Pairing).
type Pairing struct {
	ID       string    `json:"id"`
	Device   string    `json:"device,omitempty"`
	Paired   time.Time `json:"paired,omitzero"`
	LastSeen time.Time `json:"last_seen,omitzero"`
	Watching bool      `json:"watching,omitempty"`
	E2E      string    `json:"e2e,omitempty"` // its browser id on the robot
}

// pairingView is a pairing on the owner's page.
type pairingView struct {
	App      string     `json:"app"`
	ID       string     `json:"id"`
	Device   string     `json:"device,omitempty"`
	Paired   *time.Time `json:"paired,omitempty"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
	Watching bool       `json:"watching,omitempty"`
	E2E      bool       `json:"e2e,omitempty"`
	Removing bool       `json:"removing,omitempty"` // removed here; the app has not dropped it yet
}

// pairingsSeenLocked stores what an app reported and drops the removals it has done. m.mu held.
func (m *Manager) pairingsSeenLocked(rb *Robot, app string, ps []Pairing) {
	if len(ps) > maxPairings {
		ps = ps[:maxPairings]
	}
	if rb.Pairings == nil {
		rb.Pairings = map[string][]Pairing{}
	}
	rb.Pairings[app] = ps
	if rb.pairingsAt == nil {
		rb.pairingsAt = map[string]time.Time{}
	}
	rb.pairingsAt[app] = time.Now()
	if len(ps) == 0 {
		delete(rb.Pairings, app)
	}
	if ids := rb.Unpair[app]; len(ids) > 0 {
		left := slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
			return !slices.ContainsFunc(ps, func(p Pairing) bool { return p.ID == id })
		})
		if len(left) == 0 {
			delete(rb.Unpair, app)
		} else {
			rb.Unpair[app] = left
		}
	}
}

// pairingViews lists the robot's pairings by app (catalog order, then the linked sm.w42.eu's),
// oldest first. m.mu held.
func (m *Manager) pairingViews(rb *Robot) []pairingView {
	keys := []string{}
	for _, app := range m.apps {
		keys = append(keys, app.ID)
	}
	for _, id := range sortedKeys(rb.Upstream) {
		keys = append(keys, upstreamKey(id))
	}
	return m.pairingViewsFor(rb, keys)
}

// pairingViewsFor lists the robot's pairings on these apps (keys), in that order. m.mu held.
func (m *Manager) pairingViewsFor(rb *Robot, keys []string) []pairingView {
	out := []pairingView{}
	for _, key := range keys {
		fresh := time.Since(rb.pairingsAt[key]) < watchingFresh
		for _, p := range rb.Pairings[key] {
			v := pairingView{App: key, ID: p.ID, Device: p.Device, Watching: p.Watching && fresh, E2E: p.E2E != "",
				Removing: slices.Contains(rb.Unpair[key], p.ID)}
			if !p.Paired.IsZero() {
				t := p.Paired
				v.Paired = &t
			}
			if !p.LastSeen.IsZero() {
				t := p.LastSeen
				v.LastSeen = &t
			}
			out = append(out, v)
		}
	}
	return out
}

// POST /api/my/robots/{id}/pairings/remove {"app": "<app id>", "id": "<pairing id>"} or
// {"app": "<app id>", "all": true}: unpair browsers from the robot on that app.
func (m *Manager) handleRemovePairings(w http.ResponseWriter, r *http.Request) {
	m.robotAction(w, r, func(rb *Robot, body json.RawMessage) (int, map[string]any) {
		var req struct {
			App string `json:"app"`
			ID  string `json:"id"`
			All bool   `json:"all"`
		}
		if json.Unmarshal(body, &req) != nil || req.App == "" || (req.ID == "") == !req.All {
			return http.StatusBadRequest, map[string]any{"error": "bad_request", "message": `{"app": "…", "id": "…"} or {"app": "…", "all": true}`}
		}
		return m.removePairingsLocked(rb, req.App, req.ID, req.All, "online")
	})
}

// removePairingsLocked unpairs browsers on one app: the app drops them with its next robot-auth
// (for the linked sm.w42.eu's apps: through the link), and end-to-end ones are forgotten by the
// robot. m.mu held.
func (m *Manager) removePairingsLocked(rb *Robot, app, id string, all bool, how string) (int, map[string]any) {
	var removed []Pairing
	for _, p := range rb.Pairings[app] {
		if all || p.ID == id {
			removed = append(removed, p)
		}
	}
	if len(removed) == 0 {
		return http.StatusNotFound, map[string]any{"error": "no_pairing", "message": "no such pairing (gone already?)"}
	}
	if rb.Unpair == nil {
		rb.Unpair = map[string][]string{}
	}
	forget := false
	var ids []string
	for _, p := range removed {
		ids = append(ids, p.ID)
		if !slices.Contains(rb.Unpair[app], p.ID) {
			rb.Unpair[app] = append(rb.Unpair[app], p.ID)
		}
		if p.E2E != "" && !slices.Contains(rb.Forget, p.E2E) {
			rb.Forget = append(rb.Forget, p.E2E)
			forget = true
		}
	}
	if up, ok := strings.CutPrefix(app, "up:"); ok {
		m.linkUnpairLocked(rb, up, ids) // link.go
	}
	sort.Strings(rb.Forget)
	if forget {
		m.sendForgetLocked(rb) // now if it is connected, else when it says Hello (channel.go)
	}
	what := "a browser removed from " + app
	if len(removed) > 1 {
		what = "all browsers removed from " + app
	}
	if how != "online" {
		what += " (" + how + ")"
	}
	m.noteLocked(rb, "online", what)
	m.requestSave()
	return http.StatusOK, map[string]any{"ok": true, "removed": len(removed), "forget": forget}
}
