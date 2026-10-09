package manager

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// seen is an app's robot-auth with what it reports (seen): the answer.
func seen(t *testing.T, h http.Handler, secret, robot, token, seenJSON string) RobotAuth {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/robot-auth", strings.NewReader(`{"robot":"`+robot+`","token":"`+token+`","seen":`+seenJSON+`}`))
	r.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var res RobotAuth
	json.NewDecoder(w.Result().Body).Decode(&res)
	return res
}

// S8: the owner sees the browsers paired on each app and removes them: the app gets the removal
// in its answer until it no longer reports them; an end-to-end browser is forgotten by the robot
// (a signed Forget on its channel, again on its next Hello until it has it).
func TestPairingsRemoved(t *testing.T) {
	m, base := serve(t, Config{})
	h := m.Handler()
	const id = "stackchan-000000000003"
	r := setUpSim(t, base, id, `{"apps":["raw"]}`)
	tok := r.Snapshot().Apps[0].Token
	two := `{"pairings":[` +
		`{"id":"aaaaaaaaaaaa","device":"Chrome on Android","paired":"2026-10-01T10:00:00Z","e2e":"0011223344556677"},` +
		`{"id":"bbbbbbbbbbbb","device":"Firefox on Linux","watching":true}]}`
	if ra := seen(t, h, "raw-secret", id, tok, two); !ra.OK || len(ra.Unpair) != 0 {
		t.Fatalf("seen: %+v", ra)
	}
	_, me := web(t, base, "GET", "/api/me", "")
	if ps := pageRobot(t, base, id).Pairings; len(ps) != 2 || ps[0].App != "raw" || ps[0].Device != "Chrome on Android" || !ps[0].E2E || !ps[1].Watching {
		t.Fatalf("pairings: %+v", ps)
	}
	if strings.Contains(me, "0011223344556677") {
		t.Error("the e2e id on the page")
	}

	if code, _ := web(t, base, "POST", "/api/my/robots/"+id+"/pairings/remove", `{"app":"raw","id":"cccccccccccc"}`); code != http.StatusNotFound {
		t.Errorf("unknown pairing: %d", code)
	}
	if code, body := web(t, base, "POST", "/api/my/robots/"+id+"/pairings/remove", `{"app":"raw","id":"aaaaaaaaaaaa"}`); code != http.StatusOK || !strings.Contains(body, `"forget":true`) {
		t.Fatalf("remove: %d %s", code, body)
	}
	eventually(t, "forgotten by the robot", func() bool { return slices.Equal(r.Snapshot().Forgot, []string{"0011223344556677"}) })
	if ra := seen(t, h, "raw-secret", id, tok, two); !slices.Equal(ra.Unpair, []string{"aaaaaaaaaaaa"}) {
		t.Fatalf("answer: %+v", ra)
	}
	// The app dropped it, the robot has the Forget: nothing more to do.
	one := `{"pairings":[{"id":"bbbbbbbbbbbb"}]}`
	if ra := seen(t, h, "raw-secret", id, tok, one); len(ra.Unpair) != 0 {
		t.Errorf("still unpairing: %+v", ra.Unpair)
	}
	eventually(t, "nothing left to do", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		rb := m.robots[id]
		return len(rb.Forget) == 0 && len(rb.Unpair) == 0 && len(rb.Pairings["raw"]) == 1
	})
	if code, _ := web(t, base, "POST", "/api/my/robots/"+id+"/pairings/remove", `{"app":"raw","all":true}`); code != http.StatusOK {
		t.Errorf("remove all: %d", code)
	}
	if ra := seen(t, h, "raw-secret", id, tok, one); !slices.Equal(ra.Unpair, []string{"bbbbbbbbbbbb"}) {
		t.Errorf("remove all: %+v", ra.Unpair)
	}
}

// "Watching now" holds only while the app keeps reporting (every 15 s while the robot is on it).
func TestWatchingIsFresh(t *testing.T) {
	m := &Manager{}
	rb := &Robot{ID: "stackchan-0a1b2c3d4e50"}
	m.pairingsSeenLocked(rb, "raw", []Pairing{{ID: "aaaaaaaaaaaa", Watching: true}})
	key := func() []pairingView { return m.pairingViewsFor(rb, []string{"raw"}) }
	if v := key(); len(v) != 1 || !v[0].Watching {
		t.Fatalf("fresh report: %+v", v)
	}
	rb.pairingsAt["raw"] = time.Now().Add(-2 * watchingFresh)
	if v := key(); len(v) != 1 || v[0].Watching {
		t.Fatalf("old report still watching: %+v", v)
	}
}
