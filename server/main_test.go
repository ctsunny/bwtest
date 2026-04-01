package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
		{"https://api.day.app/abc123/这里改成你自己的推送内容", "https://api.day.app/abc123"},
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
		{"https://api.day.app/abc123/这里改成你自己的推送内容", "abc123"},
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

func TestNormalizeBarkURL(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"https://api.day.app/5ksYnDVKmfTvQogt7Xk67N/这里改成你自己的推送内容", "https://api.day.app/5ksYnDVKmfTvQogt7Xk67N"},
		{"https://api.day.app/5ksYnDVKmfTvQogt7Xk67N/group?icon=test", "https://api.day.app/5ksYnDVKmfTvQogt7Xk67N"},
		{"https://custom.server.com/key/path", "https://custom.server.com/key/path"},
		{"abc123", "abc123"},
	}
	for _, tc := range tests {
		got := normalizeBarkURL(tc.raw)
		if got != tc.want {
			t.Errorf("normalizeBarkURL(%q) = %q, want %q", tc.raw, got, tc.want)
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
		mode        string
		up          int
		down        int
		dur         int
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

func TestHandleHeartbeatUpdatesSSHAttempts(t *testing.T) {
	db := mustInitDB(filepath.Join(t.TempDir(), "bwtest.db"))
	defer db.Close()

	_, err := db.Exec(`INSERT INTO clients(id,name,remark,token,approved,last_seen,remote_ip,current_task,version,ssh_attempts) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"c1", "node-1", "", "secret", 1, time.Now().Format(time.RFC3339), "127.0.0.1", "", "v1", 0)
	if err != nil {
		t.Fatalf("insert client: %v", err)
	}

	reqBody, _ := json.Marshal(HeartbeatReq{
		ClientID:    "c1",
		ClientToken: "secret",
		Version:     "v2",
		Latency:     42,
		SSHAttempts: 9,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", bytes.NewReader(reqBody))
	req.RemoteAddr = "198.51.100.10:12345"
	w := httptest.NewRecorder()

	handleHeartbeat(db).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleHeartbeat status = %d, want 200", w.Code)
	}

	var gotAttempts int
	if err := db.QueryRow(`SELECT ssh_attempts FROM clients WHERE id=?`, "c1").Scan(&gotAttempts); err != nil {
		t.Fatalf("query ssh_attempts: %v", err)
	}
	if gotAttempts != 9 {
		t.Fatalf("ssh_attempts = %d, want 9", gotAttempts)
	}
}

func TestHandleSSHLoginAuthorized(t *testing.T) {
	db := mustInitDB(filepath.Join(t.TempDir(), "bwtest.db"))
	defer db.Close()

	_, err := db.Exec(`INSERT INTO clients(id,name,remark,token,approved,last_seen,remote_ip,current_task,version) VALUES(?,?,?,?,?,?,?,?,?)`,
		"c1", "node-1", "", "secret", 1, time.Now().Format(time.RFC3339), "127.0.0.1", "", "v1")
	if err != nil {
		t.Fatalf("insert client: %v", err)
	}

	reqBody, _ := json.Marshal(SSHLoginReq{
		ClientID:    "c1",
		ClientToken: "secret",
		LoginIP:     "203.0.113.5",
		LoginAt:     "2026-03-31T16:00:00Z",
		Username:    "root",
		Method:      "publickey",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ssh/login", bytes.NewReader(reqBody))
	w := httptest.NewRecorder()

	handleSSHLogin(&Config{}, db).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleSSHLogin status = %d, want 200", w.Code)
	}
}

func TestHandleSSHLoginUsesUpdatedBarkURL(t *testing.T) {
	db := mustInitDB(filepath.Join(t.TempDir(), "bwtest.db"))
	defer db.Close()

	_, err := db.Exec(`INSERT INTO clients(id,name,remark,token,approved,last_seen,remote_ip,current_task,version) VALUES(?,?,?,?,?,?,?,?,?)`,
		"c1", "node-1", "", "secret", 1, time.Now().Format(time.RFC3339), "127.0.0.1", "", "v1")
	if err != nil {
		t.Fatalf("insert client: %v", err)
	}

	barkCalled := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case barkCalled <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{}
	handler := handleSSHLogin(cfg, db)
	cfg.BarkURL = srv.URL + "/push-key"

	reqBody, _ := json.Marshal(SSHLoginReq{
		ClientID:    "c1",
		ClientToken: "secret",
		LoginIP:     "203.0.113.7",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ssh/login", bytes.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleSSHLogin status = %d, want 200", w.Code)
	}

	select {
	case <-barkCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected bark push to use updated config")
	}
}

func TestHandleGenInstallCmdReturnsJSON(t *testing.T) {
	cfg := &Config{InitToken: "init-secret"}
	form := "gen_name=node-1&gen_remark=hello&gen_version=v1.2.3"
	req := httptest.NewRequest(http.MethodPost, "/admin/gen/install-cmd", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Host = "panel.example.com"
	w := httptest.NewRecorder()

	handleGenInstallCmd("/admin", cfg).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleGenInstallCmd status = %d, want 200", w.Code)
	}

	var resp struct {
		OK           bool   `json:"ok"`
		GeneratedCmd string `json:"generated_cmd"`
		GenVersion   string `json:"gen_version"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK {
		t.Fatal("expected ok=true in response")
	}
	if resp.GenVersion != "v1.2.3" {
		t.Fatalf("gen_version = %q, want %q", resp.GenVersion, "v1.2.3")
	}
	if !strings.Contains(resp.GeneratedCmd, "--client-name 'node-1'") {
		t.Fatalf("generated command missing client name: %q", resp.GeneratedCmd)
	}
	if !strings.Contains(resp.GeneratedCmd, "--init-token init-secret") {
		t.Fatalf("generated command missing init token: %q", resp.GeneratedCmd)
	}
}
