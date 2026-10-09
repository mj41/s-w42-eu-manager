package main

import (
	"os"
	"testing"
	"time"
)

func TestPosixTZ(t *testing.T) {
	cases := map[string]string{
		"Europe/Prague": "STD-1DST-2,M3.5.0/2,M10.5.0/3", "Europe/London": "STD0DST-1,M3.5.0/1,M10.5.0/2",
		"America/New_York": "STD5DST4,M3.2.0,M11.1.0", "Asia/Tokyo": "UTC-9", "Asia/Kolkata": "UTC-5:30",
		"Australia/Sydney": "UTC-10",
	}
	defer func(l *time.Location) { time.Local = l }(time.Local)
	for zone, want := range cases {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		time.Local = loc
		os.Setenv("TZ", zone)
		if got := posixTZ(time.Date(2026, 10, 7, 12, 0, 0, 0, loc)); got != want {
			t.Errorf("%s: %s, want %s", zone, got, want)
		}
	}
	os.Unsetenv("TZ")
}
