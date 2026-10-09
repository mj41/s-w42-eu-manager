package manager

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// catalog writes an app catalog: raw and pet check tokens with the manager, home has a shared token.
func catalog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	apps := []map[string]string{
		{"id": "raw", "name": "Raw data", "url": "wss://raw.example", "secret_file": write("raw", "raw-secret")},
		{"id": "pet", "name": "Pet", "url": "wss://pet.example", "secret_file": write("pet", "pet-secret")},
		{"id": "home", "name": "Home", "url": "ws://192.168.1.10:8765", "token_file": write("home", "shared-token")},
	}
	b, _ := json.Marshal(apps)
	return write("apps.json", string(b))
}

func newManager(t *testing.T, cfg Config) *Manager {
	t.Helper()
	cfg.Log = slog.New(slog.DiscardHandler)
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// call sends a request as a browser on this computer (or with a session cookie).
func call(t *testing.T, h http.Handler, method, path, body, session string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr, r.Host = "127.0.0.1:5000", "localhost:8790"
	r.Header.Set("Origin", "http://localhost:8790")
	r.Header.Set("Content-Type", "application/json")
	if session != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b)
}

// robotAuth asks as an app (its secret) whether token is the robot's.
func robotAuth(t *testing.T, h http.Handler, secret, robot, token string) (int, RobotAuth) {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/robot-auth", strings.NewReader(`{"robot":"`+robot+`","token":"`+token+`"}`))
	r.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var res RobotAuth
	json.NewDecoder(w.Result().Body).Decode(&res)
	return w.Code, res
}

type setupAnswer struct {
	Servers []struct{ ID, Name, URL, Token string } `json:"servers"`
	Start   string                                  `json:"start"`
}

