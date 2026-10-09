package manager

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The official Embody Mode firmware for the setup over USB, fetched from its GitHub release when no
// -firmware-dir is given: the manifest (re-read every 10 minutes for "latest") and its parts,
// each checked against the manifest's SHA-256 and cached on disk by that hash.
const firmwareReleases = "https://github.com/mj41/StackChan/releases"

type firmwareManifest struct {
	Version string `json:"version"`
	Parts   []struct {
		Path   string `json:"path"`
		Offset int64  `json:"offset"`
		SHA256 string `json:"sha256"`
	} `json:"parts"`
}

type firmwareRelease struct {
	base     string // e.g. https://github.com/mj41/StackChan/releases
	release  string // "latest" or a tag, e.g. "embody-v0.1.0"
	cacheDir string
	client   *http.Client

	mu       sync.Mutex
	manifest []byte
	parsed   firmwareManifest
	fetched  time.Time
	parts    map[string][]byte // by SHA-256: also without a writable cache directory
}

func newFirmwareRelease(base, release, cacheDir string) *firmwareRelease {
	return &firmwareRelease{base: base, release: release, cacheDir: cacheDir, client: &http.Client{Timeout: 2 * time.Minute}}
}

func (f *firmwareRelease) url(name string) string {
	if f.release == "latest" {
		return f.base + "/latest/download/" + name
	}
	return f.base + "/download/" + f.release + "/" + name
}

func (f *firmwareRelease) download(name string, limit int64) ([]byte, error) {
	resp, err := f.client.Get(f.url(name))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", f.url(name), resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("%s: larger than %d bytes", name, limit)
	}
	return data, err
}

// getManifest returns the release's manifest, fetched again after 10 minutes for "latest"
// (a fixed tag never changes). A failed refresh keeps the one it has.
func (f *firmwareRelease) getManifest() ([]byte, firmwareManifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.manifest != nil && (f.release != "latest" || time.Since(f.fetched) < 10*time.Minute) {
		return f.manifest, f.parsed, nil
	}
	data, err := f.download("manifest.json", 64<<10)
	var m firmwareManifest
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if err == nil && len(m.Parts) == 0 {
		err = fmt.Errorf("manifest without parts")
	}
	if err != nil {
		if f.manifest != nil {
			return f.manifest, f.parsed, nil
		}
		return nil, m, err
	}
	f.manifest, f.parsed, f.fetched = data, m, time.Now()
	return data, m, nil
}

// part returns a part of the manifest, from the cache or downloaded and checked.
func (f *firmwareRelease) part(name string) ([]byte, error) {
	_, m, err := f.getManifest()
	if err != nil {
		return nil, err
	}
	sum := ""
	for _, p := range m.Parts {
		if p.Path == name {
			sum = p.SHA256
		}
	}
	if len(sum) != 64 {
		return nil, os.ErrNotExist
	}
	f.mu.Lock()
	data, ok := f.parts[sum]
	f.mu.Unlock()
	if ok {
		return data, nil
	}
	cached := filepath.Join(f.cacheDir, sum+".bin")
	if data, err := os.ReadFile(cached); f.cacheDir != "" && err == nil && sha256Hex(data) == sum {
		f.keep(sum, data)
		return data, nil
	}
	data, err = f.download(name, 32<<20)
	if err != nil {
		return nil, err
	}
	if sha256Hex(data) != sum {
		return nil, fmt.Errorf("%s: checksum differs from the manifest", name)
	}
	if f.cacheDir != "" && os.MkdirAll(f.cacheDir, 0o755) == nil {
		tmp := cached + ".tmp"
		if os.WriteFile(tmp, data, 0o644) == nil {
			os.Rename(tmp, cached)
		}
	}
	f.keep(sum, data)
	return data, nil
}

// keep holds the parts of the current manifest in memory, and forgets older ones.
func (f *firmwareRelease) keep(sum string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current := map[string][]byte{}
	for _, p := range f.parsed.Parts {
		if d, ok := f.parts[p.SHA256]; ok {
			current[p.SHA256] = d
		}
	}
	current[sum] = data
	f.parts = current
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (f *firmwareRelease) serve(w http.ResponseWriter, r *http.Request, name string) error {
	if name == "manifest.json" {
		data, _, err := f.getManifest()
		if err != nil {
			return err
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
		return nil
	}
	data, err := f.part(name)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	return nil
}
