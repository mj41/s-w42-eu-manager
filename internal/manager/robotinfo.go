package manager

// What the manager remembers about each robot for its owner's page: a name, its status as it
// reports it on its channel (channel.go) and a history of what was done to it and how (over USB,
// here online, or by the robot). Nothing secret in any of it.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxHistory = 50
	maxNameLen = 40
)

// Event is one thing done to a robot.
type Event struct {
	Time time.Time `json:"time"`
	How  string    `json:"how"` // "usb", "online", "robot"
	What string    `json:"what"`
}

// noteLocked adds to the robot's history. m.mu held.
func (m *Manager) noteLocked(rb *Robot, how, what string) {
	rb.History = append(rb.History, Event{Time: time.Now().UTC().Truncate(time.Second), How: how, What: what})
	if len(rb.History) > maxHistory {
		rb.History = rb.History[len(rb.History)-maxHistory:]
	}
	m.requestSave()
}

// cleanName is a robot's name as shown: one line, at most maxNameLen characters.
func cleanName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for utf8.RuneCountInString(s) > maxNameLen {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s
}

// POST /api/my/robots/{id}/name {"name": "Ema's robot"} ("" = none: its id is shown).
func (m *Manager) handleRobotName(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	ok := m.owner(w, r)
	var req struct {
		Name *string `json:"name"`
	}
	if !ok || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req) != nil || req.Name == nil {
		http.Error(w, `sign in, then {"name": "…"}`, http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rb := m.robots[r.PathValue("id")]
	if rb == nil {
		http.NotFound(w, r)
		return
	}
	rb.Name = cleanName(*req.Name)
	m.requestSave()
	w.WriteHeader(http.StatusNoContent)
}