func TestCatalogErrors(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "s"), []byte("x"), 0o600)
	for name, body := range map[string]string{
		"bad id":       `[{"id":"Raw!","url":"wss://a","secret_file":"` + dir + `/s"}]`,
		"duplicate":    `[{"id":"a","url":"wss://a","secret_file":"` + dir + `/s"},{"id":"a","url":"wss://b","secret_file":"` + dir + `/s"}]`,
		"no scheme":    `[{"id":"a","url":"https://a","secret_file":"` + dir + `/s"}]`,
		"neither":      `[{"id":"a","url":"wss://a"}]`,
		"both":         `[{"id":"a","url":"wss://a","secret_file":"` + dir + `/s","token_file":"` + dir + `/s"}]`,
		"missing file": `[{"id":"a","url":"wss://a","secret_file":"` + dir + `/nope"}]`,
	} {
		p := filepath.Join(dir, "apps.json")
		os.WriteFile(p, []byte(body), 0o600)
		if _, err := LoadApps(p); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// The page on this computer sets robots up; each app that checks tokens gets a token of its own,
// the home app its shared token; apps verify with robot-auth.
func TestLocalSetupAndRobotAuth(t *testing.T) {
	m := newManager(t, Config{AppsFile: catalog(t)})
	h := m.Handler()

	// Another computer gets nothing.
	r := httptest.NewRequest("GET", "/api/me", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"signed_in":false`) {
		t.Fatalf("me from another computer: %s", w.Body.String())
	}

	code, body := call(t, h, "POST", "/api/my/robots/stackchan-0a1b2c3d4e50/setup", `{"apps":["raw","pet","home"],"start":"pet"}`, "")
	if code != 200 {
		t.Fatalf("setup: %d %s", code, body)
	}
	var a setupAnswer
	json.Unmarshal([]byte(body), &a)
	tokens := map[string]string{}
	for _, s := range a.Servers {
		tokens[s.ID] = s.Token
	}
	if _, me := call(t, h, "GET", "/api/me", "", ""); !strings.Contains(me, `"web":"https://pet.example"`) || !strings.Contains(me, `"web":"http://192.168.1.10:8765"`) {
		t.Fatalf("app pages from their robot URLs: %s", me)
	}
	if len(a.Servers) != 3 || a.Start != "wss://pet.example" || tokens["home"] != "shared-token" || len(tokens["raw"]) != 64 || tokens["raw"] == tokens["pet"] {
		t.Fatalf("setup answer: %s", body)
	}

	// Each app accepts only its own token for the robot, and learns the owner.
	if code, res := robotAuth(t, h, "raw-secret", "stackchan-0a1b2c3d4e50", tokens["raw"]); code != 200 || !res.OK || res.Owner != "local" {
		t.Fatalf("raw with its token: %d %+v", code, res)
	}
	if _, res := robotAuth(t, h, "raw-secret", "stackchan-0a1b2c3d4e50", tokens["pet"]); res.OK {
		t.Fatal("raw accepted the pet's token")
	}
	if _, res := robotAuth(t, h, "pet-secret", "stackchan-0a1b2c3d4e51", tokens["pet"]); res.OK {
		t.Fatal("the token worked for another robot")
	}
	if code, _ := robotAuth(t, h, "wrong-secret", "stackchan-0a1b2c3d4e50", tokens["raw"]); code != http.StatusUnauthorized {
		t.Fatalf("an unknown app: %d", code)
	}

	// Setting up again gives new tokens; the old ones stop working. Removing the robot ends all.
	_, body = call(t, h, "POST", "/api/my/robots/stackchan-0a1b2c3d4e50/setup", `{"apps":["raw"]}`, "")
	var b setupAnswer
	json.Unmarshal([]byte(body), &b)
	if _, res := robotAuth(t, h, "raw-secret", "stackchan-0a1b2c3d4e50", tokens["raw"]); res.OK {
		t.Fatal("an old token still works after a new setup")
	}
	if _, res := robotAuth(t, h, "pet-secret", "stackchan-0a1b2c3d4e50", tokens["pet"]); res.OK {
		t.Fatal("an app no longer approved still accepts the robot")
	}
	if _, res := robotAuth(t, h, "raw-secret", "stackchan-0a1b2c3d4e50", b.Servers[0].Token); !res.OK {
		t.Fatal("the new token does not work")
	}
	if code, _ := call(t, h, "DELETE", "/api/my/robots/stackchan-0a1b2c3d4e50", "", ""); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if _, res := robotAuth(t, h, "raw-secret", "stackchan-0a1b2c3d4e50", b.Servers[0].Token); res.OK {
		t.Fatal("a removed robot's token still works")
	}

	// Bad requests.
	for _, c := range []string{`{"apps":[]}`, `{"apps":["nope"]}`, `{"apps":["raw"],"start":"pet"}`} {
		if code, _ := call(t, h, "POST", "/api/my/robots/stackchan-0a1b2c3d4e50/setup", c, ""); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", c, code)
		}
	}
}

// The state survives a restart (an old state file with keys this manager no longer has loads
// too); changing requests need this site's Origin.
func TestStateAndOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{AppsFile: catalog(t), StateFile: path}
	m := newManager(t, cfg)
	h := m.Handler()
	const id = "stackchan-000000000001"
	if code, body := call(t, h, "POST", "/api/my/robots/"+id+"/setup", `{"apps":["raw"]}`, ""); code != 200 {
		t.Fatalf("setup: %d %s", code, body)
	}
	if code, _ := call(t, h, "POST", "/api/my/robots/"+id+"/access", `{"public":true}`, ""); code != http.StatusNoContent {
		t.Fatalf("public: %d", code)
	}
	if err := m.SaveState(); err != nil {
		t.Fatal(err)
	}
	m2 := newManager(t, cfg)
	if rb := m2.robots[id]; rb == nil || rb.Owner != localOwner.Key || !rb.Public {
		t.Fatalf("after a restart: %+v", rb)
	}
	b, _ := os.ReadFile(path)
	var st map[string]json.RawMessage
	json.Unmarshal(b, &st)
	st["logins"], st["users"], st["homes"] = json.RawMessage(`{}`), json.RawMessage(`{}`), json.RawMessage(`{}`)
	b, _ = json.Marshal(st)
	os.WriteFile(path, b, 0o600)
	if m3 := newManager(t, cfg); m3.robots[id] == nil {
		t.Fatal("an old state file: the robot is gone")
	}
	r := httptest.NewRequest("POST", "/api/my/robots/"+id+"/setup", strings.NewReader(`{"apps":["raw"]}`))
	r.RemoteAddr, r.Host = "127.0.0.1:5000", "localhost:8790"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("without Origin: %d", w.Code)
	}
}

// A provided signing key that is missing is an error, never a new key.
func TestNoNewSigningKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.pem")
	if _, err := New(Config{SigningKeyFile: p, NoNewSigningKey: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("a new key was made")
	}
}
