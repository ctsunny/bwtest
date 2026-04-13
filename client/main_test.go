package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ── token ─────────────────────────────────────────────────────────────────────

func TestTokenLength(t *testing.T) {
	for _, n := range []int{4, 8, 16} {
		tok := token(n)
		// token(n) returns hex of n random bytes → 2n hex chars
		if len(tok) != n*2 {
			t.Errorf("token(%d): got len %d, want %d", n, len(tok), n*2)
		}
		if _, err := hex.DecodeString(tok); err != nil {
			t.Errorf("token(%d) = %q is not valid hex: %v", n, tok, err)
		}
	}
}

func TestTokenUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		tok := token(8)
		if seen[tok] {
			t.Fatalf("token collision detected after %d iterations", i)
		}
		seen[tok] = true
	}
}

// ── getenv ────────────────────────────────────────────────────────────────────

func TestGetenv(t *testing.T) {
	const key = "BWTEST_UNIT_TEST_VAR"
	os.Unsetenv(key)

	// fallback when env not set
	if got := getenv(key, "default"); got != "default" {
		t.Errorf("getenv(%q, %q) = %q, want %q", key, "default", got, "default")
	}

	// uses env value when set
	os.Setenv(key, "custom")
	defer os.Unsetenv(key)
	if got := getenv(key, "default"); got != "custom" {
		t.Errorf("getenv(%q, %q) = %q, want %q", key, "default", got, "custom")
	}
}

// ── hostname ──────────────────────────────────────────────────────────────────

func TestHostname(t *testing.T) {
	h := hostname()
	if h == "" {
		t.Error("hostname() returned empty string")
	}
}

// ── verifySHA256 ──────────────────────────────────────────────────────────────

// sha256Hex is a test helper that computes the hex-encoded SHA256 of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func TestVerifySHA256Pass(t *testing.T) {
	// Write a temporary file with known content.
	tmp, err := os.CreateTemp(t.TempDir(), "bwagent-test-*")
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("hello bwtest")
	if _, err := tmp.Write(content); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	expectedHash := sha256Hex(content)
	binaryName := "bwagent-linux-amd64"

	// Serve a fake SHA256SUMS matching the temp file.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(expectedHash + "  " + binaryName + "\n"))
	}))
	defer srv.Close()

	if err := verifySHA256(tmp.Name(), srv.URL+"/SHA256SUMS", binaryName, srv.Client()); err != nil {
		t.Errorf("verifySHA256 returned error for correct hash: %v", err)
	}
}

func TestVerifySHA256Mismatch(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "bwagent-test-*")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tmp.Write([]byte("real content"))
	tmp.Close()

	binaryName := "bwagent-linux-amd64"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Deliberately wrong hash (64 zeros).
		_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  " + binaryName + "\n"))
	}))
	defer srv.Close()

	err = verifySHA256(tmp.Name(), srv.URL+"/SHA256SUMS", binaryName, srv.Client())
	if err == nil {
		t.Error("verifySHA256 should have returned an error for hash mismatch")
	}
}

func TestVerifySHA256NotFound(t *testing.T) {
	// When SHA256SUMS returns 404, verification should be skipped (nil error).
	tmp, err := os.CreateTemp(t.TempDir(), "bwagent-test-*")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tmp.Write([]byte("content"))
	tmp.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := verifySHA256(tmp.Name(), srv.URL+"/SHA256SUMS", "bwagent-linux-amd64", srv.Client()); err != nil {
		t.Errorf("verifySHA256 should return nil for 404 (graceful fallback), got: %v", err)
	}
}

func TestVerifySHA256BinaryNotInSUMS(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "bwagent-test-*")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tmp.Write([]byte("content"))
	tmp.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// SHA256SUMS exists but doesn't contain our binary name.
		_, _ = w.Write([]byte(strings.Repeat("a", 64) + "  other-binary\n"))
	}))
	defer srv.Close()

	err = verifySHA256(tmp.Name(), srv.URL+"/SHA256SUMS", "bwagent-linux-amd64", srv.Client())
	if err == nil {
		t.Error("verifySHA256 should return error when binary not found in SHA256SUMS")
	}
}

func TestParseSSHSuccessEvent(t *testing.T) {
	entry := sshLogEntry{
		Timestamp: time.Date(2026, 3, 31, 16, 0, 0, 0, time.UTC),
		Message:   "Accepted publickey for root from 203.0.113.8 port 51234 ssh2: RSA SHA256:test",
	}

	ev, ok := parseSSHSuccessEvent(entry)
	if !ok {
		t.Fatal("parseSSHSuccessEvent should parse accepted login lines")
	}
	if ev.IP != "203.0.113.8" {
		t.Fatalf("expected IP 203.0.113.8, got %q", ev.IP)
	}
	if ev.User != "root" {
		t.Fatalf("expected user root, got %q", ev.User)
	}
	if ev.Method != "publickey" {
		t.Fatalf("expected method publickey, got %q", ev.Method)
	}
}

func TestIsSSHFailedAttemptMessage(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"Failed password for invalid user test from 198.51.100.7 port 55222 ssh2", true},
		{"Failed none for invalid user admin from 198.51.100.8 port 55222 ssh2", true},
		{"error: maximum authentication attempts exceeded for root from 198.51.100.7 port 55222 ssh2 [preauth]", true},
		{"Accepted password for root from 198.51.100.7 port 55222 ssh2", false},
	}

	for _, tc := range tests {
		if got := isSSHFailedAttemptMessage(tc.msg); got != tc.want {
			t.Fatalf("isSSHFailedAttemptMessage(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// ── loadOrCreateConfig ────────────────────────────────────────────────────────

func TestLoadOrCreateConfigWithBOM(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"

	// Write a JSON config with a UTF-8 BOM prefix (as PowerShell 5 does).
	jsonBody := []byte(`{"server_url":"http://1.2.3.4:8080","name":"test","init_token":"tok","client_id":"aabbccdd","client_token":"1122334455667788"}`)
	bom := []byte{0xEF, 0xBB, 0xBF}
	if err := os.WriteFile(path, append(bom, jsonBody...), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("loadOrCreateConfig failed with BOM-prefixed config: %v", err)
	}
	if cfg.ServerURL != "http://1.2.3.4:8080" {
		t.Errorf("unexpected ServerURL %q", cfg.ServerURL)
	}
	if cfg.Name != "test" {
		t.Errorf("unexpected Name %q", cfg.Name)
	}
}

func TestNormalizeTaskMode(t *testing.T) {
	tests := map[string]string{
		"both":        "traditional",
		"traditional": "traditional",
		"browse":      "browse",
	}
	for in, want := range tests {
		if got := normalizeTaskMode(in); got != want {
			t.Fatalf("normalizeTaskMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSSHLogTimestampSyslog(t *testing.T) {
	now := time.Date(2026, 3, 31, 16, 0, 0, 0, time.Local)
	ts, ok := parseSSHLogTimestamp("Mar 31 15:59:59 host sshd[1234]: Failed password for root from 198.51.100.2 port 22 ssh2", now)
	if !ok {
		t.Fatal("expected syslog timestamp to parse")
	}
	if ts.Year() != 2026 || ts.Month() != time.March || ts.Day() != 31 {
		t.Fatalf("unexpected parsed timestamp: %v", ts)
	}
}
