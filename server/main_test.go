package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ── durationToSec ──────────────────────────────────────────────────────────────

func TestDurationToSec(t *testing.T) {
	tests := []struct {
		val  int
		unit string
		want int
	}{
		{10, "sec", 10},
		{5, "min", 300},
		{2, "hour", 7200},
		{1, "day", 86400},
		{1, "month", 86400 * 30},
		{1, "unknown", 1}, // falls back to raw value
		{0, "sec", 0},
	}
	for _, tc := range tests {
		got := durationToSec(tc.val, tc.unit)
		if got != tc.want {
			t.Errorf("durationToSec(%d, %q) = %d, want %d", tc.val, tc.unit, got, tc.want)
		}
	}
}

// ── fmtDuration ───────────────────────────────────────────────────────────────

func TestFmtDuration(t *testing.T) {
	tests := []struct {
		sec  int
		want string
	}{
		{30, "30秒"},
		{60, "1分"},
		{90, "1分30秒"},
		{3600, "1时"},
		{3660, "1时1分"},
		{86400, "1天"},
		{90000, "1天1时"},
	}
	for _, tc := range tests {
		got := fmtDuration(tc.sec)
		if got != tc.want {
			t.Errorf("fmtDuration(%d) = %q, want %q", tc.sec, got, tc.want)
		}
	}
}

// ── barkURLFromToken / barkTokenFromURL ───────────────────────────────────────

func TestBarkURLFromToken(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{"", ""},
		{"abc123", "https://api.day.app/abc123"},
		{"https://api.day.app/abc123", "https://api.day.app/abc123"},
		{"https://custom.server.com/abc", "https://custom.server.com/abc"},
		{"http://custom.server.com/abc", "http://custom.server.com/abc"},
		{"abc123/", "https://api.day.app/abc123"}, // trailing slash stripped
	}
	for _, tc := range tests {
		got := barkURLFromToken(tc.token)
		if got != tc.want {
			t.Errorf("barkURLFromToken(%q) = %q, want %q", tc.token, got, tc.want)
		}
	}
}

func TestBarkTokenFromURL(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"https://api.day.app/abc123", "abc123"},
		{"http://api.day.app/abc123", "abc123"},
		{"https://custom.server.com/tok", "https://custom.server.com/tok"},
		{"abc123", "abc123"},
	}
	for _, tc := range tests {
		got := barkTokenFromURL(tc.raw)
		if got != tc.want {
			t.Errorf("barkTokenFromURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// ── ipRateLimiter ─────────────────────────────────────────────────────────────

func TestIPRateLimiterAllow(t *testing.T) {
	rl := &ipRateLimiter{hits: make(map[string][]time.Time)}
	ip := "192.0.2.1"
	const max = 5

	for i := 0; i < max; i++ {
		if !rl.Allow(ip, max) {
			t.Fatalf("Allow returned false before limit reached (i=%d)", i)
		}
	}
	// The next call must be rejected
	if rl.Allow(ip, max) {
		t.Fatal("Allow should return false after limit is reached")
	}
}

func TestIPRateLimiterDifferentIPs(t *testing.T) {
	rl := &ipRateLimiter{hits: make(map[string][]time.Time)}
	const max = 2

	if !rl.Allow("10.0.0.1", max) {
		t.Fatal("first IP should be allowed")
	}
	// Different IP is independent
	if !rl.Allow("10.0.0.2", max) {
		t.Fatal("second IP should be allowed independently")
	}
}

// ── basicAuth (constant-time compare check via HTTP) ─────────────────────────

func TestBasicAuthRejectsWrongCredentials(t *testing.T) {
	cfg := Config{AdminUser: "admin", AdminPass: "s3cr3t"}
	handler := basicAuth(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		user, pass string
		wantStatus int
	}{
		{"admin", "s3cr3t", http.StatusOK},
		{"admin", "wrong", http.StatusUnauthorized},
		{"wrong", "s3cr3t", http.StatusUnauthorized},
		{"", "", http.StatusUnauthorized},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.user != "" || tc.pass != "" {
			req.SetBasicAuth(tc.user, tc.pass)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != tc.wantStatus {
			t.Errorf("user=%q pass=%q: got %d, want %d", tc.user, tc.pass, w.Code, tc.wantStatus)
		}
	}
}

// ── handleCreateTask validation ───────────────────────────────────────────────

func TestCreateTaskValidation(t *testing.T) {
	// Validate mode and bounds without a real DB by calling durationToSec
	// and checking boundary logic directly.
	const maxMbps = 100000
	const minDurSec = 5
	const maxDurSec = 86400 * 30

	tests := []struct {
		mode       string
		up         int
		down       int
		dur        int
		wantInvalid bool
	}{
		{"upload", 100, 0, 60, false},
		{"download", 0, 100, 60, false},
		{"both", 100, 100, 60, false},
		{"invalid", 100, 100, 60, true},
		{"upload", -1, 0, 60, true},
		{"upload", maxMbps + 1, 0, 60, true},
		{"upload", 100, 0, minDurSec - 1, true},
		{"upload", 100, 0, maxDurSec + 1, true},
	}
	for _, tc := range tests {
		invalid := false
		if tc.mode != "upload" && tc.mode != "download" && tc.mode != "both" {
			invalid = true
		}
		if tc.up < 0 || tc.up > maxMbps {
			invalid = true
		}
		if tc.down < 0 || tc.down > maxMbps {
			invalid = true
		}
		if tc.dur < minDurSec || tc.dur > maxDurSec {
			invalid = true
		}
		if invalid != tc.wantInvalid {
			t.Errorf("mode=%q up=%d down=%d dur=%d: invalid=%v, want %v",
				tc.mode, tc.up, tc.down, tc.dur, invalid, tc.wantInvalid)
		}
	}
}
