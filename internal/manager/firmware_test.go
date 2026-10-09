package manager

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFirmwareRelease(t *testing.T) {
	part := []byte("the app image")
	bad := false
	gets := 0
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		switch r.URL.Path {
		case "/releases/latest/download/manifest.json":
			fmt.Fprintf(w, `{"version":"v1","parts":[{"path":"stack-chan.bin","offset":131072,"sha256":"%s"}]}`, sha256Hex(part))
		case "/releases/latest/download/stack-chan.bin":
			if bad {
				w.Write([]byte("something else"))
				return
			}
			w.Write(part)
		default:
			http.NotFound(w, r)
		}
	}))
	defer github.Close()

	s := &Manager{cfg: Config{}, log: slog.New(slog.DiscardHandler)}
	s.firmware = newFirmwareRelease(github.URL+"/releases", "latest", t.TempDir())
	get := func(name string) (int, string) {
		r := httptest.NewRequest("GET", "/firmware/"+name, nil)
		r.SetPathValue("file", name)
		r.RemoteAddr, r.Host = "127.0.0.1:5000", "localhost:8790"
		w := httptest.NewRecorder()
		s.handleFirmware(w, r)
		body, _ := io.ReadAll(w.Result().Body)
		return w.Code, string(body)
	}
	if code, body := get("manifest.json"); code != 200 || body == "" {
		t.Fatalf("manifest: %d %s", code, body)
	}
	if code, body := get("stack-chan.bin"); code != 200 || body != string(part) {
		t.Fatalf("part: %d %q", code, body)
	}
	before := gets
	if code, _ := get("stack-chan.bin"); code != 200 || gets != before {
		t.Errorf("a second request should come from the cache (%d downloads more)", gets-before)
	}
	if code, _ := get("other.bin"); code != 404 {
		t.Errorf("a file outside the manifest: %d, want 404", code)
	}

	// A part that does not match the manifest is refused.
	s.firmware = newFirmwareRelease(github.URL+"/releases", "latest", t.TempDir())
	bad = true
	if code, _ := get("stack-chan.bin"); code != 404 {
		t.Errorf("a damaged part: %d, want 404", code)
	}
}
