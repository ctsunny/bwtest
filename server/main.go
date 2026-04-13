package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

var Version = "v0.4.20"
var tagNormalizer = strings.NewReplacer("，", ",", "\n", ",", ";", ",")

type Config struct {
	PanelAddr  string
	DataAddr   string
	ServerHost string
	InitToken  string
	AdminUser  string
	AdminPass  string
	DBPath     string
	PanelPath  string
	BarkURL    string
}

type Client struct {
	ID               string
	Name             string
	Remark           string
	GroupName        string
	Tags             string
	Approved         bool
	LastSeen         string
	RemoteIP         string
	CurrentTask      string
	UpgradeTo        string
	Version          string
	Latency          int
	SSHAttempts      int
	RestartRequested bool
}

type Task struct {
	ID            string
	ClientID      string
	Mode          string
	UpMbps        int
	DownMbps      int
	DurationSec   int
	Density       int // 0=continuous, 30=30% idle, 70=70% idle
	Status        string
	CreatedAt     string
	StartedAt     string
	FinishedAt    string
	ScheduledAt   string
	UploadBytes   int64
	DownloadBytes int64
	Logs          string
	MaxRetries    int
	RetryCount    int
	ParentTaskID  string
	TemplateName  string
	// Peer-transfer fields
	PeerClientID string // ID of the other client in a peer task
	PeerRole     string // "source" (listen) or "target" (connect)
	PeerAddr     string // host:port reported by source; filled in for target before dispatch
}

type TaskTemplate struct {
	ID          string
	Name        string
	Mode        string
	UpMbps      int
	DownMbps    int
	DurationSec int
	Density     int
	MaxRetries  int
	CreatedAt   string
}

type RegisterReq struct {
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
	Name        string `json:"name"`
	InitToken   string `json:"init_token"`
	Version     string `json:"version"`
}

type HeartbeatReq struct {
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
	Version     string `json:"version"`
	Latency     int    `json:"latency"`
	SSHAttempts int    `json:"ssh_attempts"`
}

type ResultReq struct {
	ClientID      string `json:"client_id"`
	ClientToken   string `json:"client_token"`
	TaskID        string `json:"task_id"`
	Status        string `json:"status"`
	UploadBytes   int64  `json:"upload_bytes"`
	DownloadBytes int64  `json:"download_bytes"`
	Logs          string `json:"logs"`
}

type ProgressReq struct {
	ClientID      string `json:"client_id"`
	ClientToken   string `json:"client_token"`
	TaskID        string `json:"task_id"`
	UploadBytes   int64  `json:"upload_bytes"`
	DownloadBytes int64  `json:"download_bytes"`
	Logs          string `json:"logs"`
}

type AssignResp struct {
	ID          string `json:"id"`
	Mode        string `json:"mode"`
	UpMbps      int    `json:"up_mbps"`
	DownMbps    int    `json:"down_mbps"`
	DurationSec int    `json:"duration_sec"`
	Density     int    `json:"density"`
	DataAddr    string `json:"data_addr"`
	// Peer-transfer fields
	PeerRole string `json:"peer_role,omitempty"` // "source" or "target"
	PeerAddr string `json:"peer_addr,omitempty"` // for target: address of source to connect
}

type ControlResp struct {
	Status string `json:"status"`
}

type SSHLoginReq struct {
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
	LoginIP     string `json:"login_ip"`
	LoginAt     string `json:"login_at,omitempty"`
	Username    string `json:"username,omitempty"`
	Method      string `json:"method,omitempty"`
}

type DataHello struct {
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
	TaskID      string `json:"task_id"`
	Mode        string `json:"mode"`
	RateMbps    int    `json:"rate_mbps,omitempty"`
	DurationSec int    `json:"duration_sec"`
}

var taskModeLabels = map[string]string{
	"upload":      "上传",
	"download":    "下载",
	"both":        "传统",
	"traditional": "传统",
	"browse":      "网页浏览",
	"stream":      "流媒体",
	"backup":      "备份同步",
	"peer":        "对传",
}

func normalizeTaskMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "both":
		return "traditional"
	default:
		return strings.TrimSpace(mode)
	}
}

func isValidTaskMode(mode string) bool {
	_, ok := taskModeLabels[normalizeTaskMode(mode)]
	return ok
}

func taskModeLabel(mode string) string {
	if label, ok := taskModeLabels[normalizeTaskMode(mode)]; ok {
		return label
	}
	return mode
}

func densityLabel(d int) string {
	switch d {
	case 30:
		return "30% 间歇"
	case 70:
		return "70% 间歇"
	default:
		return "无间歇"
	}
}

// ── SSE Broker ────────────────────────────────────────────────────────────────

type Broker struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func NewBroker() *Broker {
	return &Broker{subs: map[chan string]struct{}{}}
}

func (b *Broker) Subscribe() chan string {
	ch := make(chan string, 8)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) Unsubscribe(ch chan string) {
	b.mu.Lock()
	delete(b.subs, ch)
	close(ch)
	b.mu.Unlock()
}

func (b *Broker) Publish(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// ── Rate Limiter ──────────────────────────────────────────────────────────────

// ipRateLimiter implements a simple sliding-window per-IP rate limiter.
type ipRateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newIPRateLimiter() *ipRateLimiter {
	rl := &ipRateLimiter{hits: make(map[string][]time.Time)}
	go rl.cleanup()
	return rl
}

// cleanup removes stale entries every 5 minutes to prevent unbounded growth.
func (rl *ipRateLimiter) cleanup() {
	tk := time.NewTicker(5 * time.Minute)
	defer tk.Stop()
	for range tk.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-time.Minute)
		for ip, ts := range rl.hits {
			var keep []time.Time
			for _, t := range ts {
				if t.After(cutoff) {
					keep = append(keep, t)
				}
			}
			if len(keep) == 0 {
				delete(rl.hits, ip)
			} else {
				rl.hits[ip] = keep
			}
		}
		rl.mu.Unlock()
	}
}

// Allow returns true if the IP has not exceeded maxPerMin requests in the last minute.
func (rl *ipRateLimiter) Allow(ip string, maxPerMin int) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	ts := rl.hits[ip]
	var valid []time.Time
	for _, t := range ts {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= maxPerMin {
		rl.hits[ip] = valid
		return false
	}
	rl.hits[ip] = append(valid, now)
	return true
}

// registerLimiter is used to rate-limit client registration requests.
var registerLimiter = newIPRateLimiter()

// ── Bark Push Helpers ─────────────────────────────────────────────────────────

func barkPush(barkURL, title, body string) {
	if barkURL == "" {
		return
	}
	base := normalizeBarkURL(barkURL)
	if base == "" {
		return
	}
	pushURL := fmt.Sprintf("%s/%s/%s", base, url.PathEscape(title), url.PathEscape(body))
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(pushURL)
	if err != nil {
		log.Printf("bark push error: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Printf("bark push error: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

func loadBarkURL(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func barkURLFromToken(token string) string {
	token = strings.TrimSpace(token)
	token = strings.Trim(token, "/")
	if token == "" {
		return ""
	}
	if strings.HasPrefix(token, "http://") || strings.HasPrefix(token, "https://") {
		return normalizeBarkURL(token)
	}
	return "https://api.day.app/" + token
}

func barkTokenFromURL(raw string) string {
	raw = normalizeBarkURL(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "https://api.day.app/") {
		return strings.TrimPrefix(raw, "https://api.day.app/")
	}
	if strings.HasPrefix(raw, "http://api.day.app/") {
		return strings.TrimPrefix(raw, "http://api.day.app/")
	}
	return raw
}

func normalizeBarkURL(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, "/")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	if !strings.EqualFold(u.Host, "api.day.app") {
		return strings.TrimRight(raw, "/")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return raw
	}
	u.Path = "/" + parts[0]
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func main() {
	cfg := Config{
		PanelAddr:  getenv("PANEL_ADDR", ":8080"),
		DataAddr:   getenv("DATA_ADDR", ":9000"),
		ServerHost: getenv("SERVER_HOST", "127.0.0.1"),
		InitToken:  getenv("INIT_TOKEN", genToken(16)),
		AdminUser:  getenv("ADMIN_USER", "admin"),
		AdminPass:  getenv("ADMIN_PASS", "admin123456"),
		DBPath:     getenv("DB_PATH", "/opt/bwtest/bwtest.db"),
		PanelPath:  strings.TrimRight(getenv("PANEL_PATH", "/admin"), "/"),
		BarkURL:    getenv("BARK_URL", ""),
	}
	if cfg.PanelPath == "" {
		cfg.PanelPath = "/admin"
	}
	if saved := loadBarkURL("/opt/bwtest/bark_url"); saved != "" {
		cfg.BarkURL = normalizeBarkURL(saved)
	}
	cfg.BarkURL = normalizeBarkURL(cfg.BarkURL)

	// Warn if the default weak password is still in use.
	if cfg.AdminPass == "admin123456" || cfg.AdminPass == "admin" {
		log.Printf("⚠️  [SECURITY] 管理员密码使用了默认值 %q！请通过环境变量 ADMIN_PASS 设置强密码。", cfg.AdminPass)
	}

	db := mustInitDB(cfg.DBPath)
	broker := NewBroker()

	log.Printf("bwtest server %s starting...", Version)
	log.Printf("panel=%s%s data=%s db=%s bark=%v",
		cfg.PanelAddr, cfg.PanelPath, cfg.DataAddr, cfg.DBPath, cfg.BarkURL != "")

	go runDataServer(cfg, db)
	go watchStuckTasks(db, broker)
	go watchScheduledTasks(db, broker)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", jsonHandler(handleRegister(&cfg, db, broker)))
	mux.HandleFunc("/api/heartbeat", jsonHandler(handleHeartbeat(db)))
	mux.HandleFunc("/api/ssh/login", jsonHandler(handleSSHLogin(&cfg, db)))
	mux.HandleFunc("/api/task/next", jsonHandler(handleNextTask(cfg, db, broker)))
	mux.HandleFunc("/api/task/result", jsonHandler(handleTaskResult(&cfg, db, broker)))
	mux.HandleFunc("/api/task/control", jsonHandler(handleTaskControl(db)))
	mux.HandleFunc("/api/task/progress", jsonHandler(handleTaskProgress(db)))
	mux.HandleFunc("/api/peer/addr", jsonHandler(handlePeerAddr(db, broker)))
	mux.HandleFunc("/api/data", jsonHandler(handleAPIData(db)))

	p := cfg.PanelPath
	mux.Handle(p, basicAuth(cfg, http.HandlerFunc(handleAdmin(&cfg, db))))
	mux.Handle(p+"/approve", basicAuth(cfg, http.HandlerFunc(handleApprove(p, cfg, db, broker))))
	mux.Handle(p+"/client/edit", basicAuth(cfg, http.HandlerFunc(handleClientEdit(p, db, broker))))
	mux.Handle(p+"/client/upgrade", basicAuth(cfg, http.HandlerFunc(handlePushUpgrade(p, db))))
	mux.Handle(p+"/client/delete", basicAuth(cfg, http.HandlerFunc(handleDeleteClient(p, db, broker))))
	mux.Handle(p+"/client/bulk-delete", basicAuth(cfg, http.HandlerFunc(handleBulkDeleteClients(p, db, broker))))
	mux.Handle(p+"/client/bulk-restart", basicAuth(cfg, http.HandlerFunc(handleBulkRestartClients(p, db, broker))))
	mux.Handle(p+"/task/create", basicAuth(cfg, http.HandlerFunc(handleCreateTask(p, &cfg, db, broker))))
	mux.Handle(p+"/task/peer-create", basicAuth(cfg, http.HandlerFunc(handleCreatePeerTask(p, &cfg, db, broker))))
	mux.Handle(p+"/task/stop", basicAuth(cfg, http.HandlerFunc(handleStopTask(p, db, broker))))
	mux.Handle(p+"/task/stop-all", basicAuth(cfg, http.HandlerFunc(handleStopAllTasks(p, db, broker))))
	mux.Handle(p+"/task/delete", basicAuth(cfg, http.HandlerFunc(handleDeleteTask(p, db, broker))))
	mux.Handle(p+"/task/template/save", basicAuth(cfg, http.HandlerFunc(handleSaveTaskTemplate(p, db))))
	mux.Handle(p+"/task/logs", basicAuth(cfg, http.HandlerFunc(handleGetTaskLogs(db))))
	mux.Handle(p+"/task/clear-history", basicAuth(cfg, http.HandlerFunc(handleClearHistory(p, db, broker))))
	mux.Handle(p+"/gen/install-cmd", basicAuth(cfg, http.HandlerFunc(handleGenInstallCmd(p, &cfg))))
	mux.Handle(p+"/settings", basicAuth(cfg, http.HandlerFunc(handleSettings(p, &cfg))))
	mux.Handle(p+"/events", basicAuth(cfg, http.HandlerFunc(handleEvents(broker))))
	mux.Handle(p+"/restart", basicAuth(cfg, http.HandlerFunc(handleRestart(p))))
	mux.Handle(p+"/server", basicAuth(cfg, http.HandlerFunc(handleServerPage(p, &cfg))))

	if p != "/admin" {
		mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, p, http.StatusFound)
		})
	}

	srv := &http.Server{
		Addr:    cfg.PanelAddr,
		Handler: mux,
	}

	// Graceful shutdown: drain existing connections on SIGINT / SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-quit
		log.Printf("received signal %v, shutting down gracefully (max 30s)...", s)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
	}()

	log.Printf("panel listening on %s", cfg.PanelAddr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Printf("server shutdown complete")
}

func resetStuckTasks(db *sql.DB) {
	// deprecated: running tasks should be preserved to allow auto-reconnect
}

func watchStuckTasks(db *sql.DB, broker *Broker) {
	tk := time.NewTicker(30 * time.Second)
	defer tk.Stop()
	for range tk.C {
		threshold := time.Now().Add(-300 * time.Second).Format(time.RFC3339)
		rows, err := db.Query(`
			SELECT t.id, t.client_id FROM tasks t
			JOIN clients c ON c.id = t.client_id
			WHERE t.status = 'running'
			AND c.last_seen < ?`, threshold)
		if err != nil {
			continue
		}
		type pair struct{ tid, cid string }
		var pairs []pair
		for rows.Next() {
			var p pair
			_ = rows.Scan(&p.tid, &p.cid)
			pairs = append(pairs, p)
		}
		rows.Close()
		for _, p := range pairs {
			now := time.Now().Format(time.RFC3339)
			_, _ = db.Exec(`UPDATE tasks SET status='stopped', finished_at=?, started_at=COALESCE(NULLIF(started_at,''), created_at) WHERE id=?`, now, p.tid)
			_, _ = db.Exec(`UPDATE clients SET current_task='' WHERE id=?`, p.cid)
			maybeRetryTask(db, p.tid, now)
			log.Printf("watchStuckTasks: mark task %s (client %s) stopped due to heartbeat timeout", p.tid, p.cid)
		}
		if len(pairs) > 0 {
			broker.Publish("tasks")
		}
	}
}

func watchScheduledTasks(db *sql.DB, broker *Broker) {
	tk := time.NewTicker(5 * time.Second)
	defer tk.Stop()
	for range tk.C {
		now := time.Now().Format(time.RFC3339)
		res, err := db.Exec(`UPDATE tasks SET status='pending' WHERE status='scheduled' AND scheduled_at<>'' AND scheduled_at<=?`, now)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			broker.Publish("tasks")
		}
	}
}

func maybeRetryTask(db *sql.DB, taskID, now string) bool {
	var t Task
	err := db.QueryRow(`SELECT id, client_id, mode, up_mbps, down_mbps, duration_sec, density, max_retries, retry_count, parent_task_id, template_name FROM tasks WHERE id=?`, taskID).
		Scan(&t.ID, &t.ClientID, &t.Mode, &t.UpMbps, &t.DownMbps, &t.DurationSec, &t.Density, &t.MaxRetries, &t.RetryCount, &t.ParentTaskID, &t.TemplateName)
	if err != nil || t.MaxRetries <= t.RetryCount {
		return false
	}
	newID := genToken(8)
	parentID := t.ParentTaskID
	if parentID == "" {
		parentID = t.ID
	}
	_, err = db.Exec(`INSERT INTO tasks(id,client_id,mode,up_mbps,down_mbps,duration_sec,density,status,created_at,scheduled_at,max_retries,retry_count,parent_task_id,template_name) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		newID, t.ClientID, t.Mode, t.UpMbps, t.DownMbps, t.DurationSec, t.Density, "pending", now, "", t.MaxRetries, t.RetryCount+1, parentID, t.TemplateName)
	return err == nil
}

// ── Database / Schema ─────────────────────────────────────────────────────────

func mustInitDB(path string) *sql.DB {
	dir := filepath.Dir(path)
	_ = os.MkdirAll(dir, 0755)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		log.Fatal(err)
	}
	stmts := []string{
		`PRAGMA journal_mode = WAL;`,
		`CREATE TABLE IF NOT EXISTS clients(
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			remark TEXT NOT NULL DEFAULT '',
			token TEXT NOT NULL,
			approved INTEGER NOT NULL DEFAULT 0,
			last_seen TEXT NOT NULL,
			remote_ip TEXT NOT NULL DEFAULT '',
			current_task TEXT NOT NULL DEFAULT ''
		);`,
		`ALTER TABLE clients ADD COLUMN remark TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE clients ADD COLUMN upgrade_to TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE clients ADD COLUMN version TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE clients ADD COLUMN latency INTEGER NOT NULL DEFAULT 0;`,
		`ALTER TABLE clients ADD COLUMN ssh_attempts INTEGER NOT NULL DEFAULT 0;`,
		`ALTER TABLE clients ADD COLUMN group_name TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE clients ADD COLUMN tags TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE clients ADD COLUMN restart_requested INTEGER NOT NULL DEFAULT 0;`,
		`CREATE TABLE IF NOT EXISTS tasks(
			id TEXT PRIMARY KEY,
			client_id TEXT NOT NULL,
			mode TEXT NOT NULL,
			up_mbps INTEGER NOT NULL,
			down_mbps INTEGER NOT NULL,
			duration_sec INTEGER NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			started_at TEXT NOT NULL DEFAULT '',
			finished_at TEXT NOT NULL DEFAULT '',
			upload_bytes INTEGER NOT NULL DEFAULT 0,
			download_bytes INTEGER NOT NULL DEFAULT 0,
			logs TEXT NOT NULL DEFAULT ''
		);`,
		`ALTER TABLE tasks ADD COLUMN logs TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE tasks ADD COLUMN density INTEGER NOT NULL DEFAULT 0;`,
		`ALTER TABLE tasks ADD COLUMN scheduled_at TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE tasks ADD COLUMN max_retries INTEGER NOT NULL DEFAULT 0;`,
		`ALTER TABLE tasks ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0;`,
		`ALTER TABLE tasks ADD COLUMN parent_task_id TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE tasks ADD COLUMN template_name TEXT NOT NULL DEFAULT '';`,
		// Peer-transfer columns
		`ALTER TABLE tasks ADD COLUMN peer_client_id TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE tasks ADD COLUMN peer_role TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE tasks ADD COLUMN peer_addr TEXT NOT NULL DEFAULT '';`,
		`CREATE TABLE IF NOT EXISTS task_templates(
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			mode TEXT NOT NULL,
			up_mbps INTEGER NOT NULL,
			down_mbps INTEGER NOT NULL,
			duration_sec INTEGER NOT NULL,
			density INTEGER NOT NULL DEFAULT 0,
			max_retries INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		);`,
		// Indexes for common query patterns.
		`CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_client_id ON tasks(client_id);`,
		`CREATE INDEX IF NOT EXISTS idx_clients_approved ON clients(approved);`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				log.Printf("db init warn: %v", err)
			}
		}
	}
	return db
}

// ── Data Server (TCP) ─────────────────────────────────────────────────────────

func runDataServer(cfg Config, db *sql.DB) {
	ln, err := net.Listen("tcp", cfg.DataAddr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := ln.Accept()
		if err == nil {
			go handleDataConn(db, conn)
		}
	}
}

func handleDataConn(db *sql.DB, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	var line []byte
	var err error
	for i := 0; i < 20; i++ { // Limit to 20 lines to prevent DoS
		line, err = br.ReadBytes('\n')
		if err != nil {
			return
		}
		if len(line) > 0 && line[0] == '{' {
			break // Found JSON payload
		}
	}
	if len(line) == 0 || line[0] != '{' {
		return // JSON not found
	}

	var hello DataHello
	if err := json.Unmarshal(line, &hello); err != nil {
		return
	}
	var token, status, mode string
	var downMbps, duration int
	err = db.QueryRow(`
		SELECT c.token, t.status, t.mode, t.down_mbps, t.duration_sec
		FROM clients c
		JOIN tasks t ON c.id=t.client_id
		WHERE c.id=? AND t.id=?`,
		hello.ClientID, hello.TaskID,
	).Scan(&token, &status, &mode, &downMbps, &duration)
	if err != nil || token != hello.ClientToken || status != "running" {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	deadline := time.Now().Add(time.Duration(hello.DurationSec) * time.Second)
	effMode := hello.Mode
	if effMode == "" {
		effMode = mode
	}
	effDownMbps := downMbps
	if hello.RateMbps > 0 && hello.RateMbps < effDownMbps {
		effDownMbps = hello.RateMbps
	}
	switch effMode {
	case "upload":
		readDiscardLoop(conn, deadline, func() bool { return taskStatus(db, hello.TaskID) == "running" })
	case "download":
		_ = pacedWrite(conn, effDownMbps, deadline, func() bool { return taskStatus(db, hello.TaskID) == "running" })
	case "both", "traditional":
		done := make(chan struct{})
		go func() {
			readDiscardLoop(conn, deadline, func() bool { return taskStatus(db, hello.TaskID) == "running" })
			close(done)
		}()
		_ = pacedWrite(conn, effDownMbps, deadline, func() bool { return taskStatus(db, hello.TaskID) == "running" })
		<-done
	}
}

func readDiscardLoop(conn net.Conn, deadline time.Time, keep func() bool) {
	buf := make([]byte, 64*1024)
	for time.Now().Before(deadline) && keep() {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := conn.Read(buf)
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			continue
		}
		if err != nil {
			return
		}
	}
}

func pacedWrite(w io.Writer, mbps int, deadline time.Time, keep func() bool) error {
	if mbps <= 0 {
		mbps = 1
	}
	bytesPerSec := int64(mbps) * 1024 * 1024 / 8
	buf := make([]byte, 32*1024)
	_, _ = rand.Read(buf)

	// Fake HTTP Response Header for Obfuscation
	httpHeader := []byte("HTTP/1.1 200 OK\r\nContent-Type: video/mp4\r\nServer: nginx/1.24.0\r\nConnection: keep-alive\r\n\r\n")
	_, _ = w.Write(httpHeader)

	// Jitter / Human-like burst algorithm (Moderate sleep for visible peaks without huge spikes)
	for time.Now().Before(deadline) && keep() {
		sleepMs := mrand.Intn(1000) + 500 // Sleep 500ms ~ 1.5s
		time.Sleep(time.Duration(sleepMs) * time.Millisecond)

		if !keep() || time.Now().After(deadline) {
			break
		}

		baseChunk := bytesPerSec * int64(sleepMs) / 1000
		jitter := baseChunk / 10 // 10% jitter
		if jitter <= 0 {
			jitter = 1
		}

		finalChunk := baseChunk
		if mrand.Intn(2) == 0 {
			finalChunk += int64(mrand.Intn(int(jitter)))
		} else {
			finalChunk -= int64(mrand.Intn(int(jitter)))
		}

		left := finalChunk
		for left > 0 && keep() {
			n := int64(len(buf))
			if n > left {
				n = left
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			left -= n
		}
	}
	return nil
}

// ── API Handlers (client-facing) ──────────────────────────────────────────────

func handleRegister(cfg *Config, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Rate-limit registration attempts: max 20 per IP per minute.
		ip := realIP(r)
		if !registerLimiter.Allow(ip, 20) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}

		var req RegisterReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if req.ClientID == "" || req.ClientToken == "" || req.Name == "" {
			http.Error(w, "bad request", 400)
			return
		}
		now := time.Now().Format(time.RFC3339)
		var token string
		err := db.QueryRow(`SELECT token FROM clients WHERE id=?`, req.ClientID).Scan(&token)
		if err == sql.ErrNoRows {
			if req.InitToken != cfg.InitToken {
				http.Error(w, "bad init token", 401)
				return
			}
			_, err = db.Exec(`INSERT INTO clients(id,name,remark,token,approved,last_seen,remote_ip,current_task,version) VALUES(?,?,?,?,?,?,?,?,?)`,
				req.ClientID, req.Name, "", req.ClientToken, 0, now, realIP(r), "", req.Version)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			broker.Publish("clients")
			go barkPush(cfg.BarkURL, "新客户端申请",
				fmt.Sprintf("客户端 %s (%s) 申请接入，请到面板批准。", req.Name, realIP(r)))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "approved": false})
			return
		}
		if err != nil || token != req.ClientToken {
			http.Error(w, "unauthorized", 401)
			return
		}
		_, _ = db.Exec(`UPDATE clients SET last_seen=?, remote_ip=?, name=? WHERE id=?`,
			now, realIP(r), req.Name, req.ClientID)
		var approved int
		_ = db.QueryRow(`SELECT approved FROM clients WHERE id=?`, req.ClientID).Scan(&approved)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "approved": approved == 1})
	}
}

func handleHeartbeat(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req HeartbeatReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var token string
		if err := db.QueryRow(`SELECT token FROM clients WHERE id=?`, req.ClientID).Scan(&token); err != nil || token != req.ClientToken {
			http.Error(w, "unauthorized", 401)
			return
		}
		// 读取 upgrade_to，然后原子清除，保证只下发一次
		var upgradeTo string
		var restartRequested int
		_ = db.QueryRow(`SELECT upgrade_to,restart_requested FROM clients WHERE id=?`, req.ClientID).Scan(&upgradeTo, &restartRequested)
		if upgradeTo != "" {
			_, _ = db.Exec(`UPDATE clients SET upgrade_to='' WHERE id=?`, req.ClientID)
		}
		if restartRequested == 1 {
			_, _ = db.Exec(`UPDATE clients SET restart_requested=0 WHERE id=?`, req.ClientID)
		}
		_, _ = db.Exec(`UPDATE clients SET last_seen=?, remote_ip=?, version=?, latency=?, ssh_attempts=? WHERE id=?`,
			time.Now().Format(time.RFC3339), realIP(r), req.Version, req.Latency, req.SSHAttempts, req.ClientID)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upgrade_to": upgradeTo, "restart_requested": restartRequested == 1})
	}
}

func handleSSHLogin(cfg *Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req SSHLoginReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		loginIP := strings.TrimSpace(req.LoginIP)
		if loginIP == "" || len(loginIP) > 128 {
			http.Error(w, "bad login ip", http.StatusBadRequest)
			return
		}

		var token, clientName string
		if err := db.QueryRow(`SELECT token, name FROM clients WHERE id=?`, req.ClientID).Scan(&token, &clientName); err != nil || token != req.ClientToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		body := fmt.Sprintf("客户端 %s 检测到 SSH 登录成功\n登录 IP: %s", clientName, loginIP)
		if user := strings.TrimSpace(req.Username); user != "" {
			body += "\n用户: " + user
		}
		if method := strings.TrimSpace(req.Method); method != "" {
			body += "\n方式: " + method
		}
		if loginAt := strings.TrimSpace(req.LoginAt); loginAt != "" {
			body += "\n时间: " + loginAt
		}
		go barkPush(cfg.BarkURL, "SSH 登录提醒", body)

		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func handleNextTask(cfg Config, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID := r.URL.Query().Get("client_id")
		clientToken := r.URL.Query().Get("client_token")
		var token string
		var approved int
		var currentTask string
		err := db.QueryRow(`SELECT token,approved,current_task FROM clients WHERE id=?`, clientID).
			Scan(&token, &approved, &currentTask)
		if err != nil || token != clientToken || approved != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		if currentTask != "" {
			if taskStatus(db, currentTask) == "running" {
				var rt Task
				err = db.QueryRow(`
					SELECT id,client_id,mode,up_mbps,down_mbps,duration_sec,density,started_at,peer_role,peer_addr
					FROM tasks WHERE id=?`, currentTask).
					Scan(&rt.ID, &rt.ClientID, &rt.Mode, &rt.UpMbps, &rt.DownMbps, &rt.DurationSec, &rt.Density, &rt.StartedAt, &rt.PeerRole, &rt.PeerAddr)
				if err == nil && rt.StartedAt != "" {
					started, _ := time.Parse(time.RFC3339, rt.StartedAt)
					elapsed := int(time.Since(started).Seconds())
					if elapsed < rt.DurationSec {
						addr := cfg.DataAddr
						if strings.HasPrefix(addr, ":") {
							addr = cfg.ServerHost + addr
						}
						_ = json.NewEncoder(w).Encode(AssignResp{
							ID: rt.ID, Mode: rt.Mode, UpMbps: rt.UpMbps, DownMbps: rt.DownMbps,
							DurationSec: rt.DurationSec - elapsed, Density: rt.Density, DataAddr: addr,
							PeerRole: rt.PeerRole, PeerAddr: rt.PeerAddr,
						})
						return
					}
					// 运行时长已超出 duration_sec，标记为 done 避免任务卡死在 running 状态
					now := time.Now().Format(time.RFC3339)
					if _, dbErr := db.Exec(`UPDATE tasks SET status='done', finished_at=? WHERE id=? AND status='running'`, now, currentTask); dbErr != nil {
						log.Printf("handleNextTask: failed to mark expired task %s as done: %v", currentTask, dbErr)
					} else {
						broker.Publish("tasks")
					}
				}
			}
			// current_task 指向非 running 的旧任务或已超时，先清空脏数据
			_, _ = db.Exec(`UPDATE clients SET current_task='' WHERE id=?`, clientID)
		}
		var t Task
		// For peer target tasks: only dispatch when source has already reported its address.
		err = db.QueryRow(`
			SELECT id,client_id,mode,up_mbps,down_mbps,duration_sec,density,status,created_at,peer_role,peer_addr,peer_client_id
			FROM tasks WHERE client_id=? AND status='pending'
			AND (mode != 'peer' OR peer_role = 'source' OR (peer_role = 'target' AND peer_addr != ''))
			ORDER BY created_at ASC LIMIT 1`, clientID).
			Scan(&t.ID, &t.ClientID, &t.Mode, &t.UpMbps, &t.DownMbps, &t.DurationSec, &t.Density, &t.Status, &t.CreatedAt, &t.PeerRole, &t.PeerAddr, &t.PeerClientID)
		if err == sql.ErrNoRows {
			w.WriteHeader(204)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		now := time.Now().Format(time.RFC3339)
		_, _ = db.Exec(`UPDATE tasks SET status='running', started_at=? WHERE id=?`, now, t.ID)
		_, _ = db.Exec(`UPDATE clients SET current_task=? WHERE id=?`, t.ID, clientID)
		broker.Publish("tasks")
		clientName := clientID
		_ = db.QueryRow(`SELECT name FROM clients WHERE id=?`, clientID).Scan(&clientName)
		go barkPush(cfg.BarkURL, "任务开始",
			fmt.Sprintf("客户端 %s 开始执行任务\n模式:%s 上传:%dMbps 下载:%dMbps 时长:%ds",
				clientName, taskModeLabel(t.Mode), t.UpMbps, t.DownMbps, t.DurationSec))
		addr := cfg.DataAddr
		if strings.HasPrefix(addr, ":") {
			addr = cfg.ServerHost + addr
		}
		_ = json.NewEncoder(w).Encode(AssignResp{
			ID: t.ID, Mode: t.Mode, UpMbps: t.UpMbps, DownMbps: t.DownMbps,
			DurationSec: t.DurationSec, Density: t.Density, DataAddr: addr,
			PeerRole: t.PeerRole, PeerAddr: t.PeerAddr,
		})
	}
}

func handleTaskResult(cfg *Config, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ResultReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var token, status, clientID string
		err := db.QueryRow(`
			SELECT c.token, t.status, t.client_id
			FROM clients c JOIN tasks t ON c.id=t.client_id
			WHERE c.id=? AND t.id=?`, req.ClientID, req.TaskID).
			Scan(&token, &status, &clientID)
		if err != nil || token != req.ClientToken || clientID != req.ClientID {
			http.Error(w, "unauthorized", 401)
			return
		}
		finalStatus := req.Status
		if finalStatus == "" {
			finalStatus = "done"
		}
		// 服务端已经强制终止的任务，保留服务端状态，不被客户端上报覆盖
		if status == "stopped" || status == "done" {
			finalStatus = status
		} else if status == "stopping" {
			finalStatus = "stopped"
		}
		now := time.Now().Format(time.RFC3339)
		_, _ = db.Exec(`UPDATE tasks SET status=?, finished_at=?, upload_bytes=?, download_bytes=?, logs=? WHERE id=?`,
			finalStatus, now, req.UploadBytes, req.DownloadBytes, req.Logs, req.TaskID)
		_, _ = db.Exec(`UPDATE clients SET current_task='' WHERE id=?`, req.ClientID)
		if status == "running" && finalStatus == "stopped" {
			_ = maybeRetryTask(db, req.TaskID, now)
		}
		broker.Publish("tasks")
		clientName := req.ClientID
		_ = db.QueryRow(`SELECT name FROM clients WHERE id=?`, req.ClientID).Scan(&clientName)
		upGB := float64(req.UploadBytes) / (1024 * 1024 * 1024)
		dnGB := float64(req.DownloadBytes) / (1024 * 1024 * 1024)
		var barkTitle string
		switch finalStatus {
		case "done":
			barkTitle = "任务完成"
		case "stopped":
			barkTitle = "任务已停止"
		default:
			barkTitle = "任务中断"
		}
		go barkPush(cfg.BarkURL, barkTitle,
			fmt.Sprintf("客户端 %s 任务结束 [%s]\n上传:%.3fGB 下载:%.3fGB",
				clientName, finalStatus, upGB, dnGB))
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func handleTaskControl(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID := r.URL.Query().Get("client_id")
		clientToken := r.URL.Query().Get("client_token")
		taskID := r.URL.Query().Get("task_id")
		var token, status string
		err := db.QueryRow(`
			SELECT c.token, t.status FROM clients c
			JOIN tasks t ON c.id=t.client_id WHERE c.id=? AND t.id=?`,
			clientID, taskID).Scan(&token, &status)
		if err != nil || token != clientToken {
			http.Error(w, "unauthorized", 401)
			return
		}
		_ = json.NewEncoder(w).Encode(ControlResp{Status: status})
	}
}

func handleTaskProgress(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ProgressReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var token, status string
		err := db.QueryRow(`
			SELECT c.token, t.status FROM clients c
			JOIN tasks t ON c.id=t.client_id WHERE c.id=? AND t.id=?`,
			req.ClientID, req.TaskID).Scan(&token, &status)
		if err != nil || token != req.ClientToken {
			http.Error(w, "unauthorized", 401)
			return
		}
		if status != "running" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false})
			return
		}
		_, _ = db.Exec(`UPDATE tasks SET upload_bytes=?, download_bytes=?, logs=? WHERE id=?`,
			req.UploadBytes, req.DownloadBytes, req.Logs, req.TaskID)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func handleAPIData(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clients := mustClients(db)
		tasks := mustTasks(db)
		type clientJSON struct {
			ID               string `json:"id"`
			Name             string `json:"name"`
			Remark           string `json:"remark"`
			GroupName        string `json:"group_name"`
			Tags             string `json:"tags"`
			Approved         bool   `json:"approved"`
			LastSeen         string `json:"last_seen"`
			RemoteIP         string `json:"remote_ip"`
			CurrentTask      string `json:"current_task"`
			Latency          int    `json:"latency"`
			SSHAttempts      int    `json:"ssh_attempts"`
			RestartRequested bool   `json:"restart_requested"`
		}
		type taskJSON struct {
			ID            string  `json:"id"`
			ClientID      string  `json:"client_id"`
			ClientName    string  `json:"client_name"`
			Mode          string  `json:"mode"`
			UpMbps        int     `json:"up_mbps"`
			DownMbps      int     `json:"down_mbps"`
			DurationSec   int     `json:"duration_sec"`
			Density       int     `json:"density"`
			Status        string  `json:"status"`
			StartedAt     string  `json:"started_at"`
			FinishedAt    string  `json:"finished_at"`
			ScheduledAt   string  `json:"scheduled_at"`
			UploadGB      float64 `json:"upload_gb"`
			DownloadGB    float64 `json:"download_gb"`
			UploadBytes   int64   `json:"upload_bytes"`
			DownloadBytes int64   `json:"download_bytes"`
			MaxRetries    int     `json:"max_retries"`
			RetryCount    int     `json:"retry_count"`
			ParentTaskID  string  `json:"parent_task_id"`
			TemplateName  string  `json:"template_name"`
			PeerClientID  string  `json:"peer_client_id,omitempty"`
			PeerRole      string  `json:"peer_role,omitempty"`
			PeerClientName string `json:"peer_client_name,omitempty"`
		}
		statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
		modeFilter := normalizeTaskMode(strings.TrimSpace(r.URL.Query().Get("mode")))
		clientFilter := strings.TrimSpace(r.URL.Query().Get("client_id"))
		search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		clientNames := map[string]string{}
		for _, c := range clients {
			clientNames[c.ID] = c.Name
		}
		cj := make([]clientJSON, 0, len(clients))
		for _, c := range clients {
			cj = append(cj, clientJSON{
				ID: c.ID, Name: c.Name, Remark: c.Remark, GroupName: c.GroupName, Tags: c.Tags,
				Approved: c.Approved, LastSeen: c.LastSeen,
				RemoteIP: c.RemoteIP, CurrentTask: c.CurrentTask,
				Latency: c.Latency, SSHAttempts: c.SSHAttempts, RestartRequested: c.RestartRequested,
			})
		}
		tj := make([]taskJSON, 0, len(tasks))
		for _, t := range tasks {
			clientName := clientNames[t.ClientID]
			if statusFilter != "" && t.Status != statusFilter {
				continue
			}
			if modeFilter != "" && t.Mode != modeFilter {
				continue
			}
			if clientFilter != "" && t.ClientID != clientFilter {
				continue
			}
			if search != "" {
				parts := []string{t.ID, clientName, t.ClientID, t.Mode, t.Status, t.TemplateName}
				filtered := make([]string, 0, len(parts))
				for _, part := range parts {
					if part != "" {
						filtered = append(filtered, part)
					}
				}
				haystack := strings.ToLower(strings.Join(filtered, " "))
				if !strings.Contains(haystack, search) {
					continue
				}
			}
			tj = append(tj, taskJSON{
				ID: t.ID, ClientID: t.ClientID,
				ClientName: clientName,
				Mode:       t.Mode, UpMbps: t.UpMbps, DownMbps: t.DownMbps,
				DurationSec: t.DurationSec, Density: t.Density, Status: t.Status,
				StartedAt: t.StartedAt, FinishedAt: t.FinishedAt, ScheduledAt: t.ScheduledAt,
				UploadGB:      float64(t.UploadBytes) / (1024 * 1024 * 1024),
				DownloadGB:    float64(t.DownloadBytes) / (1024 * 1024 * 1024),
				UploadBytes:   t.UploadBytes,
				DownloadBytes: t.DownloadBytes,
				MaxRetries:    t.MaxRetries, RetryCount: t.RetryCount, ParentTaskID: t.ParentTaskID, TemplateName: t.TemplateName,
				PeerClientID:   t.PeerClientID,
				PeerRole:       t.PeerRole,
				PeerClientName: clientNames[t.PeerClientID],
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"clients": cj, "tasks": tj})
	}
}

func durationToSec(val int, unit string) int {
	switch unit {
	case "min":
		return val * 60
	case "hour":
		return val * 3600
	case "day":
		return val * 86400
	case "month":
		return val * 86400 * 30
	default:
		return val
	}
}

func fmtDuration(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%d秒", sec)
	} else if sec < 3600 {
		m := sec / 60
		s := sec % 60
		if s == 0 {
			return fmt.Sprintf("%d分", m)
		}
		return fmt.Sprintf("%d分%d秒", m, s)
	} else if sec < 86400 {
		h := sec / 3600
		m := (sec % 3600) / 60
		if m == 0 {
			return fmt.Sprintf("%d时", h)
		}
		return fmt.Sprintf("%d时%d分", h, m)
	}
	d := sec / 86400
	h := (sec % 86400) / 3600
	if h == 0 {
		return fmt.Sprintf("%d天", d)
	}
	return fmt.Sprintf("%d天%d时", d, h)
}

func handleRestart(panelPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, panelPath+"/server", http.StatusFound)
			return
		}
		go func() {
			time.Sleep(500 * time.Millisecond)
			_ = exec.Command("systemctl", "restart", "bwpanel").Run()
		}()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "msg": "重启指令已发出，服务将在1秒内重启"})
	}
}

func handleServerPage(panelPath string, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		barkToken := barkTokenFromURL(cfg.BarkURL)
		barkStatus := "未配置"
		if cfg.BarkURL != "" {
			barkStatus = "已配置 ✓"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>服务器管理 — 带宽测试面板</title>
<style>
@import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&display=swap');
:root{
  --primary:#6366f1;--primary-dk:#4f46e5;--primary-glow:rgba(99,102,241,.15);
  --no:#ef4444;--no-bg:rgba(239,68,68,.1);--warn:#f59e0b;
  --bg:#f8fafc;--surf:#fff;--surf2:#f1f5f9;--bdr:#e2e8f0;
  --tx:#0f172a;--tx2:#475569;--tx3:#94a3b8;
  --r:12px;--r-sm:8px;--r-lg:16px;--r-pill:9999px;
  --sh:0 1px 3px rgba(0,0,0,.06),0 4px 8px rgba(0,0,0,.04);
  --sh-lg:0 8px 24px rgba(0,0,0,.08),0 2px 8px rgba(0,0,0,.04);
}
@media(prefers-color-scheme:dark){
  :root{
    --primary:#818cf8;--primary-dk:#6366f1;--primary-glow:rgba(129,140,248,.15);
    --no:#f87171;--no-bg:rgba(248,113,113,.12);--warn:#fbbf24;
    --bg:#060d1b;--surf:#0f172a;--surf2:#1a2236;--bdr:#1e293b;
    --tx:#f1f5f9;--tx2:#94a3b8;--tx3:#475569;
  }
}
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
body{font-family:'Inter',-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Noto Sans CJK SC','Microsoft YaHei',sans-serif;
  background:var(--bg);color:var(--tx);font-size:14px;line-height:1.55;-webkit-font-smoothing:antialiased}
.nav{position:sticky;top:0;z-index:100;height:60px;display:flex;align-items:center;gap:12px;
  padding:0 24px;background:var(--surf);border-bottom:1px solid var(--bdr)}
.nav-icon{width:34px;height:34px;background:linear-gradient(135deg,var(--primary),var(--primary-dk));
  border-radius:9px;display:flex;align-items:center;justify-content:center;font-size:17px;
  box-shadow:0 3px 10px var(--primary-glow);flex-shrink:0}
.nav-title{font-weight:800;font-size:17px;letter-spacing:-.03em}
.page{max-width:760px;margin:0 auto;padding:28px 24px 60px}
.card{background:var(--surf);border:1px solid var(--bdr);border-radius:var(--r-lg);
  padding:24px;margin-bottom:20px;box-shadow:var(--sh)}
.card-hd{display:flex;align-items:center;gap:10px;margin-bottom:18px}
.card-hd-icon{width:30px;height:30px;border-radius:8px;background:var(--surf2);
  display:flex;align-items:center;justify-content:center;font-size:14px}
h2{font-size:15px;font-weight:700;color:var(--tx)}
.row{display:flex;justify-content:space-between;align-items:center;
  padding:11px 0;border-bottom:1px solid var(--bdr);gap:12px}
.row:last-child{border-bottom:none}
.row-label{font-size:13px;color:var(--tx2);font-weight:500;flex-shrink:0}
.row-val{font-family:'SF Mono','Fira Code',monospace;font-size:12px;color:var(--tx);
  word-break:break-all;text-align:right;max-width:70%%}
button,.btn{display:inline-flex;align-items:center;gap:5px;padding:9px 16px;border:none;
  border-radius:var(--r-sm);font-size:13px;font-weight:600;cursor:pointer;
  text-decoration:none;font-family:inherit;line-height:1;white-space:nowrap;
  transition:filter .15s,transform .12s}
button:hover,.btn:hover{filter:brightness(1.08);transform:translateY(-1px)}
button:active,.btn:active{transform:translateY(0);filter:brightness(.96)}
button{background:var(--primary);color:#fff}
button.danger{background:var(--no);color:#fff}
button.sec,.btn.sec{background:var(--surf2);color:var(--tx2);border:1px solid var(--bdr)}
.actions{display:flex;gap:10px;flex-wrap:wrap}
.note{font-size:13px;color:var(--tx3);margin-bottom:16px}
</style>
</head>
<body>
<nav class="nav">
  <div class="nav-icon">🐧</div>
  <span class="nav-title">服务器管理</span>
</nav>
<div class="page">

<div class="card">
  <div class="card-hd">
    <div class="card-hd-icon">🐧</div>
    <h2>服务器 Linux 管理</h2>
  </div>
  <p class="note">服务状态查看与系统操作</p>
  <a class="btn sec" href="%s">← 返回面板</a>
</div>

<div class="card">
  <div class="card-hd">
    <div class="card-hd-icon">📋</div>
    <h2>运行信息</h2>
  </div>
  <div class="row"><span class="row-label">面板监听地址</span><span class="row-val">%s</span></div>
  <div class="row"><span class="row-label">数据端口</span><span class="row-val">%s</span></div>
  <div class="row"><span class="row-label">服务器 Host</span><span class="row-val">%s</span></div>
  <div class="row"><span class="row-label">数据库路径</span><span class="row-val">%s</span></div>
  <div class="row"><span class="row-label">面板路径</span><span class="row-val">%s</span></div>
  <div class="row"><span class="row-label">Bark 推送</span><span class="row-val">%s</span></div>
  <div class="row"><span class="row-label">Bark Token</span><span class="row-val">%s</span></div>
</div>

<div class="card">
  <div class="card-hd">
    <div class="card-hd-icon">⚙️</div>
    <h2>系统操作</h2>
  </div>
  <p class="note">重启服务后面板约1秒内不可访问，会自动恢复。</p>
  <div class="actions">
    <button id="restartBtn" class="danger">🔄 重启 bwpanel 服务</button>
    <a class="btn sec" href="%s/settings">⚙️ Bark 设置</a>
  </div>
</div>

</div>
<script>
(function(){
var PANEL_PATH = %q;
document.getElementById('restartBtn').addEventListener('click', function() {
  if (!confirm('确认重启服务端？重启过程约1秒，期间面板短暂不可访问。')) return;
  fetch(PANEL_PATH + '/restart', {
    method: 'POST',
    credentials: 'include',
    headers: {'Content-Type': 'application/json'}
  }).then(function() {
    alert('重启指令已发出，3秒后自动刷新页面。');
    setTimeout(function(){ location.reload(); }, 3000);
  }).catch(function() {
    setTimeout(function(){ location.reload(); }, 3000);
  });
});
})();
</script>
</body></html>`,
			panelPath,
			cfg.PanelAddr, cfg.DataAddr, cfg.ServerHost, cfg.DBPath, cfg.PanelPath,
			barkStatus, barkToken,
			panelPath,
			panelPath,
		)
	}
}

// ── Admin Handlers (panel-facing) ─────────────────────────────────────────────

func handleAdmin(cfg *Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clients := mustClients(db)
		tasks := mustTasks(db)

		type approvedClient struct {
			ID   string
			Name string
		}
		type pageData struct {
			Clients              []Client
			RunningTasks         []Task
			HistoryTasks         []Task
			TaskTemplates        []TaskTemplate
			ApprovedClients      []approvedClient
			CancellableTaskCount int
			DefaultClientID      string
			ClientNames          map[string]string
			PanelPath            string
			ServerHost           string
			InitToken            string
			Version              string
			BarkURL              string
			PanelPathJS          template.JS
			InitTokenJS          template.JS
			VersionJS            template.JS
			TemplatesJS          template.JS
			GenName              string
			GenRemark            string
			GenVersion           string
			GenPlatform          string
			GeneratedCmd         string
		}

		var approvedClients []approvedClient
		defaultClientID := ""
		clientNames := map[string]string{}
		templates := mustTaskTemplates(db)
		for _, c := range clients {
			clientNames[c.ID] = c.Name
			if c.Approved {
				approvedClients = append(approvedClients, approvedClient{ID: c.ID, Name: c.Name})
				if defaultClientID == "" {
					defaultClientID = c.ID
				}
			}
		}

		var runningTasks, historyTasks []Task
		cancellableTaskCount := 0
		for _, t := range tasks {
			if t.Status == "running" || t.Status == "pending" || t.Status == "stopping" {
				runningTasks = append(runningTasks, t)
				if t.Status == "running" || t.Status == "pending" {
					cancellableTaskCount++
				}
			} else {
				historyTasks = append(historyTasks, t)
			}
		}

		version := getenv("BWPANEL_VERSION", Version)
		genName := strings.TrimSpace(r.URL.Query().Get("gen_name"))
		genRemark := strings.TrimSpace(r.URL.Query().Get("gen_remark"))
		genVersion := strings.TrimSpace(r.URL.Query().Get("gen_version"))
		genPlatform := strings.TrimSpace(r.URL.Query().Get("gen_platform"))
		generatedCmd := strings.TrimSpace(r.URL.Query().Get("gen_cmd"))
		if genVersion == "" {
			genVersion = version
		}
		if genPlatform != "windows" {
			genPlatform = "linux"
		}

		jsStr := func(s string) template.JS {
			b, _ := json.Marshal(s)
			return template.JS(b)
		}

		// ─────────────────────────────────────────────────────────────
		// 核心修复说明：
		// 1. 所有操作按钮（停止/删除/批准）全部改为 data-* 属性 + JS fetch
		// 2. fetch 统一带 credentials:'include'，保证 Basic Auth cookie 正常传递
		// 3. buildRunningRow / buildHistoryRow 里的按钮也全部用 data-task-id
		//    通过事件委托(event delegation)处理，不再用 innerHTML 插入 form
		// ─────────────────────────────────────────────────────────────
		const page = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>带宽测试面板</title>
<style>
@import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&display=swap');
:root {
  --primary: #6366f1; --primary-dk: #4f46e5; --primary-lt: #a5b4fc;
  --primary-glow: rgba(99,102,241,.15);
  --ok: #10b981; --ok-bg: rgba(16,185,129,.1); --ok-txt: #059669;
  --warn: #f59e0b; --warn-bg: rgba(245,158,11,.1); --warn-txt: #b45309;
  --no: #ef4444; --no-bg: rgba(239,68,68,.1); --no-txt: #dc2626;
  --info: #3b82f6; --info-bg: rgba(59,130,246,.1); --info-txt: #2563eb;
  --orange: #f97316; --orange-bg: rgba(249,115,22,.1);
  --bg: #f8fafc; --surf: #fff; --surf2: #f1f5f9;
  --bdr: #e2e8f0; --bdr2: #cbd5e1;
  --tx: #0f172a; --tx2: #475569; --tx3: #94a3b8;
  --r: 12px; --r-sm: 8px; --r-lg: 16px; --r-pill: 9999px;
  --sh: 0 1px 3px rgba(0,0,0,.06), 0 4px 8px rgba(0,0,0,.04);
  --sh-lg: 0 8px 24px rgba(0,0,0,.08), 0 2px 8px rgba(0,0,0,.04);
  --sh-xl: 0 24px 48px rgba(0,0,0,.12), 0 8px 16px rgba(0,0,0,.06);
}
@media (prefers-color-scheme: dark) {
  :root {
    --primary: #818cf8; --primary-dk: #6366f1; --primary-lt: #c7d2fe;
    --primary-glow: rgba(129,140,248,.15);
    --ok: #34d399; --ok-bg: rgba(52,211,153,.12); --ok-txt: #6ee7b7;
    --warn: #fbbf24; --warn-bg: rgba(251,191,36,.12); --warn-txt: #fde68a;
    --no: #f87171; --no-bg: rgba(248,113,113,.12); --no-txt: #fca5a5;
    --info: #60a5fa; --info-bg: rgba(96,165,250,.12); --info-txt: #93c5fd;
    --orange: #fb923c; --orange-bg: rgba(251,146,60,.12);
    --bg: #060d1b; --surf: #0f172a; --surf2: #1a2236;
    --bdr: #1e293b; --bdr2: #334155;
    --tx: #f1f5f9; --tx2: #94a3b8; --tx3: #475569;
    --sh: 0 1px 3px rgba(0,0,0,.3), 0 4px 8px rgba(0,0,0,.2);
    --sh-lg: 0 8px 24px rgba(0,0,0,.4), 0 2px 8px rgba(0,0,0,.2);
    --sh-xl: 0 24px 48px rgba(0,0,0,.65), 0 8px 16px rgba(0,0,0,.3);
  }
}
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
body {
  font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', 'Noto Sans CJK SC', 'Microsoft YaHei', sans-serif;
  background: var(--bg); color: var(--tx); font-size: 14px; line-height: 1.55;
  -webkit-font-smoothing: antialiased;
}
/* ── Nav ── */
.nav {
  position: sticky; top: 0; z-index: 200; height: 52px;
  display: flex; align-items: center; gap: 10px; padding: 0 20px;
  background: var(--surf); border-bottom: 1px solid var(--bdr);
  box-shadow: 0 1px 0 var(--bdr);
}
.nav-logo {
  display: flex; align-items: center; gap: 10px;
  font-weight: 800; font-size: 16px; letter-spacing: -.03em;
  text-decoration: none; color: var(--tx);
}
.nav-icon {
  width: 32px; height: 32px; background: linear-gradient(135deg, var(--primary), var(--primary-dk));
  border-radius: 8px; display: flex; align-items: center; justify-content: center;
  font-size: 16px; box-shadow: 0 3px 10px var(--primary-glow); flex-shrink: 0;
}
.ver-badge {
  display: inline-flex; align-items: center; padding: 2px 8px;
  background: var(--primary-glow); color: var(--primary); border-radius: var(--r-pill);
  font-size: 11px; font-weight: 600; vertical-align: middle; margin-left: 6px;
}
.nav-spacer { flex: 1; }
.nav-status { display: flex; align-items: center; gap: 6px; font-size: 12px; color: var(--tx3); flex-shrink: 0; }
.live-dot {
  width: 7px; height: 7px; background: var(--ok); border-radius: 50%;
  animation: pulse 2.4s ease infinite; flex-shrink: 0;
}
@keyframes pulse {
  0%,100% { opacity:1; box-shadow:0 0 0 0 rgba(16,185,129,.4); }
  50% { opacity:.6; box-shadow:0 0 0 5px rgba(16,185,129,0); }
}
.nav-acts { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
/* ── Dropdown menu ── */
.dropdown { position: relative; display: inline-flex; }
.dropdown-menu {
  display: none; position: absolute; top: calc(100% + 6px); right: 0;
  background: var(--surf); border: 1px solid var(--bdr); border-radius: var(--r);
  box-shadow: var(--sh-lg); min-width: 150px; z-index: 300; overflow: hidden;
}
.dropdown.open .dropdown-menu { display: block; }
.dropdown-item {
  display: flex; align-items: center; gap: 8px; padding: 9px 14px;
  font-size: 13px; font-weight: 500; color: var(--tx2); cursor: pointer;
  border: none; background: none; width: 100%; text-align: left; font-family: inherit;
  text-decoration: none; white-space: nowrap;
  transition: background .1s;
}
.dropdown-item:hover { background: var(--surf2); color: var(--tx); transform: none; filter: none; }
/* ── Peer transfer badge ── */
.peer-badge {
  display: inline-flex; align-items: center; gap: 4px; padding: 2px 7px;
  background: rgba(99,102,241,.12); color: var(--primary); border-radius: var(--r-pill);
  font-size: 11px; font-weight: 700; white-space: nowrap;
}
/* ── Name cell with sub-info ── */
.name-cell { display: flex; flex-direction: column; gap: 2px; }
.name-cell-main { font-weight: 600; }
.name-cell-sub { font-size: 11px; color: var(--tx3); display: flex; gap: 6px; flex-wrap: wrap; }
/* ── Page ── */
.wrap { max-width: 1440px; margin: 0 auto; padding: 24px 24px 60px; }
/* ── Card ── */
.card {
  background: var(--surf); border: 1px solid var(--bdr);
  border-radius: var(--r-lg); padding: 24px; margin-bottom: 20px;
  box-shadow: var(--sh);
}
.card-hd {
  display: flex; align-items: center; justify-content: space-between;
  margin-bottom: 18px; flex-wrap: wrap; gap: 10px;
}
.card-hd-left { display: flex; align-items: center; gap: 10px; }
.card-hd-right { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-left: auto; }
h1 { font-size: 22px; font-weight: 800; letter-spacing: -.03em; color: var(--tx); }
h2 { font-size: 15px; font-weight: 700; color: var(--tx); display: flex; align-items: center; gap: 8px; margin: 0; }
.h2-icon {
  width: 30px; height: 30px; border-radius: 8px; background: var(--surf2);
  display: flex; align-items: center; justify-content: center; font-size: 14px; flex-shrink: 0;
}
/* ── Stats ── */
.stats-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 14px; margin-bottom: 20px; }
.stat-card {
  background: var(--surf); border: 1px solid var(--bdr); border-radius: var(--r-lg);
  padding: 18px 20px; box-shadow: var(--sh); display: flex; align-items: center; gap: 14px;
  transition: transform .15s, box-shadow .15s;
}
.stat-card:hover { transform: translateY(-2px); box-shadow: var(--sh-lg); }
.stat-ico {
  width: 44px; height: 44px; border-radius: 11px;
  display: flex; align-items: center; justify-content: center; font-size: 20px; flex-shrink: 0;
}
.stat-ico.c1 { background: var(--info-bg); }
.stat-ico.c2 { background: var(--ok-bg); }
.stat-ico.c3 { background: var(--warn-bg); }
.stat-ico.c4 { background: var(--primary-glow); }
.stat-body { flex: 1; min-width: 0; }
.stat-label { font-size: 11px; font-weight: 600; color: var(--tx3); text-transform: uppercase; letter-spacing: .06em; }
.stat-val { font-size: 26px; font-weight: 800; color: var(--tx); letter-spacing: -.03em; line-height: 1.2; margin-top: 2px; }
/* ── Buttons ── */
button, .btn {
  display: inline-flex; align-items: center; gap: 5px;
  padding: 8px 14px; border: none; border-radius: var(--r-sm);
  font-size: 13px; font-weight: 600; cursor: pointer;
  text-decoration: none; font-family: inherit; line-height: 1; white-space: nowrap;
  transition: filter .15s, transform .12s, box-shadow .15s;
}
button:hover, .btn:hover { filter: brightness(1.08); transform: translateY(-1px); }
button:active, .btn:active { transform: translateY(0); filter: brightness(.96); }
button:disabled, .btn:disabled { opacity: .5; cursor: not-allowed; pointer-events: none; }
button { background: var(--primary); color: #fff; }
button.sec, .btn.sec { background: var(--surf2); color: var(--tx2); border: 1px solid var(--bdr); }
button.danger { background: var(--no); color: #fff; }
button.warn { background: var(--warn); color: #fff; }
button.info { background: var(--info); color: #fff; }
/* ── Table ── */
.tbl-wrap { overflow-x: auto; margin: 0 -4px; padding: 0 4px; }
table { width: 100%; border-collapse: separate; border-spacing: 0; }
th {
  padding: 9px 14px; font-size: 11px; font-weight: 700; color: var(--tx3);
  text-transform: uppercase; letter-spacing: .07em; background: var(--surf2);
  border-bottom: 1px solid var(--bdr); text-align: left; white-space: nowrap;
}
th:first-child { border-radius: 8px 0 0 8px; }
th:last-child { border-radius: 0 8px 8px 0; }
td { padding: 11px 14px; border-bottom: 1px solid var(--bdr); font-size: 13px; vertical-align: middle; }
tbody tr:last-child td { border-bottom: none; }
tbody tr { transition: background .1s; }
tbody tr:hover td { background: var(--surf2); }
/* ── Badge ── */
.badge {
  display: inline-flex; align-items: center; gap: 4px; padding: 3px 9px;
  border-radius: var(--r-pill); font-size: 11px; font-weight: 700;
  text-transform: uppercase; letter-spacing: .04em; white-space: nowrap;
}
.badge::before { content: ''; width: 5px; height: 5px; border-radius: 50%; background: currentColor; flex-shrink: 0; }
.badge.running { background: var(--info-bg); color: var(--info); }
.badge.done { background: var(--ok-bg); color: var(--ok); }
.badge.pending { background: var(--surf2); color: var(--tx3); border: 1px solid var(--bdr); }
.badge.stopping { background: var(--orange-bg); color: var(--orange); }
/* ── Progress ── */
.progress-bar-container { width: 90px; height: 4px; background: var(--bdr); border-radius: 2px; overflow: hidden; }
.progress-bar-fill {
  height: 100%; background: linear-gradient(90deg, var(--primary), var(--primary-lt));
  border-radius: 2px; transition: width .6s cubic-bezier(.4,0,.2,1);
}
/* ── Form ── */
.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; align-items: flex-end; }
label.note, .note { font-size: 12px; font-weight: 600; color: var(--tx2); }
input, select, textarea {
  width: 100%; padding: 9px 11px; border: 1.5px solid var(--bdr); border-radius: var(--r-sm);
  background: var(--surf); color: var(--tx); font-size: 13px; font-family: inherit;
  transition: border-color .15s, box-shadow .15s;
}
input:focus, select:focus, textarea:focus {
  outline: none; border-color: var(--primary); box-shadow: 0 0 0 3px var(--primary-glow);
}
.dur-wrap { display: flex; gap: 6px; }
.dur-wrap input { flex: 1; }
.dur-wrap select { width: auto; min-width: 72px; flex-shrink: 0; }
/* ── Task form layout ── */
.task-form-grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: 14px; align-items: flex-start; }
.form-group { display: flex; flex-direction: column; gap: 6px; }
.density-wrap { display: flex; flex-direction: column; gap: 6px; }
.density-opt {
  display: flex; align-items: center; gap: 6px; cursor: pointer;
  padding: 6px 10px; border-radius: var(--r-sm); border: 1.5px solid var(--bdr);
  background: var(--surf); transition: border-color .15s, box-shadow .15s, background .15s;
  font-size: 12px;
}
.density-opt:hover { border-color: var(--primary); background: var(--primary-glow); }
.density-opt input[type="radio"] { accent-color: var(--primary); margin: 0; }
.density-opt:has(input:checked) {
  border-color: var(--primary); background: var(--primary-glow);
  box-shadow: 0 0 0 2px var(--primary-glow);
}
.density-label { font-weight: 600; white-space: nowrap; }
.quick-bar {
  margin-top: 14px; display: flex; gap: 8px; align-items: center; flex-wrap: wrap;
  padding-top: 14px; border-top: 1px solid var(--bdr);
}
@media (max-width: 768px) {
  .task-form-grid { grid-template-columns: 1fr 1fr; }
}
@media (max-width: 480px) {
  .task-form-grid { grid-template-columns: 1fr; }
}
/* ── Toolbar ── */
.toolbar { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 16px; align-items: center; justify-content: space-between; }
/* ── Misc ── */
.tip { font-size: 12px; color: var(--tx3); margin-top: 8px; }
.copy-box { display: flex; gap: 8px; align-items: center; }
.copy-box input { font-family: 'SF Mono','Fira Code',monospace; font-size: 12px; background: var(--surf2); }
.ping-ok { color: var(--ok); font-weight: 600; font-size: 12px; }
.ping-warn { color: var(--warn); font-weight: 600; font-size: 12px; }
.ping-dead { color: var(--tx3); font-size: 12px; }
/* ── Modal ── */
.modal-overlay {
  position: fixed; inset: 0; background: rgba(0,0,0,.6); z-index: 1000;
  display: flex; align-items: center; justify-content: center;
  backdrop-filter: blur(8px); visibility: hidden; opacity: 0;
  transition: visibility .2s, opacity .2s;
}
.modal-overlay.open { visibility: visible; opacity: 1; }
.modal-inner {
  background: var(--surf); border: 1px solid var(--bdr); border-radius: var(--r-lg);
  padding: 28px; width: min(560px, 95vw); box-shadow: var(--sh-xl);
  transform: scale(.92) translateY(10px); opacity: 0;
  transition: transform .25s cubic-bezier(.34,1.56,.64,1), opacity .2s;
}
.modal-overlay.open .modal-inner { transform: scale(1) translateY(0); opacity: 1; }
.modal-hd { display: flex; align-items: center; justify-content: space-between; margin-bottom: 18px; }
.modal-title { font-size: 16px; font-weight: 700; }
.modal-x {
  background: var(--surf2); border: 1px solid var(--bdr); border-radius: 6px;
  width: 28px; height: 28px; display: flex; align-items: center; justify-content: center;
  cursor: pointer; font-size: 15px; color: var(--tx3); padding: 0; line-height: 1; flex-shrink: 0;
}
.modal-x:hover { background: var(--no-bg); color: var(--no); border-color: var(--no-bg); transform: none; filter: none; }
/* ── Log viewer ── */
#logsContent {
  background: #0d1117; color: #e6edf3; padding: 14px; border-radius: 8px;
  font-family: 'SF Mono','Fira Code',monospace; font-size: 12px; line-height: 1.6;
  height: 480px; overflow-y: auto; white-space: pre-wrap; word-break: break-all;
  border: 1px solid var(--bdr);
}
#logsContent::-webkit-scrollbar { width: 5px; }
#logsContent::-webkit-scrollbar-track { background: transparent; }
#logsContent::-webkit-scrollbar-thumb { background: #30363d; border-radius: 3px; }
/* ── Responsive ── */
@media (max-width: 1200px) { .stats-grid { grid-template-columns: repeat(2, 1fr); } }
@media (max-width: 768px) {
  .wrap { padding: 12px 12px 40px; }
  .nav { padding: 0 14px; height: auto; min-height: 52px; flex-wrap: wrap; padding-top: 8px; padding-bottom: 8px; }
  .nav-acts { order: 3; width: 100%; }
  .stats-grid { grid-template-columns: 1fr 1fr; }
  th { display: none; }
  td { display: block; padding: 7px 14px; border: none; text-align: right; font-size: 13px; }
  td::before { content: attr(data-label); float: left; font-weight: 600; color: var(--tx3); font-size: 11px; text-transform: uppercase; }
  tr { display: block; border: 1px solid var(--bdr); border-radius: 10px; margin-bottom: 8px; padding: 6px 0; }
  tbody tr:hover td { background: transparent; }
  .grid { grid-template-columns: 1fr; }
  .ssh-col { display: none; }
}
@media (max-width: 480px) { .stats-grid { grid-template-columns: 1fr; } }
</style>
</head>
<body>

<!-- ── Navigation Bar ────────────────────────────────────────────── -->
<nav class="nav">
  <a class="nav-logo" href="#">
    <div class="nav-icon">⚡</div>
    带宽测试面板
    <span class="ver-badge">{{.Version}}</span>
  </a>
  <div class="nav-spacer"></div>
  <div class="nav-status">
    <div class="live-dot"></div>
    <span id="liveStatus">同步中...</span>
  </div>
  <div class="nav-acts">
    <button type="button" class="info" onclick="document.getElementById('genModal').classList.add('open')">➕ 接入客户端</button>
    <button type="button" id="openPeerModalBtn" style="background:var(--primary)">⚡ 对传</button>
    <button type="button" class="warn" id="upgradeAllBtn">🚀 一键升级</button>
    <div class="dropdown" id="moreDropdown">
      <button type="button" class="sec" id="moreBtn">⋯ 更多</button>
      <div class="dropdown-menu">
        <button type="button" class="dropdown-item" id="reloadBtn">🔄 刷新</button>
        <button type="button" class="dropdown-item" id="toggleHistoryBtn">📜 历史任务</button>
        <a class="dropdown-item" href="{{.PanelPath}}/settings">⚙️ 设置</a>
        <a class="dropdown-item" href="{{.PanelPath}}/server">🐧 服务器</a>
      </div>
    </div>
  </div>
</nav>

<div class="wrap">

<!-- ── Stats ─────────────────────────────────────────────────────── -->
<div class="stats-grid">
  <div class="stat-card">
    <div class="stat-ico c1">🖥</div>
    <div class="stat-body">
      <div class="stat-label">在线客户端</div>
      <div id="statOnline" class="stat-val">-</div>
    </div>
  </div>
  <div class="stat-card">
    <div class="stat-ico c2">⚡</div>
    <div class="stat-body">
      <div class="stat-label">运行中任务</div>
      <div id="statRunning" class="stat-val">-</div>
    </div>
  </div>
  <div class="stat-card">
    <div class="stat-ico c3">⏳</div>
    <div class="stat-body">
      <div class="stat-label">待处理请求</div>
      <div id="statPending" class="stat-val">-</div>
    </div>
  </div>
  <div class="stat-card">
    <div class="stat-ico c4">📊</div>
    <div class="stat-body">
      <div class="stat-label">总吞吐量</div>
      <div id="statTraffic" class="stat-val">-</div>
    </div>
  </div>
</div>

<!-- ── 任务日志弹窗 ──────────────────────────────────────────────── -->
<div id="logsModal" class="modal-overlay">
  <div class="modal-inner" style="width:min(900px,95vw)">
    <div class="modal-hd">
      <span class="modal-title">📄 任务执行日志 <span style="font-size:12px;font-weight:400;color:var(--tx3)">(最近200条)</span></span>
      <button type="button" class="modal-x" onclick="document.getElementById('logsModal').classList.remove('open')">✕</button>
    </div>
    <div id="logsContent">正在加载...</div>
  </div>
</div>

<!-- ── 升级命令弹窗 ──────────────────────────────────────────────── -->
<div id="upgradeModal" class="modal-overlay">
  <div class="modal-inner">
    <div class="modal-hd">
      <span class="modal-title">📦 客户端升级命令</span>
      <button type="button" class="modal-x" id="closeUpgradeBtn">✕</button>
    </div>
    <p style="margin-bottom:14px;font-size:13px;color:var(--tx3)">在对应客户端 VPS 上以 root 运行此命令，配置文件将自动保留。</p>
    <div class="copy-box">
      <input id="upgradeCmd" readonly>
      <button type="button" class="sec" id="copyUpgradeBtn">复制</button>
    </div>
  </div>
</div>

<!-- ── 推送更新弹窗 ──────────────────────────────────────────────── -->
<div id="pushUpgradeModal" class="modal-overlay">
  <div class="modal-inner" style="width:min(480px,95vw)">
    <div class="modal-hd">
      <span class="modal-title">🚀 推送自动更新</span>
      <button type="button" class="modal-x" id="closePushUpgradeBtn">✕</button>
    </div>
    <p style="margin-bottom:16px;font-size:13px;color:var(--tx3)">客户端下次心跳（约20秒内）将自动下载并替换二进制，然后重启 bwagent 服务。</p>
    <div style="margin-bottom:12px">
      <label style="font-size:12px;font-weight:600;color:var(--tx2);display:block;margin-bottom:4px">目标客户端</label>
      <input id="pushUpgradeClientName" readonly style="background:var(--surf2)">
    </div>
    <div style="margin-bottom:18px">
      <label style="font-size:12px;font-weight:600;color:var(--tx2);display:block;margin-bottom:4px">目标版本号（留空使用当前服务端版本）</label>
      <input id="pushUpgradeVersion" placeholder="例：v0.2.1 或 latest">
    </div>
    <div style="display:flex;gap:8px">
      <button type="button" class="warn" id="confirmPushUpgradeBtn">确认推送</button>
      <button type="button" class="sec" onclick="document.getElementById('pushUpgradeModal').classList.remove('open')">取消</button>
    </div>
  </div>
</div>

<!-- ── 编辑客户端弹窗 ─────────────────────────────────────────────── -->
<div id="editModal" class="modal-overlay">
  <div class="modal-inner" style="width:min(480px,95vw)">
    <div class="modal-hd">
      <span class="modal-title">✏️ 编辑客户端</span>
      <button type="button" class="modal-x" onclick="document.getElementById('editModal').classList.remove('open')">✕</button>
    </div>
    <div style="margin-bottom:12px">
      <label style="font-size:12px;font-weight:600;color:var(--tx2);display:block;margin-bottom:4px">名称</label>
      <input id="editName" placeholder="客户端名称">
    </div>
    <div style="margin-bottom:18px">
      <label style="font-size:12px;font-weight:600;color:var(--tx2);display:block;margin-bottom:4px">备注</label>
      <textarea id="editRemark" placeholder="备注（可选）" rows="3"></textarea>
    </div>
    <div style="margin-bottom:12px">
      <label style="font-size:12px;font-weight:600;color:var(--tx2);display:block;margin-bottom:4px">分组</label>
      <input id="editGroupName" placeholder="例如：香港 / 美国 / 核心节点">
    </div>
    <div style="margin-bottom:18px">
      <label style="font-size:12px;font-weight:600;color:var(--tx2);display:block;margin-bottom:4px">标签</label>
      <input id="editTags" placeholder="例如：CN2,优选,高防">
    </div>
    <div style="display:flex;gap:8px">
      <button type="button" id="saveEditBtn">保存</button>
      <button type="button" class="sec" id="closeEditBtn">取消</button>
    </div>
  </div>
</div>

<!-- ── 对传任务弹窗 ───────────────────────────────────────────────── -->
<div id="peerModal" class="modal-overlay">
  <div class="modal-inner" style="width:min(560px,95vw)">
    <div class="modal-hd">
      <span class="modal-title">⚡ 创建对传任务</span>
      <button type="button" class="modal-x" onclick="document.getElementById('peerModal').classList.remove('open')">✕</button>
    </div>
    <p style="margin-bottom:14px;font-size:13px;color:var(--tx3)">
      两台客户端直接建立 TCP 连接互传数据，不经过服务端，真实测量端到端带宽。
    </p>
    <!-- 源端 / 目标端选择 -->
    <div style="display:grid;grid-template-columns:1fr auto 1fr;gap:8px;align-items:end;margin-bottom:14px">
      <div>
        <label style="font-size:11px;font-weight:700;color:var(--tx2);display:flex;align-items:center;gap:4px;margin-bottom:5px">
          <span style="display:inline-flex;align-items:center;justify-content:center;width:18px;height:18px;background:var(--info-bg);color:var(--info);border-radius:4px;font-size:10px">🔊</span>
          源端 <span style="font-weight:400;color:var(--tx3)">(监听等待连接)</span>
        </label>
        <select id="peerSourceSelect" style="width:100%;padding:9px 11px;border:1.5px solid var(--bdr);border-radius:var(--r-sm);background:var(--surf);color:var(--tx);font-size:13px">
          <option value="">— 请选择 —</option>
          {{range .ApprovedClients}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
        </select>
      </div>
      <button type="button" id="peerSwapBtn" title="互换源端与目标端" style="background:var(--surf2);color:var(--tx2);border:1px solid var(--bdr);padding:8px 10px;align-self:end;margin-bottom:1px">⇄</button>
      <div>
        <label style="font-size:11px;font-weight:700;color:var(--tx2);display:flex;align-items:center;gap:4px;margin-bottom:5px">
          <span style="display:inline-flex;align-items:center;justify-content:center;width:18px;height:18px;background:var(--ok-bg);color:var(--ok-txt);border-radius:4px;font-size:10px">🔌</span>
          目标端 <span style="font-weight:400;color:var(--tx3)">(主动发起连接)</span>
        </label>
        <select id="peerTargetSelect" style="width:100%;padding:9px 11px;border:1.5px solid var(--bdr);border-radius:var(--r-sm);background:var(--surf);color:var(--tx);font-size:13px">
          <option value="">— 请选择 —</option>
          {{range .ApprovedClients}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
        </select>
      </div>
    </div>
    <!-- 连接方向示意 -->
    <div style="display:flex;align-items:center;justify-content:center;gap:6px;margin-bottom:14px;font-size:12px;color:var(--tx3)">
      <span id="peerSrcLabel" style="font-weight:600;color:var(--info);max-width:120px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">源端</span>
      <span style="flex:1;height:2px;background:linear-gradient(90deg,var(--info),var(--ok));border-radius:1px;position:relative;min-width:40px">
        <span style="position:absolute;left:50%;top:50%;transform:translate(-50%,-50%);background:var(--surf);padding:0 4px;font-size:11px;color:var(--primary);font-weight:700">↔</span>
      </span>
      <span id="peerTgtLabel" style="font-weight:600;color:var(--ok-txt);max-width:120px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">目标端</span>
    </div>
    <!-- 速率 + 时长 -->
    <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-bottom:14px">
      <div>
        <label style="font-size:12px;font-weight:600;color:var(--tx2);display:block;margin-bottom:5px">目标速率 (Mbps)</label>
        <input id="peerMbps" type="number" value="100" min="1" max="100000" style="width:100%">
      </div>
      <div>
        <label style="font-size:12px;font-weight:600;color:var(--tx2);display:block;margin-bottom:5px">测试时长</label>
        <div style="display:flex;gap:6px">
          <input id="peerDurVal" type="number" value="1" min="1" style="flex:1;min-width:0">
          <select id="peerDurUnit" style="width:auto;min-width:68px;padding:9px 11px;border:1.5px solid var(--bdr);border-radius:var(--r-sm);background:var(--surf);color:var(--tx);font-size:13px;flex-shrink:0">
            <option value="sec">秒</option>
            <option value="min" selected>分</option>
            <option value="hour">时</option>
          </select>
        </div>
      </div>
    </div>
    <!-- 预计传输量 -->
    <div style="display:flex;align-items:center;gap:8px;padding:10px 12px;background:var(--surf2);border-radius:var(--r-sm);margin-bottom:12px;font-size:12px">
      <span style="color:var(--tx3)">📊 预计传输量：</span>
      <span id="peerEstimate" style="font-weight:700;color:var(--primary)">—</span>
      <span style="margin-left:auto;color:var(--tx3)">源端需有公网 IP 或与目标端同网</span>
    </div>
    <div style="display:flex;gap:8px">
      <button type="button" id="confirmPeerBtn" style="background:var(--primary)">⚡ 创建对传</button>
      <button type="button" class="sec" onclick="document.getElementById('peerModal').classList.remove('open')">取消</button>
    </div>
  </div>
</div>

<!-- ── 接入新客户端弹窗 ───────────────────────────────────────────── -->
<div id="genModal" class="modal-overlay {{if .GeneratedCmd}}open{{end}}">
  <div class="modal-inner">
    <div class="modal-hd">
      <span class="modal-title">➕ 接入新客户端</span>
      <button type="button" class="modal-x" onclick="document.getElementById('genModal').classList.remove('open')">✕</button>
    </div>
    <form id="genForm" method="post" action="{{.PanelPath}}/gen/install-cmd">
      <div style="display:flex;flex-direction:column;gap:12px">
        <div>
          <label class="note" style="display:block;margin-bottom:4px">客户端名称 *</label>
          <input id="genName" name="gen_name" value="{{.GenName}}" required placeholder="例如：my-vps">
        </div>
        <div>
          <label class="note" style="display:block;margin-bottom:4px">备注（可选）</label>
          <input id="genRemark" name="gen_remark" value="{{.GenRemark}}" placeholder="可选备注">
        </div>
        <div>
          <label class="note" style="display:block;margin-bottom:4px">版本号</label>
          <input id="genVersion" name="gen_version" value="{{.GenVersion}}">
        </div>
        <div>
          <label class="note" style="display:block;margin-bottom:4px">平台</label>
          <select id="genPlatform" name="gen_platform" style="width:100%;padding:8px;border-radius:6px;border:1px solid var(--bdr);background:var(--surf);color:var(--tx);font-size:14px">
            <option value="linux"   {{if ne .GenPlatform "windows"}}selected{{end}}>Linux (bash)</option>
            <option value="windows" {{if eq .GenPlatform "windows"}}selected{{end}}>Windows (PowerShell)</option>
          </select>
        </div>
        <button type="submit" id="genBtn" style="width:100%">生成安装命令</button>
      </div>
    </form>
    <div class="copy-box" id="cmdBox" style="display:{{if .GeneratedCmd}}flex{{else}}none{{end}};margin-top:16px">
      <input id="cmdText" readonly value="{{.GeneratedCmd}}">
      <button type="button" class="sec" id="copyCmdBtn">复制</button>
    </div>
    <div class="tip" id="cmdTip">{{if .GeneratedCmd}}{{if eq .GenPlatform "windows"}}以管理员身份打开 PowerShell，粘贴并执行此命令即可完成安装。{{else}}将此命令复制到客户端 VPS 上执行即可完成安装与注册。{{end}}{{end}}</div>
  </div>
</div>

<!-- ── 客户端列表 ─────────────────────────────────────────────────── -->
<div class="card">
  <div class="card-hd">
    <div class="card-hd-left">
      <div class="h2-icon">🖥</div>
      <h2>客户端列表</h2>
    </div>
    <div class="card-hd-right" style="font-size:12px">
      <button type="button" class="sec" id="selectAllClientsBtn">全选任务目标</button>
      <button type="button" class="sec" id="deselectAllClientsBtn">清空选择</button>
      <button type="button" class="sec" id="toggleSshColBtn" title="显示/隐藏 SSH 尝试次数列">🔒 SSH列</button>
      <button type="button" class="warn" id="bulkRestartClientsBtn">批量重启客户端</button>
      <button type="button" class="danger" id="bulkDeleteClientsBtn">批量删除客户端</button>
    </div>
  </div>
  <div class="tbl-wrap">
  <table>
    <thead><tr>
      <th><label style="display:inline-flex;align-items:center;gap:6px;margin:0;color:inherit;font-size:inherit;font-weight:inherit;cursor:pointer"><input type="checkbox" id="clientSelectAllToggle" aria-label="全选客户端" style="width:15px;height:15px;margin:0;cursor:pointer;accent-color:var(--primary)">全选</label></th>
      <th>名称 / 分组 / 标签</th><th>版本</th><th>批准</th><th>延迟</th>
      <th class="ssh-col" style="display:none">SSH尝试(1h)</th>
      <th>心跳</th><th>IP</th><th>当前任务</th><th>操作</th>
    </tr></thead>
    <tbody id="clientBody">
    {{range .Clients}}
    <tr data-client-id="{{.ID}}" data-last-seen="{{.LastSeen}}" data-name="{{.Name}}" data-remark="{{.Remark}}" data-group-name="{{.GroupName}}" data-tags="{{.Tags}}" data-latency="{{.Latency}}" data-ssh-attempts="{{.SSHAttempts}}" data-approved="{{if .Approved}}1{{else}}0{{end}}" data-upgrade-to="{{.UpgradeTo}}">
      <td data-label="选择" class="select-col">{{if .Approved}}<input type="checkbox" class="task-client-checkbox" value="{{.ID}}" aria-label="选择客户端 {{.Name}}" {{if eq .ID $.DefaultClientID}}checked{{end}} style="width:15px;height:15px;margin:0;cursor:pointer;accent-color:var(--primary)">{{else}}-{{end}}</td>
      <td data-label="名称" class="name-col">
        <div class="name-cell">
          <div class="name-cell-main">{{.Name}}</div>
          <div class="name-cell-sub">
            {{if .GroupName}}<span>📁 {{.GroupName}}</span>{{end}}
            {{if .Tags}}<span>🏷 {{.Tags}}</span>{{end}}
            {{if .Remark}}<span style="font-style:italic">{{.Remark}}</span>{{end}}
          </div>
        </div>
      </td>
      <td data-label="版本"><code style="font-size:11px;background:var(--surf2);padding:2px 7px;border-radius:5px;border:1px solid var(--bdr);font-family:monospace">{{.Version}}</code></td>
      <td data-label="批准" class="approve-col">{{if .Approved}}<span class="badge done">YES</span>{{else}}<span class="badge pending">NO</span>{{end}}</td>
      <td data-label="延迟" class="ping-col">{{if gt .Latency 0}}{{.Latency}} ms{{else}}-{{end}}</td>
      <td data-label="SSH尝试" class="sshattempt-col ssh-col" style="display:none">{{.SSHAttempts}}</td>
      <td data-label="心跳" class="lastseen-col" style="color:var(--tx3);font-size:12px">{{.LastSeen | shortTime}}</td>
      <td data-label="IP" class="ip-col" style="font-family:monospace;font-size:12px;color:var(--tx2)">{{.RemoteIP}}</td>
      <td data-label="任务" class="curtask-col" style="font-family:monospace;font-size:11px;color:var(--tx3)">{{.CurrentTask}}</td>
      <td data-label="操作">
        <div class="client-actions" style="display:flex;gap:5px;flex-wrap:wrap">
          {{if (not .Approved)}}<button type="button" class="approve-btn" data-id="{{.ID}}" style="background:var(--ok)">批准</button>{{end}}
          <button type="button" class="sec edit-btn" data-id="{{.ID}}" data-name="{{.Name}}" data-remark="{{.Remark}}" data-group-name="{{.GroupName}}" data-tags="{{.Tags}}">编辑</button>
          <button type="button" class="info upgrade-btn" data-id="{{.ID}}" data-name="{{.Name}}">升级码</button>
          <button type="button" class="warn push-upgrade-btn" data-id="{{.ID}}" data-name="{{.Name}}">推送</button>
          <button type="button" class="danger del-client-btn" data-id="{{.ID}}">删除</button>
        </div>
      </td>
    </tr>
    {{end}}
    </tbody>
  </table>
  </div>
</div>

<!-- ── 创建任务 ───────────────────────────────────────────────────── -->
<div class="card">
  <div class="card-hd">
    <div class="card-hd-left">
      <div class="h2-icon">➕</div>
      <h2>创建任务</h2>
    </div>
    <div class="card-hd-right" style="font-size:12px">
      <select id="templateSelect">
        <option value="">加载任务模板</option>
        {{range .TaskTemplates}}<option value="{{.ID}}" data-mode="{{.Mode}}" data-up="{{.UpMbps}}" data-down="{{.DownMbps}}" data-duration="{{.DurationSec}}" data-density="{{.Density}}" data-max-retries="{{.MaxRetries}}">{{.Name}}</option>{{end}}
      </select>
      <button type="button" class="sec" id="saveTemplateBtn">💾 保存模板</button>
    </div>
  </div>
  <form id="taskForm" method="post" action="{{.PanelPath}}/task/create">
    <div class="task-form-grid">
      <div class="form-group">
        <label class="note">模式</label>
        <select name="mode">
          <option value="upload">上传</option>
          <option value="download">下载</option>
          <option value="traditional">传统</option>
          <option value="browse">网页浏览</option>
          <option value="stream">流媒体</option>
          <option value="backup">备份同步</option>
        </select>
      </div>
      <div class="form-group">
        <label class="note">上传 Mbps</label>
        <input name="up_mbps" value="1">
      </div>
      <div class="form-group">
        <label class="note">下载 Mbps</label>
        <input name="down_mbps" value="1">
      </div>
      <div class="form-group">
        <label class="note">时长</label>
        <div class="dur-wrap">
          <input name="duration_val" value="1" type="number" min="1">
          <select name="duration_unit">
            <option value="sec">秒</option>
            <option value="min" selected>分</option>
            <option value="hour">时</option>
            <option value="day">天</option>
            <option value="month">月</option>
          </select>
        </div>
      </div>
      <div class="form-group">
        <label class="note">密度</label>
        <div class="density-wrap">
          <label class="density-opt" for="density-0" title="无间歇，全程持续运行"><input type="radio" name="density" id="density-0" value="0" checked><span class="density-label">🔴 无间歇</span></label>
          <label class="density-opt" for="density-30" title="30% 时间随机暂停"><input type="radio" name="density" id="density-30" value="30"><span class="density-label">🟡 30% 间歇</span></label>
          <label class="density-opt" for="density-70" title="70% 时间随机暂停"><input type="radio" name="density" id="density-70" value="70"><span class="density-label">🟢 70% 间歇</span></label>
        </div>
      </div>
      <div class="form-group">
        <label class="note">失败自动重试</label>
        <input name="max_retries" value="0" type="number" min="0" max="10">
      </div>
      <div class="form-group">
        <label class="note">定时执行</label>
        <input id="scheduledAtInput" type="datetime-local">
        <input type="hidden" name="scheduled_at" id="scheduledAtRFC3339">
      </div>
      <div class="form-group">
        <label class="note">模板标识</label>
        <input name="template_name" id="templateNameInput" placeholder="可选模板名">
      </div>
      <div class="form-group" style="align-self:end">
        <button type="submit" id="createTaskBtn" style="width:100%">🚀 创建任务</button>
      </div>
    </div>
  </form>
  <div class="tip">💡 提示：先在上方客户端列表勾选目标服务器，可同时下发任务给多个客户端。密度选项控制任务运行时的间歇比例。</div>
  <div class="quick-bar">
    <span class="note" style="font-weight:700">⚡ 快速测速:</span>
    <button type="button" class="sec flash-test-btn" data-mode="upload">🚀 上传</button>
    <button type="button" class="sec flash-test-btn" data-mode="download">📥 下载</button>
    <button type="button" class="sec flash-test-btn" data-mode="traditional">🔄 传统</button>
    <button type="button" class="sec flash-test-btn" data-mode="browse">🌐 网页</button>
    <button type="button" class="sec flash-test-btn" data-mode="stream">🎬 流媒体</button>
    <button type="button" class="sec flash-test-btn" data-mode="backup">💾 备份</button>
  </div>
</div>

<!-- ── 正在运行的任务 ────────────────────────────────────────── -->
<div class="card">
  <div class="card-hd">
    <div class="card-hd-left">
      <div class="h2-icon">🟢</div>
      <h2>正在执行的任务 <span id="taskRefreshHint" style="font-size:12px;color:var(--tx3);font-weight:400;margin-left:6px"></span></h2>
    </div>
    <div class="card-hd-right">
      <button type="button" class="warn" id="stopAllTasksBtn" data-count="{{.CancellableTaskCount}}" {{if eq .CancellableTaskCount 0}}disabled{{end}}>{{if gt .CancellableTaskCount 0}}✖ 一键取消全部 ({{.CancellableTaskCount}}){{else}}✖ 一键取消全部{{end}}</button>
    </div>
  </div>
  <div class="toolbar" style="margin-bottom:14px">
    <input id="taskSearchInput" placeholder="搜索任务ID / 客户端 / 模式 / 模板">
    <select id="taskStatusFilter"><option value="">全部状态</option><option value="scheduled">scheduled</option><option value="pending">pending</option><option value="running">running</option><option value="stopping">stopping</option><option value="stopped">stopped</option><option value="done">done</option></select>
    <select id="taskModeFilter"><option value="">全部模式</option><option value="upload">upload</option><option value="download">download</option><option value="traditional">traditional</option><option value="browse">browse</option><option value="stream">stream</option><option value="backup">backup</option><option value="peer">peer</option></select>
    <select id="taskClientFilter"><option value="">全部客户端</option>{{range .ApprovedClients}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select>
  </div>
  <div class="tbl-wrap">
  <table>
    <thead><tr><th>客户端</th><th>模式</th><th>速率 ↑↓</th><th>时长</th><th>密度</th><th>状态</th><th>进度</th><th>延迟</th><th>已传</th><th>已拉</th><th>日期</th><th>操作</th></tr></thead>
    <tbody id="runningTaskBody">
    {{range .RunningTasks}}
    <tr data-task-id="{{.ID}}" data-status="{{.Status}}">
      <td data-label="客户端">{{if .PeerRole}}{{if eq .PeerRole "source"}}{{index $.ClientNames .ClientID}} <span class="peer-badge">源端</span>{{else}}{{index $.ClientNames .ClientID}} <span class="peer-badge">目标</span>{{end}}{{else}}{{index $.ClientNames .ClientID}}{{end}}</td>
      <td data-label="模式">{{modeLabel .Mode}}</td>
      <td data-label="速率">↑{{.UpMbps}}/↓{{.DownMbps}}</td>
      <td data-label="时长">{{.DurationSec | fmtDurationHTML}}</td>
      <td data-label="密度">{{densityLabel .Density}}</td>
      <td data-label="状态"><span class="badge {{.Status}}">{{.Status}}</span></td>
      <td data-label="进度" class="progress-col">{{buildProgressHTML .}}</td>
      <td data-label="延迟" class="rtt-col" data-client-id="{{.ClientID}}">-</td>
      <td data-label="已传" class="up-col">-</td>
      <td data-label="已拉" class="down-col">-</td>
      <td data-label="日期">{{.StartedAt | shortTime}}</td>
      <td data-label="操作">
        <div style="display:flex;gap:4px">
          <button type="button" class="info view-logs-btn" data-task-id="{{.ID}}">📄 日志</button>
          {{if eq .Status "running"}}<button type="button" class="danger stop-btn" data-task-id="{{.ID}}">🛑 停止</button>
          {{else if eq .Status "pending"}}<button type="button" class="warn stop-btn" data-task-id="{{.ID}}">✖ 取消</button>
          {{else}}<button type="button" class="danger force-del-btn" data-task-id="{{.ID}}">🗑 强制删除</button>{{end}}
        </div>
      </td>
    </tr>
    {{end}}
    {{if eq (len .RunningTasks) 0}}<tr id="noRunningRow"><td colspan="12" style="text-align:center;color:var(--tx3);padding:48px 20px">
      <div style="font-size:30px;margin-bottom:8px;opacity:.35">⚡</div>
      <div style="font-size:14px">暂无正在执行的任务</div>
    </td></tr>{{end}}
    </tbody>
  </table>
  </div>
</div>

<!-- ── 历史任务 ───────────────────────────────────────────────────── -->
<div class="card" id="historyCard" style="display:none">
  <div class="card-hd">
    <div class="card-hd-left">
      <div class="h2-icon">📋</div>
      <h2>历史任务</h2>
    </div>
    <form method="post" action="{{.PanelPath}}/task/clear-history" onsubmit="return confirm('确认清空所有历史任务？')">
      <button type="submit" class="danger sec">清空历史</button>
    </form>
  </div>
  <div class="tbl-wrap">
  <table>
    <thead><tr><th>客户端</th><th>模式</th><th>速率 ↑↓</th><th>时长</th><th>密度</th><th>状态</th><th>已传</th><th>已拉</th><th>开始</th><th>结束</th><th>操作</th></tr></thead>
    <tbody id="historyTaskBody">
    {{range .HistoryTasks}}
    <tr data-task-id="{{.ID}}">
      <td data-label="客户端">{{if .PeerRole}}{{if eq .PeerRole "source"}}{{index $.ClientNames .ClientID}} <span class="peer-badge">源端</span>{{else}}{{index $.ClientNames .ClientID}} <span class="peer-badge">目标</span>{{end}}{{else}}{{index $.ClientNames .ClientID}}{{end}}</td>
      <td data-label="模式">{{modeLabel .Mode}}</td>
      <td data-label="速率">↑{{.UpMbps}}/↓{{.DownMbps}}</td>
      <td data-label="时长">{{.DurationSec | fmtDurationHTML}}</td>
      <td data-label="密度">{{densityLabel .Density}}</td>
      <td data-label="状态"><span class="badge {{.Status}}">{{.Status}}</span></td>
      <td data-label="已传">{{printf "%.3f" (divf .UploadBytes 1073741824)}} GB</td>
      <td data-label="已拉">{{printf "%.3f" (divf .DownloadBytes 1073741824)}} GB</td>
      <td data-label="开始">{{.StartedAt | shortTime}}</td>
      <td data-label="结束">{{.FinishedAt | shortTime}}</td>
      <td data-label="操作">
        <div style="display:flex;gap:4px;flex-wrap:wrap">
          <button type="button" class="info view-logs-btn" data-task-id="{{.ID}}">📄 日志</button>
          <button type="button" class="danger del-task-btn" data-task-id="{{.ID}}">🗑 删除</button>
          {{if eq .PeerRole ""}}<button type="button" class="sec clone-btn" data-id="{{.ID}}" data-client="{{.ClientID}}" data-mode="{{.Mode}}" data-up="{{.UpMbps}}" data-down="{{.DownMbps}}" data-dur="{{.DurationSec}}" data-density="{{.Density}}" data-max-retries="{{.MaxRetries}}" data-template-name="{{.TemplateName}}">🔄 克隆</button>
          <button type="button" class="sec save-template-row-btn" data-mode="{{.Mode}}" data-up="{{.UpMbps}}" data-down="{{.DownMbps}}" data-dur="{{.DurationSec}}" data-density="{{.Density}}" data-max-retries="{{.MaxRetries}}" data-template-name="{{.TemplateName}}">💾 模板</button>{{end}}
        </div>
      </td>
    </tr>
    {{end}}
    {{if eq (len .HistoryTasks) 0}}<tr id="noHistoryRow"><td colspan="11" style="text-align:center;color:var(--tx3);padding:48px 20px">
      <div style="font-size:30px;margin-bottom:8px;opacity:.35">📋</div>
      <div style="font-size:14px">暂无历史记录</div>
    </td></tr>{{end}}
    </tbody>
  </table>
  </div>
</div>

</div><!-- /wrap -->

<script>
(function(){
var PANEL_PATH = {{.PanelPathJS}};
var INIT_TOKEN  = {{.InitTokenJS}};
var PANEL_ADDR  = location.host;
var VERSION     = {{.VersionJS}};
var TASK_TEMPLATES = {{.TemplatesJS}};
var FAST_POLL_INTERVAL_MS = 5000;
var IDLE_POLL_INTERVAL_MS = 15000;

// ── 通用 fetch 封装，强制返回 JSON 并自动带 credentials ──
function apiFetch(path, body) {
  var opts = {
    method: 'POST',
    credentials: 'include',
    headers: {
      'Content-Type': 'application/x-www-form-urlencoded',
      'Accept': 'application/json'
    }
  };
  if (body) opts.body = body;
  return fetch(PANEL_PATH + path, opts).then(function(r) {
    if (!r.ok) return r.text().then(function(t) { throw new Error(t || r.statusText); });
    return r.json();
  });
}

function bindClick(id, fn) {
  var el = document.getElementById(id);
  if (el) el.addEventListener('click', fn);
}

function escapeHTML(value) {
  var BACKTICK_CHAR = String.fromCharCode(96);
  var ESCAPE_RE = new RegExp("[&<>\"'" + BACKTICK_CHAR + "]", 'g');
  return String(value == null ? '' : value).replace(ESCAPE_RE, function(ch) {
    if (ch === BACKTICK_CHAR) return '&#96;';
    return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch];
  });
}

var clientNameMap = {};
document.querySelectorAll('#clientBody tr[data-client-id]').forEach(function(row) {
  clientNameMap[row.dataset.clientId] = row.dataset.name || row.dataset.clientId;
});

function getTaskClientCheckboxes() {
  return document.querySelectorAll('#clientBody .task-client-checkbox');
}

function syncClientSelectAllToggle() {
  var toggle = document.getElementById('clientSelectAllToggle');
  if (!toggle) return;
  var checkboxes = Array.prototype.slice.call(getTaskClientCheckboxes());
  if (checkboxes.length === 0) {
    toggle.checked = false;
    toggle.indeterminate = false;
    toggle.disabled = true;
    return;
  }
  var checkedCount = checkboxes.filter(function(cb) { return cb.checked; }).length;
  toggle.disabled = false;
  toggle.checked = checkedCount === checkboxes.length;
  toggle.indeterminate = checkedCount > 0 && checkedCount < checkboxes.length;
}

function getSelectedClientIDs() {
  var ids = [];
  getTaskClientCheckboxes().forEach(function(cb) {
    if (cb.checked) ids.push(cb.value);
  });
  return ids;
}

function setSelectedClientIDs(ids) {
  var wanted = {};
  ids.forEach(function(id) { wanted[id] = true; });
  getTaskClientCheckboxes().forEach(function(cb) {
    cb.checked = !!wanted[cb.value];
  });
  syncClientSelectAllToggle();
}

function selectedClientIDsCSV() {
  return getSelectedClientIDs().join(',');
}

function buildTaskRequestBody(form) {
  var params = new URLSearchParams(new FormData(form));
  var scheduledInput = document.getElementById('scheduledAtInput');
  var scheduledRFC = document.getElementById('scheduledAtRFC3339');
  if (scheduledInput && scheduledRFC) {
    if (scheduledInput.value) {
      var scheduledDate = new Date(scheduledInput.value);
      scheduledRFC.value = isNaN(scheduledDate.getTime()) ? '' : scheduledDate.toISOString();
    } else {
      scheduledRFC.value = '';
    }
    if (scheduledRFC.value) params.set('scheduled_at', scheduledRFC.value);
    else params.delete('scheduled_at');
  }
  getSelectedClientIDs().forEach(function(id) {
    params.append('client_id', id);
  });
  return params.toString();
}

function tableColspan(bodyId, fallback) {
  var body = document.getElementById(bodyId);
  if (!body) return fallback;
  var table = body.closest('table');
  if (!table) return fallback;
  var count = table.querySelectorAll('thead th').length;
  return count || fallback;
}

function syncClientSelectionCell(row, approved) {
  var cell = row.querySelector('.select-col');
  if (!cell) return;
  var checkbox = cell.querySelector('.task-client-checkbox');
  var keepChecked = !!(checkbox && checkbox.checked);
  if (!approved) {
    cell.textContent = '-';
    return;
  }
  if (!checkbox) {
    cell.innerHTML = '<input type="checkbox" class="task-client-checkbox" style="width:15px;height:15px;margin:0;cursor:pointer;accent-color:var(--primary)">';
    checkbox = cell.querySelector('.task-client-checkbox');
  }
  checkbox.value = row.dataset.clientId;
  checkbox.setAttribute('aria-label', '选择客户端 ' + (row.dataset.name || row.dataset.clientId));
  checkbox.checked = keepChecked;
}

function syncClientApprovalUI(row, approved) {
  row.dataset.approved = approved ? '1' : '0';
  var approvalCell = row.querySelector('.approve-col');
  if (approvalCell) {
    approvalCell.innerHTML = approved
      ? '<span class="badge done">YES</span>'
      : '<span class="badge pending">NO</span>';
  }
  syncClientSelectionCell(row, approved);
  var actions = row.querySelector('.client-actions');
  if (!actions) return;
  var approveBtn = actions.querySelector('.approve-btn');
  if (approved) {
    if (approveBtn) approveBtn.remove();
    return;
  }
  if (!approveBtn) {
    approveBtn = document.createElement('button');
    approveBtn.type = 'button';
    approveBtn.className = 'approve-btn';
    approveBtn.dataset.id = row.dataset.clientId;
    approveBtn.style.background = 'var(--ok)';
    approveBtn.textContent = '批准';
    actions.insertBefore(approveBtn, actions.firstChild);
  }
}

function fmtGB(bytes) {
  if (!bytes || bytes === 0) return '0.000 GB';
  return (bytes / 1073741824).toFixed(3) + ' GB';
}
function modeLabel(mode) {
  switch (mode) {
    case 'upload':
      return '上传';
    case 'download':
      return '下载';
    case 'both':
    case 'traditional':
      return '传统';
    case 'browse':
      return '网页浏览';
    case 'stream':
      return '流媒体';
    case 'backup':
      return '备份同步';
    case 'peer':
      return '对传';
    default:
      return mode || '-';
  }
}
function densityLbl(d) {
  d = parseInt(d) || 0;
  if (d === 30) return '30% 间歇';
  if (d === 70) return '70% 间歇';
  return '无间歇';
}
function renderPing(lat, lastSeen) {
  if (!lastSeen) return { text: '?', cls: 'ping-warn' };
  var t = new Date(lastSeen);
  if (isNaN(t) || Date.now() - t.getTime() > 180000) {
    return { text: '离线', cls: 'ping-dead' }; // 超过 3 分钟心跳未到视为离线
  }
  if (!lat || lat <= 0) {
    return { text: '-', cls: 'ping-warn' };
  }
  var cls = 'ping-ok';
  if (lat > 200) cls = 'ping-warn';
  if (lat > 500) cls = 'ping-dead';
  return { text: lat + ' ms', cls: cls };
}

function syncRunningTaskLatency() {
  document.querySelectorAll('#runningTaskBody .rtt-col[data-client-id]').forEach(function(cell) {
    var cid = cell.dataset.clientId;
    var crow = document.querySelector('#clientBody tr[data-client-id="' + cid + '"]');
    var lat = crow ? parseInt(crow.dataset.latency) || 0 : 0;
    var lastSeen = crow ? crow.dataset.lastSeen : null;
    var r = renderPing(lat, lastSeen);
    cell.textContent = r.text;
    cell.className = 'rtt-col ' + r.cls;
  });
}

function tickPing() {
  document.querySelectorAll('#clientBody tr[data-client-id]').forEach(function(row) {
    var cell = row.querySelector('.ping-col');
    if (!cell) return;
    var lat = parseInt(row.dataset.latency) || 0;
    var r = renderPing(lat, row.dataset.lastSeen);
    cell.textContent = r.text;
    cell.className = 'ping-col ' + r.cls;
  });
  syncRunningTaskLatency();
}
// 数据刷新时同步更新一次这块 UI 就够了，不需要单独的计时器高频刷了

function fmtShortTime(s) {
  if (!s) return '';
  var parts = s.split(/[-T:+]/);
  if (parts.length >= 5) {
    return parts[1] + '-' + parts[2] + ' ' + parts[3] + ':' + parts[4];
  }
  return s;
}

function fmtDuration(secs) {
  if (secs < 60) return secs + ' 秒';
  if (secs < 3600) return Math.floor(secs / 60) + ' 分钟';
  if (secs < 86400) return (secs / 3600).toFixed(1) + ' 小时';
  return (secs / 86400).toFixed(1) + ' 天';
}

function durationToSeconds(val, unit) {
  var num = parseInt(val, 10) || 0;
  if (unit === 'min') return num * 60;
  if (unit === 'hour') return num * 3600;
  if (unit === 'day') return num * 86400;
  if (unit === 'month') return num * 86400 * 30;
  return num;
}

function buildProgress(t) {
  if (t.status === 'pending') return '<div class="progress-bar-container"><div class="progress-bar-fill" style="width:0%"></div></div><div style="font-size:10px;margin-top:2px;color:var(--tx3)">等待中 (0%)</div>';
  if (t.status !== 'running' && t.status !== 'stopping') return '-';
  if (!t.started_at) return '-';
  var st = new Date(t.started_at).getTime();
  if (isNaN(st)) return '-';
  var elapsed = (Date.now() - st) / 1000;
  var pct = Math.min((elapsed / t.duration_sec) * 100, 100);
  return '<div class="progress-bar-container"><div class="progress-bar-fill" style="width:'+pct.toFixed(1)+'%"></div></div><div style="font-size:10px;margin-top:2px;color:var(--primary)">'+pct.toFixed(1)+'%</div>';
}

// 动态行：停止按钮输出为 form，保证无 JS 也可提交
function buildRunningRow(t) {
  var name = clientNameMap[t.client_id] || t.client_name || t.client_id;
  if (t.peer_role === 'source') name += ' <span class="peer-badge">源端</span>';
  else if (t.peer_role === 'target') name += ' <span class="peer-badge">目标</span>';
  var stopBtn = t.status === 'running'
    ? '<button type="button" class="danger stop-btn" data-task-id="'+t.id+'">🛑 停止</button>'
    : t.status === 'pending'
    ? '<button type="button" class="warn stop-btn" data-task-id="'+t.id+'">✖ 取消</button>'
    : '<button type="button" class="danger force-del-btn" data-task-id="'+t.id+'">🗑 强制删除</button>';
  
  return '<tr data-task-id="' + t.id + '" data-status="' + t.status + '">'
    + '<td data-label="客户端">' + name + '</td>'
    + '<td data-label="模式">' + modeLabel(t.mode) + '</td>'
    + '<td data-label="速率">↑' + t.up_mbps + '/↓' + t.down_mbps + '</td>'
    + '<td data-label="时长">' + fmtDuration(t.duration_sec) + '</td>'
    + '<td data-label="密度">' + densityLbl(t.density) + '</td>'
    + '<td data-label="状态"><span class="badge ' + t.status + '">' + t.status + '</span></td>'
    + '<td data-label="进度" class="progress-col">' + buildProgress(t) + '</td>'
    + '<td data-label="网络" class="rtt-col" data-client-id="' + t.client_id + '">-</td>'
    + '<td data-label="已传" class="up-col">' + fmtGB(t.upload_bytes) + '</td>'
    + '<td data-label="已拉" class="down-col">' + fmtGB(t.download_bytes) + '</td>'
    + '<td data-label="开始">' + fmtShortTime(t.started_at) + '</td>'
    + '<td data-label="操作"><div style="display:flex;gap:4px">'
    + '<button type="button" class="info view-logs-btn" data-task-id="'+t.id+'">📄 日志</button>'
    + stopBtn
    + '</div></td>'
    + '</tr>';
}

// 动态行：删除按钮输出为 form，保证无 JS 也可提交
function buildHistoryRow(t) {
  var name = clientNameMap[t.client_id] || t.client_name || t.client_id;
  if (t.peer_role === 'source') name += ' <span class="peer-badge">源端</span>';
  else if (t.peer_role === 'target') name += ' <span class="peer-badge">目标</span>';
  var cloneBtn = t.peer_role ? '' :
    '<button type="button" class="sec clone-btn" data-id="'+escapeHTML(t.id)+'" data-client="'+escapeHTML(t.client_id)+'" data-mode="'+escapeHTML(t.mode)+'" data-up="'+t.up_mbps+'" data-down="'+t.down_mbps+'" data-dur="'+t.duration_sec+'" data-density="'+(t.density||0)+'" data-max-retries="'+(t.max_retries||0)+'" data-template-name="'+escapeHTML(t.template_name||'')+'">🔄 克隆</button>'
    + '<button type="button" class="sec save-template-row-btn" data-mode="'+escapeHTML(t.mode)+'" data-up="'+t.up_mbps+'" data-down="'+t.down_mbps+'" data-dur="'+t.duration_sec+'" data-density="'+(t.density||0)+'" data-max-retries="'+(t.max_retries||0)+'" data-template-name="'+escapeHTML(t.template_name||'')+'">💾 模板</button>';
  return '<tr data-task-id="' + t.id + '">'
    + '<td data-label="客户端">' + name + '</td>'
    + '<td data-label="模式">' + modeLabel(t.mode) + '</td>'
    + '<td data-label="速率">↑' + t.up_mbps + '/↓' + t.down_mbps + '</td>'
    + '<td data-label="时长">' + fmtDuration(t.duration_sec) + '</td>'
    + '<td data-label="密度">' + densityLbl(t.density) + '</td>'
    + '<td data-label="状态"><span class="badge ' + t.status + '">' + t.status + '</span></td>'
    + '<td data-label="已传">' + fmtGB(t.upload_bytes) + '</td>'
    + '<td data-label="已拉">' + fmtGB(t.download_bytes) + '</td>'
    + '<td data-label="开始">' + fmtShortTime(t.started_at) + '</td>'
    + '<td data-label="结束">' + fmtShortTime(t.finished_at) + '</td>'
    + '<td data-label="操作"><div style="display:flex;gap:4px;flex-wrap:wrap">'
    + '<button type="button" class="info view-logs-btn" data-task-id="'+t.id+'">📄 日志</button>'
    + '<button type="button" class="danger del-task-btn" data-task-id="'+t.id+'">🗑 删除</button>'
    + cloneBtn
    + '</div></td>'
    + '</tr>';
}

function updateStats(data) {
  var clients = data.clients || [];
  var tasks = data.tasks || [];
  var online = clients.filter(function(c) {
    if (!c.last_seen) return false;
    return (Date.now() - new Date(c.last_seen).getTime()) < 180000;
  }).length;
  var running = tasks.filter(function(t) { return t.status === 'running'; }).length;
  var pending = tasks.filter(function(t) { return t.status === 'pending'; }).length;
  var totalTraffic = tasks.reduce(function(a, b) { return a + (b.upload_bytes || 0) + (b.download_bytes || 0); }, 0);

  var elOnline = document.getElementById('statOnline');
  var elRunning = document.getElementById('statRunning');
  var elPending = document.getElementById('statPending');
  var elTraffic = document.getElementById('statTraffic');
  if (elOnline) elOnline.textContent = online;
  if (elRunning) elRunning.textContent = running;
  if (elPending) elPending.textContent = pending;
  if (elTraffic) elTraffic.textContent = fmtGB(totalTraffic);
}

function getTaskFilters() {
  var qEl = document.getElementById('taskSearchInput');
  var statusEl = document.getElementById('taskStatusFilter');
  var modeEl = document.getElementById('taskModeFilter');
  var clientEl = document.getElementById('taskClientFilter');
  return {
    q: qEl ? qEl.value.trim().toLowerCase() : '',
    status: statusEl ? statusEl.value : '',
    mode: modeEl ? modeEl.value : '',
    clientId: clientEl ? clientEl.value : ''
  };
}

function getActiveTasks(tasks) {
  return (tasks || []).filter(function(t) {
    return t.status === 'running' || t.status === 'pending';
  });
}

function syncStopAllTasksButton(tasks) {
  var btn = document.getElementById('stopAllTasksBtn');
  if (!btn) return;
  var activeTasks = getActiveTasks(tasks);
  btn.disabled = activeTasks.length === 0;
  btn.dataset.count = String(activeTasks.length);
  btn.textContent = activeTasks.length > 0 ? '✖ 一键取消全部 (' + activeTasks.length + ')' : '✖ 一键取消全部';
}

function taskLayoutKey(tasks) {
  return tasks.map(function(t) {
    return [t.id, t.status, t.client_id, t.started_at || '', t.finished_at || ''].join(':');
  }).join('|');
}

var pollTimer = null;
var pollInFlight = false;
var queuedPoll = false;
var sseConnected = false;
var taskViewKey = '';

function scheduleNextPoll(delay) {
  if (pollTimer) clearTimeout(pollTimer);
  pollTimer = setTimeout(function() { pollData(); }, delay);
}

function pollData() {
  if (pollInFlight) {
    queuedPoll = true;
    return;
  }
  pollInFlight = true;
  var filters = getTaskFilters();
  var query = new URLSearchParams();
  if (filters.q) query.set('q', filters.q);
  if (filters.status) query.set('status', filters.status);
  if (filters.mode) query.set('mode', filters.mode);
  if (filters.clientId) query.set('client_id', filters.clientId);
  var apiURL = '/api/data' + (query.toString() ? '?' + query.toString() : '');
  fetch(apiURL, {credentials: 'include', cache: 'no-store'})
    .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
    .then(function(data) {
      var tasks = data.tasks || [];
      var runningStatuses = ['running', 'pending', 'stopping'];
      var runningTasks = tasks.filter(function(t) { return runningStatuses.indexOf(t.status) !== -1; });
      var historyTasks = tasks.filter(function(t) { return runningStatuses.indexOf(t.status) === -1; });
      var nextTaskViewKey = taskLayoutKey(runningTasks) + '//' + taskLayoutKey(historyTasks);

      if (nextTaskViewKey !== taskViewKey) {
        var rb = document.getElementById('runningTaskBody');
        var runningColspan = tableColspan('runningTaskBody', 12);
        if (rb) rb.innerHTML = runningTasks.length === 0
          ? '<tr id="noRunningRow"><td colspan="' + runningColspan + '" style="text-align:center;color:var(--tx3);padding:20px">暂无正在执行的任务</td></tr>'
          : runningTasks.map(buildRunningRow).join('');
        var hb = document.getElementById('historyTaskBody');
        var historyColspan = tableColspan('historyTaskBody', 11);
        if (hb) hb.innerHTML = historyTasks.length === 0
          ? '<tr id="noHistoryRow"><td colspan="' + historyColspan + '" style="text-align:center;color:var(--muted);padding:20px">暂无历史任务</td></tr>'
          : historyTasks.map(buildHistoryRow).join('');
      } else {
        runningTasks.forEach(function(t) {
          var row = document.querySelector('#runningTaskBody [data-task-id="' + t.id + '"]');
          if (!row) return;
          var pCol = row.querySelector('.progress-col');
          var upCol = row.querySelector('.up-col');
          var dnCol = row.querySelector('.down-col');
          if (pCol) pCol.innerHTML = buildProgress(t);
          if (upCol) upCol.textContent = fmtGB(t.upload_bytes);
          if (dnCol) dnCol.textContent = fmtGB(t.download_bytes);
        });
      }
      taskViewKey = nextTaskViewKey;

      var clients = data.clients || [];
      var shouldReloadClients = clients.length !== document.querySelectorAll('#clientBody tr[data-client-id]').length;
      if (!shouldReloadClients) {
        shouldReloadClients = clients.some(function(c) {
          return !document.querySelector('#clientBody [data-client-id="' + c.id + '"]');
        });
      }
      if (shouldReloadClients) {
        window.location.reload();
        return;
      }
      clients.forEach(function(c) {
        var row = document.querySelector('#clientBody [data-client-id="' + c.id + '"]');
        if (!row) return;
        row.dataset.name = c.name || row.dataset.name || '';
        row.dataset.remark = c.remark || '';
        row.dataset.groupName = c.group_name || '';
        row.dataset.tags = c.tags || '';
        if (c.name) clientNameMap[c.id] = c.name;
        var nameCell = row.querySelector('.name-col');
        if (nameCell) {
          var mainEl = nameCell.querySelector('.name-cell-main');
          var subEl = nameCell.querySelector('.name-cell-sub');
          if (mainEl && c.name !== undefined) mainEl.textContent = c.name || c.id;
          if (subEl) {
            var subParts = [];
            if (c.group_name) subParts.push('<span>📁 ' + escapeHTML(c.group_name) + '</span>');
            if (c.tags) subParts.push('<span>🏷 ' + escapeHTML(c.tags) + '</span>');
            if (c.remark) subParts.push('<span style="font-style:italic">' + escapeHTML(c.remark) + '</span>');
            subEl.innerHTML = subParts.join('');
          }
        }
        syncClientApprovalUI(row, !!c.approved);
        row.dataset.lastSeen = c.last_seen || '';
        var lsCol = row.querySelector('.lastseen-col');
        if (lsCol) lsCol.textContent = fmtShortTime(c.last_seen || '');
        if (c.latency !== undefined) {
          row.dataset.latency = c.latency;
        }
        if (c.ssh_attempts !== undefined) {
          row.dataset.sshAttempts = c.ssh_attempts;
          var sshCell = row.querySelector('.sshattempt-col');
          if (sshCell) sshCell.textContent = c.ssh_attempts;
        }
        var ipCell = row.querySelector('.ip-col');
        if (ipCell && c.remote_ip !== undefined) ipCell.textContent = c.remote_ip || '';
        var ctCell = row.querySelector('.curtask-col');
        if (ctCell) ctCell.textContent = c.current_task || '';
      });
      syncClientSelectAllToggle();
      tickPing();

      updateStats(data);
      syncStopAllTasksButton(tasks);
      var hint = document.getElementById('taskRefreshHint');
      if (hint) hint.textContent = '(上次同步: ' + new Date().toLocaleTimeString() + ')';
    })
    .catch(function(){})
    .finally(function() {
      pollInFlight = false;
      if (queuedPoll) {
        queuedPoll = false;
        pollData();
        return;
      }
      scheduleNextPoll(sseConnected ? IDLE_POLL_INTERVAL_MS : FAST_POLL_INTERVAL_MS);
    });
}
pollData();
tickPing();

var es = new EventSource(PANEL_PATH + '/events');
var liveStatus = document.getElementById('liveStatus');
es.onmessage = function(e) {
  if (e.data === 'ready') {
    sseConnected = true;
    if (liveStatus) liveStatus.textContent = '实时消息流已连接，空闲时将降低轮询频率。';
    return;
  }
  if (e.data !== 'ping' && e.data !== 'ready') {
    pollData();
    if (liveStatus) liveStatus.textContent = '检测到状态变化，已立即刷新。';
  }
};
es.onerror = function() {
  sseConnected = false;
  if (liveStatus) liveStatus.textContent = '实时消息流异常，仍会每 ' + (FAST_POLL_INTERVAL_MS / 1000) + ' 秒自动刷新数据。';
};

// ── 编辑客户端弹窗 ──
var editClientId = '';
var pushUpgradeClientId = '';
function delegate(id, className, fn) {
  var el = document.getElementById(id);
  if (!el) return;
  el.addEventListener('click', function(e) {
    var target = e.target;
    while (target && target !== el) {
      if (target.classList && target.classList.contains(className)) {
        fn(target);
        return;
      }
      target = target.parentNode;
    }
  });
}
document.getElementById('closeEditBtn').addEventListener('click', function() {
  document.getElementById('editModal').classList.remove('open');
});

delegate('clientBody', 'edit-btn', function(target) {
  var row = target.closest('tr');
  editClientId = target.dataset.id;
  document.getElementById('editName').value   = (row && row.dataset.name) || target.dataset.name || '';
  document.getElementById('editRemark').value = (row && row.dataset.remark) || target.dataset.remark || '';
  document.getElementById('editGroupName').value = (row && row.dataset.groupName) || target.dataset.groupName || '';
  document.getElementById('editTags').value = (row && row.dataset.tags) || target.dataset.tags || '';
  document.getElementById('editModal').classList.add('open');
});

delegate('clientBody', 'upgrade-btn', function(target) {
  var clientName = target.dataset.name || '';
  var panelUrl   = location.protocol + '//' + PANEL_ADDR;
  var cmd = "curl --proto '=https' --tlsv1.2 -fsSL "
    + "https://raw.githubusercontent.com/ctsunny/bwtest/main/scripts/install_client.sh"
    + " | bash -s -- "
    + " --server-url " + panelUrl
    + " --init-token " + INIT_TOKEN
    + " --client-name '" + clientName.replace(/'/g, "'\\''") + "'"
    + " --version " + (VERSION || 'latest');
  document.getElementById('upgradeCmd').value = cmd;
  document.getElementById('upgradeModal').classList.add('open');
});

delegate('clientBody', 'push-upgrade-btn', function(target) {
  pushUpgradeClientId = target.dataset.id || '';
  document.getElementById('pushUpgradeClientName').value = target.dataset.name || pushUpgradeClientId;
  document.getElementById('pushUpgradeVersion').value = VERSION || 'latest';
  document.getElementById('pushUpgradeModal').classList.add('open');
});

function taskAction(path, taskId, msg) {
  if (msg && !confirm(msg)) return;
  apiFetch(path, 'task_id=' + encodeURIComponent(taskId))
    .then(function() { pollData(); })
    .catch(function(err) { alert('操作失败: ' + err); });
}

delegate('runningTaskBody', 'view-logs-btn', viewLogs);
delegate('historyTaskBody', 'view-logs-btn', viewLogs);

delegate('runningTaskBody', 'stop-btn', function(target) {
    taskAction('/task/stop', target.dataset.taskId, '确认停止此任务？');
});
delegate('runningTaskBody', 'force-del-btn', function(target) {
    taskAction('/task/delete', target.dataset.taskId, '确认强制删除此任务？');
});
delegate('historyTaskBody', 'del-task-btn', function(target) {
    taskAction('/task/delete', target.dataset.taskId, '确认删除此记录？');
});

function viewLogs(target) {
    var tid = target.dataset.taskId;
    var content = document.getElementById('logsContent');
    content.textContent = '正在从服务器加载日志...';
    document.getElementById('logsModal').classList.add('open');
    fetch(PANEL_PATH + '/task/logs?task_id=' + tid, { credentials: 'include' })
        .then(function(r) { return r.json(); })
        .then(function(d) {
            content.textContent = d.logs || '（该任务暂无可用日志，可能任务尚在队列中或客户端尚未上报）';
            content.scrollTop = content.scrollHeight;
        })
        .catch(function(err) { content.textContent = '加载失败: ' + err; });
}

bindClick('closePushUpgradeBtn', function() {
  document.getElementById('pushUpgradeModal').classList.remove('open');
});
bindClick('confirmPushUpgradeBtn', function() {
  var ver = document.getElementById('pushUpgradeVersion').value.trim();
  var confirmBtn = document.getElementById('confirmPushUpgradeBtn');
  confirmBtn.disabled = true;
  apiFetch('/client/upgrade',
    'client_id=' + encodeURIComponent(pushUpgradeClientId) +
    '&version='   + encodeURIComponent(ver))
    .then(function(d) {
      document.getElementById('pushUpgradeModal').classList.remove('open');
      alert('✅ 已推送更新指令（版本: ' + (d.version || ver) + '）\n客户端将在下次心跳后自动升级（约20秒内）。');
    })
    .catch(function(err) {
      alert('推送失败: ' + err);
    })
    .finally(function() { confirmBtn.disabled = false; });
});

bindClick('upgradeAllBtn', function() {
  if (!confirm("确定要向所有客户端推送升至最新版的指令吗？\n所有客户端将在 20 秒内自动下载并重启服务。")) return;
  var btn = document.getElementById('upgradeAllBtn');
  btn.disabled = true;
  var oldText = btn.textContent;
  btn.textContent = "推送中...";
  var targetVer = VERSION || '';
  apiFetch('/client/upgrade', 'client_id=all&version=' + encodeURIComponent(targetVer))
    .then(function(d) {
      alert('✅ 已向所有客户端推送批量更新指令（目标版本: ' + (d.version || targetVer) + '）。\n客户端将在收到下一次心跳时完成自动更新。');
    })
    .catch(function(err) {
      alert('推送批量更新失败: ' + err);
    })
    .finally(function() { 
      btn.disabled = false; 
      btn.textContent = oldText;
    });
});

delegate('clientBody', 'approve-btn', function(target) {
  apiFetch('/approve', 'client_id=' + encodeURIComponent(target.dataset.id))
    .then(function() { pollData(); })
    .catch(function(err) { alert('批准失败: ' + err); });
});

delegate('clientBody', 'del-client-btn', function(target) {
  if (!confirm('确认彻底删除此客户端？')) return;
  apiFetch('/client/delete', 'client_id=' + encodeURIComponent(target.dataset.id))
    .then(function() { pollData(); })
    .catch(function(err) { alert('删除失败: ' + err); });
});

delegate('historyTaskBody', 'clone-btn', function(target) {
    setSelectedClientIDs([target.dataset.client]);
    document.querySelector('select[name="mode"]').value = target.dataset.mode;
    document.querySelector('input[name="up_mbps"]').value = target.dataset.up;
    document.querySelector('input[name="down_mbps"]').value = target.dataset.down;
    document.querySelector('input[name="duration_val"]').value = target.dataset.dur;
    document.querySelector('input[name="max_retries"]').value = target.dataset.maxRetries || '0';
    document.getElementById('templateNameInput').value = target.dataset.templateName || '';
    document.getElementById('scheduledAtInput').value = '';
    document.querySelector('select[name="duration_unit"]').value = 'sec';
    var densityVal = target.dataset.density || '0';
    var densityRadio = document.querySelector('input[name="density"][value="' + densityVal + '"]');
    if (densityRadio) densityRadio.checked = true;
    var form = document.querySelector('form[action$="/task/create"]');
    if (form) window.scrollTo({ top: form.offsetTop - 100, behavior: 'smooth' });
});

function saveTemplateFromData(data) {
  var name = prompt('模板名称', data.templateName || '');
  if (!name) return;
  apiFetch('/task/template/save',
    'name=' + encodeURIComponent(name) +
    '&mode=' + encodeURIComponent(data.mode) +
    '&up_mbps=' + encodeURIComponent(data.up) +
    '&down_mbps=' + encodeURIComponent(data.down) +
    '&duration_sec=' + encodeURIComponent(data.durationSec) +
    '&density=' + encodeURIComponent(data.density) +
    '&max_retries=' + encodeURIComponent(data.maxRetries || 0))
    .then(function(resp) {
      TASK_TEMPLATES.unshift({
        id: resp.id,
        name: name,
        mode: data.mode,
        up_mbps: Number(data.up),
        down_mbps: Number(data.down),
        duration_sec: Number(data.durationSec),
        density: Number(data.density),
        max_retries: Number(data.maxRetries || 0)
      });
      var sel = document.getElementById('templateSelect');
      if (sel) {
        var opt = document.createElement('option');
        opt.value = resp.id;
        opt.textContent = name;
        opt.dataset.mode = data.mode;
        opt.dataset.up = data.up;
        opt.dataset.down = data.down;
        opt.dataset.duration = data.durationSec;
        opt.dataset.density = data.density;
        opt.dataset.maxRetries = data.maxRetries || 0;
        sel.appendChild(opt);
      }
      alert('模板已保存');
    })
    .catch(function(err) { alert('保存模板失败: ' + err); });
}

delegate('historyTaskBody', 'save-template-row-btn', function(target) {
  saveTemplateFromData({
    mode: target.dataset.mode,
    up: target.dataset.up,
    down: target.dataset.down,
    durationSec: target.dataset.dur,
    density: target.dataset.density || '0',
    maxRetries: target.dataset.maxRetries || '0',
    templateName: target.dataset.templateName || ''
  });
});

document.getElementById('saveEditBtn').addEventListener('click', function() {
  var name   = document.getElementById('editName').value.trim();
  var remark = document.getElementById('editRemark').value.trim();
  var groupName = document.getElementById('editGroupName').value.trim();
  var tags = document.getElementById('editTags').value.trim();
  if (!name) { alert('名称不能为空'); return; }
  var saveBtn = document.getElementById('saveEditBtn');
  saveBtn.disabled = true;
  apiFetch('/client/edit',
    'client_id=' + encodeURIComponent(editClientId) +
    '&name='      + encodeURIComponent(name) +
    '&remark='    + encodeURIComponent(remark) +
    '&group_name=' + encodeURIComponent(groupName) +
    '&tags=' + encodeURIComponent(tags))
    .then(function() {
      document.getElementById('editModal').classList.remove('open');
      pollData();
    })
    .catch(function(err) {
      alert('保存失败: ' + err);
    })
    .finally(function() { saveBtn.disabled = false; });
});
bindClick('closeUpgradeBtn', function() {
  document.getElementById('upgradeModal').classList.remove('open');
});
bindClick('copyUpgradeBtn', function() {
  var el = document.getElementById('upgradeCmd');
  el.select();
  document.execCommand('copy');
  alert('已复制到剪贴板');
});
document.getElementById('copyCmdBtn').addEventListener('click', function() {
  var el = document.getElementById('cmdText');
  el.select();
  document.execCommand('copy');
  alert('已复制到剪贴板');
});
var genForm = document.getElementById('genForm');
if (genForm) genForm.addEventListener('submit', function(e) {
  e.preventDefault();
  var btn = document.getElementById('genBtn');
  if (btn) btn.disabled = true;
  apiFetch('/gen/install-cmd', new URLSearchParams(new FormData(genForm)).toString())
    .then(function(resp) {
      var cmd = resp.generated_cmd || '';
      var cmdBox = document.getElementById('cmdBox');
      var cmdText = document.getElementById('cmdText');
      var cmdTip = document.getElementById('cmdTip');
      if (cmdText) cmdText.value = cmd;
      if (cmdBox) cmdBox.style.display = cmd ? 'flex' : 'none';
      if (cmdTip) {
        if (cmd) {
          var platform = resp.gen_platform || 'linux';
          cmdTip.textContent = platform === 'windows'
            ? '以管理员身份打开 PowerShell，粘贴并执行此命令即可完成安装。'
            : '将此命令复制到客户端 VPS 上执行即可完成安装与注册。';
        } else {
          cmdTip.textContent = '';
        }
      }
      document.getElementById('genModal').classList.add('open');
      if (location.search.indexOf('gen_cmd=') !== -1 && window.history && window.history.replaceState) {
        window.history.replaceState({}, document.title, PANEL_PATH);
      }
    })
    .catch(function(err) { alert('生成命令失败: ' + err); })
    .finally(function() { if (btn) btn.disabled = false; });
});

function closeModalOnBackdrop(modalId) {
  var modal = document.getElementById(modalId);
  if (!modal) return;
  modal.addEventListener('click', function(e) {
    if (e.target === modal) {
      modal.classList.remove('open');
    }
  });
}
closeModalOnBackdrop('editModal');
closeModalOnBackdrop('upgradeModal');
closeModalOnBackdrop('pushUpgradeModal');
closeModalOnBackdrop('genModal');
closeModalOnBackdrop('logsModal');
document.addEventListener('keydown', function(e) {
  if (e.key !== 'Escape') return;
  var opened = document.querySelectorAll('.modal-overlay.open');
  for (var i = 0; i < opened.length; i++) {
    opened[i].classList.remove('open');
  }
});


// \u2500\u2500 \u624b\u52a8\u5237\u65b0\u6309\u9215\u4e8b\u4ef6\u76d1\u542c \u2500\u2500
var reloadBtnEl = document.getElementById('reloadBtn');
if (reloadBtnEl) reloadBtnEl.addEventListener('click', function() {
  pollData();
  if (liveStatus) liveStatus.textContent = '\u624b\u52a8\u5237\u65b0\u5b8c\u6210: ' + new Date().toLocaleTimeString();
});

// ── 下拉菜单 ──
var moreDropdown = document.getElementById('moreDropdown');
var moreBtn = document.getElementById('moreBtn');
if (moreBtn && moreDropdown) {
  moreBtn.addEventListener('click', function(e) {
    e.stopPropagation();
    moreDropdown.classList.toggle('open');
  });
  document.addEventListener('click', function(e) {
    if (!moreDropdown.contains(e.target)) moreDropdown.classList.remove('open');
  });
}

// ── 对传弹窗 ──
var openPeerModalBtn = document.getElementById('openPeerModalBtn');
if (openPeerModalBtn) openPeerModalBtn.addEventListener('click', function() {
  document.getElementById('peerModal').classList.add('open');
});
closeModalOnBackdrop('peerModal');
document.querySelectorAll('.modal-overlay').forEach(function(m) {
  m.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') m.classList.remove('open');
  });
});

// 对传: 互换源端/目标端
bindClick('peerSwapBtn', function() {
  var srcSel = document.getElementById('peerSourceSelect');
  var tgtSel = document.getElementById('peerTargetSelect');
  var tmp = srcSel.value;
  srcSel.value = tgtSel.value;
  tgtSel.value = tmp;
  updatePeerLabels();
  updatePeerEstimate();
});

// 对传: 实时更新连接标签
function updatePeerLabels() {
  var srcSel = document.getElementById('peerSourceSelect');
  var tgtSel = document.getElementById('peerTargetSelect');
  var srcLabel = document.getElementById('peerSrcLabel');
  var tgtLabel = document.getElementById('peerTgtLabel');
  if (srcLabel) srcLabel.textContent = (srcSel && srcSel.value) ? srcSel.options[srcSel.selectedIndex].text : '源端';
  if (tgtLabel) tgtLabel.textContent = (tgtSel && tgtSel.value) ? tgtSel.options[tgtSel.selectedIndex].text : '目标端';
}

// 对传: 预计传输量
function updatePeerEstimate() {
  var mbps = parseFloat(document.getElementById('peerMbps').value) || 0;
  var durVal = parseInt(document.getElementById('peerDurVal').value) || 0;
  var durUnit = document.getElementById('peerDurUnit').value;
  var durSec = durationToSeconds(durVal, durUnit);
  var el = document.getElementById('peerEstimate');
  if (!el) return;
  if (!mbps || !durSec) { el.textContent = '—'; return; }
  var bytes = (mbps * 1000000 / 8) * durSec;
  var gb = bytes / 1073741824;
  el.textContent = gb >= 0.1 ? gb.toFixed(2) + ' GB' : (bytes / 1048576).toFixed(0) + ' MB';
}

(function() {
  var peerSrcSel = document.getElementById('peerSourceSelect');
  var peerTgtSel = document.getElementById('peerTargetSelect');
  var peerMbpsEl = document.getElementById('peerMbps');
  var peerDurValEl = document.getElementById('peerDurVal');
  var peerDurUnitEl = document.getElementById('peerDurUnit');
  if (peerSrcSel) peerSrcSel.addEventListener('change', function() { updatePeerLabels(); updatePeerEstimate(); });
  if (peerTgtSel) peerTgtSel.addEventListener('change', function() { updatePeerLabels(); updatePeerEstimate(); });
  if (peerMbpsEl) peerMbpsEl.addEventListener('input', updatePeerEstimate);
  if (peerDurValEl) peerDurValEl.addEventListener('input', updatePeerEstimate);
  if (peerDurUnitEl) peerDurUnitEl.addEventListener('change', updatePeerEstimate);
})();
updatePeerEstimate();

bindClick('confirmPeerBtn', function() {
  var src = document.getElementById('peerSourceSelect').value;
  var tgt = document.getElementById('peerTargetSelect').value;
  var mbps = document.getElementById('peerMbps').value;
  var durVal = document.getElementById('peerDurVal').value;
  var durUnit = document.getElementById('peerDurUnit').value;
  if (!src) { alert('请选择源端客户端'); return; }
  if (!tgt) { alert('请选择目标端客户端'); return; }
  if (src === tgt) { alert('源端和目标端不能是同一台客户端'); return; }
  var btn = document.getElementById('confirmPeerBtn');
  btn.disabled = true;
  apiFetch('/task/peer-create',
    'source_client_id=' + encodeURIComponent(src) +
    '&target_client_id=' + encodeURIComponent(tgt) +
    '&mbps=' + encodeURIComponent(mbps) +
    '&duration_val=' + encodeURIComponent(durVal) +
    '&duration_unit=' + encodeURIComponent(durUnit))
    .then(function() {
      document.getElementById('peerModal').classList.remove('open');
      pollData();
    })
    .catch(function(err) { alert('创建对传任务失败: ' + err); })
    .finally(function() { btn.disabled = false; });
});

// ── SSH 列显示切换 ──
var sshColVisible = false;
var toggleSshColBtn = document.getElementById('toggleSshColBtn');
if (toggleSshColBtn) toggleSshColBtn.addEventListener('click', function() {
  sshColVisible = !sshColVisible;
  document.querySelectorAll('.ssh-col').forEach(function(el) {
    el.style.display = sshColVisible ? '' : 'none';
  });
  toggleSshColBtn.textContent = sshColVisible ? '🔒 SSH列 ✓' : '🔒 SSH列';
});

var historyOpen = false;
var toggleHistoryBtnEl = document.getElementById('toggleHistoryBtn');
if (toggleHistoryBtnEl) toggleHistoryBtnEl.addEventListener('click', function() {
  historyOpen = !historyOpen;
  var card = document.getElementById('historyCard');
  if (card) card.style.display = historyOpen ? 'block' : 'none';
  this.textContent = historyOpen ? '📜 隐藏历史任务' : '📜 显示历史任务';
});

var taskForm = document.getElementById('taskForm');
if (taskForm) taskForm.addEventListener('submit', function(e) {
  e.preventDefault();
  var selectedIDs = getSelectedClientIDs();
  if (selectedIDs.length === 0) { alert('请先在上方客户端列表勾选至少一个客户端'); return; }
  var body = buildTaskRequestBody(taskForm);
  var btn = document.getElementById('createTaskBtn');
  if (btn) btn.disabled = true;
  apiFetch('/task/create', body)
    .then(function() { pollData(); })
    .catch(function(err) { alert('创建失败: ' + err); })
    .finally(function() { if (btn) btn.disabled = false; });
});

var templateSelectEl = document.getElementById('templateSelect');
if (templateSelectEl) templateSelectEl.addEventListener('change', function() {
  var opt = templateSelectEl.options[templateSelectEl.selectedIndex];
  if (!opt || !opt.value) return;
  document.querySelector('select[name="mode"]').value = opt.dataset.mode || 'upload';
  document.querySelector('input[name="up_mbps"]').value = opt.dataset.up || '1';
  document.querySelector('input[name="down_mbps"]').value = opt.dataset.down || '1';
  document.querySelector('input[name="duration_val"]').value = opt.dataset.duration || '60';
  document.querySelector('select[name="duration_unit"]').value = 'sec';
  document.querySelector('input[name="max_retries"]').value = opt.dataset.maxRetries || '0';
  document.getElementById('templateNameInput').value = opt.textContent || '';
  var densityRadio = document.querySelector('input[name="density"][value="' + (opt.dataset.density || '0') + '"]');
  if (densityRadio) densityRadio.checked = true;
});
bindClick('saveTemplateBtn', function() {
  saveTemplateFromData({
    mode: document.querySelector('select[name="mode"]').value,
    up: document.querySelector('input[name="up_mbps"]').value,
    down: document.querySelector('input[name="down_mbps"]').value,
    durationSec: durationToSeconds(document.querySelector('input[name="duration_val"]').value, document.querySelector('select[name="duration_unit"]').value),
    density: (document.querySelector('input[name="density"]:checked') || {}).value || '0',
    maxRetries: document.querySelector('input[name="max_retries"]').value || '0',
    templateName: document.getElementById('templateNameInput').value || ''
  });
});
['taskSearchInput', 'taskStatusFilter', 'taskModeFilter', 'taskClientFilter'].forEach(function(id) {
  var el = document.getElementById(id);
  if (el) el.addEventListener(id === 'taskSearchInput' ? 'input' : 'change', function() { pollData(); });
});

var sab = document.getElementById('selectAllClientsBtn');
if (sab) sab.addEventListener('click', function() {
  getTaskClientCheckboxes().forEach(function(cb) { cb.checked = true; });
  syncClientSelectAllToggle();
});
var dsab = document.getElementById('deselectAllClientsBtn');
if (dsab) dsab.addEventListener('click', function() {
  getTaskClientCheckboxes().forEach(function(cb) { cb.checked = false; });
  syncClientSelectAllToggle();
});
var clientSelectAllToggle = document.getElementById('clientSelectAllToggle');
if (clientSelectAllToggle) clientSelectAllToggle.addEventListener('change', function() {
  getTaskClientCheckboxes().forEach(function(cb) { cb.checked = clientSelectAllToggle.checked; });
  clientSelectAllToggle.indeterminate = false;
});
var clientBodyEl = document.getElementById('clientBody');
if (clientBodyEl) clientBodyEl.addEventListener('change', function(e) {
  if (e.target && e.target.classList && e.target.classList.contains('task-client-checkbox')) {
    syncClientSelectAllToggle();
  }
});
syncClientSelectAllToggle();

var stopAllTasksBtn = document.getElementById('stopAllTasksBtn');
if (stopAllTasksBtn) stopAllTasksBtn.addEventListener('click', function() {
  var count = parseInt(stopAllTasksBtn.dataset.count || '0', 10) || 0;
  if (count <= 0) {
    alert('当前没有可取消的任务');
    return;
  }
  if (!confirm('确认一键取消全部 ' + count + ' 个任务？')) return;
  stopAllTasksBtn.disabled = true;
  apiFetch('/task/stop-all')
    .then(function() { pollData(); })
    .catch(function(err) { alert('批量取消失败: ' + err); })
    .finally(function() { stopAllTasksBtn.disabled = false; pollData(); });
});
bindClick('bulkRestartClientsBtn', function() {
  var ids = selectedClientIDsCSV();
  if (!ids) { alert('请先选择客户端'); return; }
  if (!confirm('确认批量重启所选客户端服务？')) return;
  apiFetch('/client/bulk-restart', 'client_ids=' + encodeURIComponent(ids))
    .then(function() { alert('已下发重启指令，客户端会在下次心跳后重启'); pollData(); })
    .catch(function(err) { alert('批量重启失败: ' + err); });
});
bindClick('bulkDeleteClientsBtn', function() {
  var ids = selectedClientIDsCSV();
  if (!ids) { alert('请先选择客户端'); return; }
  if (!confirm('确认批量删除所选客户端？')) return;
  apiFetch('/client/bulk-delete', 'client_ids=' + encodeURIComponent(ids))
    .then(function() { pollData(); })
    .catch(function(err) { alert('批量删除失败: ' + err); });
});

document.querySelectorAll('.flash-test-btn').forEach(function(b) {
  b.addEventListener('click', function() {
    var checked = getSelectedClientIDs();
    if (checked.length === 0) { alert('请先在上方客户端列表勾选客户端'); return; }
    document.querySelector('select[name="mode"]').value = b.dataset.mode;
    document.querySelector('input[name="up_mbps"]').value = '1';
    document.querySelector('input[name="down_mbps"]').value = '1';
    document.querySelector('input[name="duration_val"]').value = '1';
    document.querySelector('select[name="duration_unit"]').value = 'min';
    var dr = document.querySelector('input[name="density"][value="0"]');
    if (dr) dr.checked = true;
    taskForm.dispatchEvent(new Event('submit'));
  });
});

if (location.search.indexOf('gen_cmd=') !== -1 && window.history && window.history.replaceState) {
  window.history.replaceState({}, document.title, PANEL_PATH);
}

})();
</script>
</body>
</html>`

		tpl := template.Must(template.New("page").Funcs(template.FuncMap{
			"not": func(b bool) bool { return !b },
			"index": func(m map[string]string, k string) string {
				if v, ok := m[k]; ok {
					return v
				}
				return k
			},
			"divf": func(a int64, b float64) float64 {
				return float64(a) / b
			},
			"modeLabel":    taskModeLabel,
			"densityLabel": densityLabel,
			"shortTime": func(s string) string {
				if s == "" {
					return ""
				}
				t, err := time.Parse(time.RFC3339, s)
				if err != nil {
					return s
				}
				loc := time.FixedZone("CST", 8*3600)
				return t.In(loc).Format("01-02 15:04")
			},
			"fmtDurationHTML": func(s int) string {
				if s < 60 {
					return fmt.Sprintf("%d 秒", s)
				}
				if s < 3600 {
					return fmt.Sprintf("%d 分钟", s/60)
				}
				if s < 86400 {
					return fmt.Sprintf("%.1f 小时", float64(s)/3600.0)
				}
				return fmt.Sprintf("%.1f 天", float64(s)/86400.0)
			},
			"buildProgressHTML": func(t Task) template.HTML {
				if t.Status == "pending" {
					return template.HTML(`<span class="note">0%</span>`)
				}
				if t.Status != "running" && t.Status != "stopping" {
					return template.HTML(`-`)
				}
				if t.StartedAt == "" {
					return template.HTML(`-`)
				}
				ts, err := time.Parse(time.RFC3339, t.StartedAt)
				if err != nil {
					return template.HTML(`-`)
				}
				elapsed := time.Since(ts).Seconds()
				if elapsed < 0 {
					return template.HTML(`0%`)
				}
				pct := (elapsed / float64(t.DurationSec)) * 100
				if pct > 100 {
					pct = 100
				}
				return template.HTML(fmt.Sprintf("%.1f%%", pct))
			},
		}).Parse(page))
		_ = tpl.Execute(w, pageData{
			Clients:              clients,
			RunningTasks:         runningTasks,
			HistoryTasks:         historyTasks,
			TaskTemplates:        templates,
			ApprovedClients:      approvedClients,
			CancellableTaskCount: cancellableTaskCount,
			DefaultClientID:      defaultClientID,
			ClientNames:          clientNames,
			PanelPath:            cfg.PanelPath,
			ServerHost:           cfg.ServerHost,
			InitToken:            cfg.InitToken,
			Version:              version,
			BarkURL:              cfg.BarkURL,
			PanelPathJS:          jsStr(cfg.PanelPath),
			InitTokenJS:          jsStr(cfg.InitToken),
			VersionJS:            jsStr(version),
			TemplatesJS:          template.JS(mustMarshalJSON(templates)),
			GenName:              genName,
			GenRemark:            genRemark,
			GenVersion:           genVersion,
			GenPlatform:          genPlatform,
			GeneratedCmd:         generatedCmd,
		})
	}
}

func handleGenInstallCmd(panelPath string, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		name := strings.TrimSpace(r.Form.Get("gen_name"))
		remark := strings.TrimSpace(r.Form.Get("gen_remark"))
		version := strings.TrimSpace(r.Form.Get("gen_version"))
		platform := strings.TrimSpace(r.Form.Get("gen_platform"))
		if version == "" {
			version = "latest"
		}
		if platform != "windows" {
			platform = "linux"
		}
		q := url.Values{}
		q.Set("gen_name", name)
		q.Set("gen_remark", remark)
		q.Set("gen_version", version)
		q.Set("gen_platform", platform)
		if name != "" {
			panelURL := fmt.Sprintf("%s://%s", requestScheme(r), r.Host)
			var cmd string
			if platform == "windows" {
				// PowerShell one-liner: download and invoke the PS1 install script
				psName := strings.ReplaceAll(name, "'", "''")   // escape single quotes for PS
				psRemark := strings.ReplaceAll(remark, "'", "''")
				scriptURL := "https://raw.githubusercontent.com/ctsunny/bwtest/main/scripts/install_client.ps1"
				cmd = `powershell -NoProfile -ExecutionPolicy Bypass -Command "& ([scriptblock]::Create((Invoke-WebRequest -UseBasicParsing '` +
					scriptURL + `').Content)) -ServerUrl '` + panelURL +
					`' -InitToken '` + cfg.InitToken +
					`' -ClientName '` + psName +
					`' -Version '` + version + `'`
				if remark != "" {
					cmd += ` -Remark '` + psRemark + `'`
				}
				cmd += `"`
			} else {
				cmd = "curl --proto '=https' --tlsv1.2 -fsSL " +
					"https://raw.githubusercontent.com/ctsunny/bwtest/main/scripts/install_client.sh | bash -s --" +
					" --server-url " + panelURL +
					" --init-token " + cfg.InitToken +
					" --client-name '" + strings.ReplaceAll(name, "'", "'\\''") + "'" +
					" --version " + version
				if remark != "" {
					cmd += " --remark '" + strings.ReplaceAll(remark, "'", "'\\''") + "'"
				}
			}
			q.Set("gen_cmd", cmd)
			if strings.Contains(r.Header.Get("Accept"), "application/json") {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok":            true,
					"gen_name":      name,
					"gen_remark":    remark,
					"gen_version":   version,
					"gen_platform":  platform,
					"generated_cmd": cmd,
				})
				return
			}
		}
		http.Redirect(w, r, panelPath+"?"+q.Encode(), http.StatusFound)
	}
}

func requestScheme(r *http.Request) string {
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || r.TLS != nil {
		return "https"
	}
	return "http"
}

func handleDeleteTask(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		taskID := r.Form.Get("task_id")
		_, _ = db.Exec(`DELETE FROM tasks WHERE id=?`, taskID)
		broker.Publish("tasks")
		// 支持 fetch 调用（返回 JSON）和传统 form 跳转
		if strings.Contains(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") &&
			!strings.Contains(r.Header.Get("Accept"), "application/json") {
			http.Redirect(w, r, panelPath, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func handleGetTaskLogs(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID := r.URL.Query().Get("task_id")
		var logs string
		_ = db.QueryRow(`SELECT logs FROM tasks WHERE id=?`, taskID).Scan(&logs)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "logs": logs})
	}
}

func handleSettings(panelPath string, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			token := strings.TrimSpace(r.Form.Get("bark_token"))
			newURL := barkURLFromToken(token)
			_ = os.MkdirAll("/opt/bwtest", 0755)
			_ = os.WriteFile("/opt/bwtest/bark_url", []byte(newURL), 0600)
			cfg.BarkURL = newURL
			http.Redirect(w, r, panelPath, http.StatusFound)
			return
		}
		currentToken := barkTokenFromURL(cfg.BarkURL)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Bark 推送设置 — 带宽测试面板</title>
<style>
@import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&display=swap');
:root{
  --primary:#6366f1;--primary-dk:#4f46e5;--primary-glow:rgba(99,102,241,.15);
  --ok:#10b981;--bg:#f8fafc;--surf:#fff;--surf2:#f1f5f9;--bdr:#e2e8f0;
  --tx:#0f172a;--tx2:#475569;--tx3:#94a3b8;
  --r-sm:8px;--r-lg:16px;--r-pill:9999px;
  --sh:0 1px 3px rgba(0,0,0,.06),0 4px 8px rgba(0,0,0,.04);
}
@media(prefers-color-scheme:dark){
  :root{
    --primary:#818cf8;--primary-dk:#6366f1;--primary-glow:rgba(129,140,248,.15);
    --bg:#060d1b;--surf:#0f172a;--surf2:#1a2236;--bdr:#1e293b;
    --tx:#f1f5f9;--tx2:#94a3b8;--tx3:#475569;
  }
}
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
body{font-family:'Inter',-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Noto Sans CJK SC','Microsoft YaHei',sans-serif;
  background:var(--bg);color:var(--tx);font-size:14px;line-height:1.55;-webkit-font-smoothing:antialiased}
.nav{position:sticky;top:0;z-index:100;height:60px;display:flex;align-items:center;gap:12px;
  padding:0 24px;background:var(--surf);border-bottom:1px solid var(--bdr)}
.nav-icon{width:34px;height:34px;background:linear-gradient(135deg,var(--primary),var(--primary-dk));
  border-radius:9px;display:flex;align-items:center;justify-content:center;font-size:17px;
  box-shadow:0 3px 10px var(--primary-glow);flex-shrink:0}
.nav-title{font-weight:800;font-size:17px;letter-spacing:-.03em}
.page{max-width:560px;margin:0 auto;padding:28px 24px 60px}
.card{background:var(--surf);border:1px solid var(--bdr);border-radius:var(--r-lg);
  padding:28px;margin-bottom:20px;box-shadow:var(--sh)}
.card-hd{display:flex;align-items:center;gap:10px;margin-bottom:6px}
.card-hd-icon{width:30px;height:30px;border-radius:8px;background:var(--surf2);
  display:flex;align-items:center;justify-content:center;font-size:14px}
h2{font-size:16px;font-weight:700;color:var(--tx)}
.desc{font-size:13px;color:var(--tx3);margin-bottom:24px;line-height:1.6}
code{background:var(--surf2);padding:2px 7px;border-radius:5px;font-size:12px;
  font-family:'SF Mono','Fira Code',monospace;border:1px solid var(--bdr)}
.form-group{margin-bottom:18px}
label{display:block;font-size:12px;font-weight:600;color:var(--tx2);margin-bottom:6px}
input{width:100%%;padding:10px 12px;border:1.5px solid var(--bdr);border-radius:var(--r-sm);
  background:var(--surf);color:var(--tx);font-size:14px;font-family:inherit;
  transition:border-color .15s,box-shadow .15s}
input:focus{outline:none;border-color:var(--primary);box-shadow:0 0 0 3px var(--primary-glow)}
.actions{display:flex;align-items:center;gap:12px;flex-wrap:wrap;margin-top:4px}
button{display:inline-flex;align-items:center;gap:5px;padding:10px 18px;border:none;
  border-radius:var(--r-sm);font-size:13px;font-weight:600;cursor:pointer;
  background:var(--primary);color:#fff;font-family:inherit;
  transition:filter .15s,transform .12s}
button:hover{filter:brightness(1.08);transform:translateY(-1px)}
button:active{transform:translateY(0);filter:brightness(.96)}
a.back{font-size:13px;color:var(--tx2);text-decoration:none;display:inline-flex;align-items:center;gap:4px}
a.back:hover{color:var(--primary)}
.hint{font-size:12px;color:var(--tx3);margin-top:16px;padding-top:16px;border-top:1px solid var(--bdr)}
</style>
</head>
<body>
<nav class="nav">
  <div class="nav-icon">⚙️</div>
  <span class="nav-title">推送设置</span>
</nav>
<div class="page">
<div class="card">
  <div class="card-hd">
    <div class="card-hd-icon">🔔</div>
    <h2>Bark 推送设置</h2>
  </div>
  <p class="desc">只需填写 Bark Token，系统会自动拼接为 <code>https://api.day.app/你的token</code>；留空则关闭推送。</p>
  <form method="post">
    <div class="form-group">
      <label>Bark Token</label>
      <input name="bark_token" value="%s" placeholder="例：AbCdEfGhXxXx" autocomplete="off">
    </div>
    <div class="actions">
      <button type="submit">✓ 保存设置</button>
      <a class="back" href="%s">← 返回面板</a>
    </div>
  </form>
  <p class="hint">保存后立即生效，无需重启服务端。配置持久化到 /opt/bwtest/bark_url 文件。</p>
</div>
</div>
</body></html>`, currentToken, panelPath)
	}
}

func handleEvents(b *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", 500)
			return
		}
		ch := b.Subscribe()
		defer b.Unsubscribe(ch)
		fmt.Fprintf(w, "data: ready\n\n")
		flusher.Flush()
		tk := time.NewTicker(20 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case msg := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", msg)
				flusher.Flush()
			case <-tk.C:
				fmt.Fprintf(w, "data: ping\n\n")
				flusher.Flush()
			}
		}
	}
}

func handleApprove(panelPath string, cfg Config, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		clientID := r.Form.Get("client_id")
		_, _ = db.Exec(`UPDATE clients SET approved=1 WHERE id=?`, clientID)
		broker.Publish("clients")
		if strings.Contains(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") &&
			!strings.Contains(r.Header.Get("Accept"), "application/json") {
			http.Redirect(w, r, panelPath, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func handleClientEdit(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		clientID := r.Form.Get("client_id")
		name := strings.TrimSpace(r.Form.Get("name"))
		remark := strings.TrimSpace(r.Form.Get("remark"))
		groupName := strings.TrimSpace(r.Form.Get("group_name"))
		tags := normalizeTags(r.Form.Get("tags"))
		if name != "" {
			_, _ = db.Exec(`UPDATE clients SET name=?, remark=?, group_name=?, tags=? WHERE id=?`, name, remark, groupName, tags, clientID)
		}
		broker.Publish("clients")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func handleCreateTask(panelPath string, cfg *Config, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		clientIDs := r.Form["client_id"]
		if len(clientIDs) == 0 {
			http.Error(w, "no client selected", 400)
			return
		}
		mode := r.Form.Get("mode")
		up, _ := strconv.Atoi(r.Form.Get("up_mbps"))
		down, _ := strconv.Atoi(r.Form.Get("down_mbps"))
		durVal, _ := strconv.Atoi(r.Form.Get("duration_val"))
		durUnit := r.Form.Get("duration_unit")
		dur := durationToSec(durVal, durUnit)
		density, _ := strconv.Atoi(r.Form.Get("density"))
		maxRetries, _ := strconv.Atoi(r.Form.Get("max_retries"))
		scheduledAt := strings.TrimSpace(r.Form.Get("scheduled_at"))
		templateName := strings.TrimSpace(r.Form.Get("template_name"))

		// ── Input validation ──────────────────────────────────────────────────
		const maxMbps = 100000 // 100 Gbps upper bound
		const minDurSec = 5
		const maxDurSec = 86400 * 30 // 30 days

		mode = normalizeTaskMode(mode)
		if !isValidTaskMode(mode) {
			http.Error(w, "invalid mode", http.StatusBadRequest)
			return
		}
		if up < 0 || up > maxMbps {
			http.Error(w, fmt.Sprintf("up_mbps out of range [0, %d]", maxMbps), http.StatusBadRequest)
			return
		}
		if down < 0 || down > maxMbps {
			http.Error(w, fmt.Sprintf("down_mbps out of range [0, %d]", maxMbps), http.StatusBadRequest)
			return
		}
		if dur < minDurSec || dur > maxDurSec {
			http.Error(w, fmt.Sprintf("duration out of range [%ds, %ds]", minDurSec, maxDurSec), http.StatusBadRequest)
			return
		}
		if density != 0 && density != 30 && density != 70 {
			density = 0
		}
		if maxRetries < 0 {
			maxRetries = 0
		}
		status := "pending"
		if scheduledAt != "" {
			if _, err := time.Parse(time.RFC3339, scheduledAt); err != nil {
				http.Error(w, "invalid scheduled_at", http.StatusBadRequest)
				return
			}
			status = "scheduled"
		}
		// ──────────────────────────────────────────────────────────────────────

		now := time.Now().Format(time.RFC3339)

		for _, clientID := range clientIDs {
			id := genToken(8)
			_, err := db.Exec(`INSERT INTO tasks(id,client_id,mode,up_mbps,down_mbps,duration_sec,density,status,created_at,scheduled_at,max_retries,retry_count,parent_task_id,template_name) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				id, clientID, mode, up, down, dur, density, status, now, scheduledAt, maxRetries, 0, "", templateName)
			if err == nil {
				clientName := clientID
				_ = db.QueryRow(`SELECT name FROM clients WHERE id=?`, clientID).Scan(&clientName)
				go barkPush(cfg.BarkURL, "任务已创建",
					fmt.Sprintf("客户端 %s 新任务已创建\n模式:%s 上传:%dMbps 下载:%dMbps 时长:%ds",
						clientName, mode, up, down, dur))
			}
		}
		broker.Publish("tasks")
		// 支持 AJAX 调用（Accept: application/json）和传统 form 跳转
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		http.Redirect(w, r, panelPath, http.StatusFound)
	}
}

func handleSaveTaskTemplate(panelPath string, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		name := strings.TrimSpace(r.Form.Get("name"))
		mode := normalizeTaskMode(r.Form.Get("mode"))
		up, _ := strconv.Atoi(r.Form.Get("up_mbps"))
		down, _ := strconv.Atoi(r.Form.Get("down_mbps"))
		durationSec, _ := strconv.Atoi(r.Form.Get("duration_sec"))
		density, _ := strconv.Atoi(r.Form.Get("density"))
		maxRetries, _ := strconv.Atoi(r.Form.Get("max_retries"))
		if name == "" || !isValidTaskMode(mode) || durationSec <= 0 {
			http.Error(w, "invalid template payload", http.StatusBadRequest)
			return
		}
		id := genToken(8)
		now := time.Now().Format(time.RFC3339)
		_, err := db.Exec(`INSERT INTO task_templates(id,name,mode,up_mbps,down_mbps,duration_sec,density,max_retries,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			id, name, mode, up, down, durationSec, density, maxRetries, now)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
	}
}

func handleStopTask(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		taskID := r.Form.Get("task_id")
		var status, clientID, peerRole string
		_ = db.QueryRow(`SELECT status, client_id, peer_role FROM tasks WHERE id=?`, taskID).Scan(&status, &clientID, &peerRole)
		now := time.Now().Format(time.RFC3339)
		switch status {
		case "running":
			_, _ = db.Exec(`UPDATE tasks SET status='stopping' WHERE id=?`, taskID)
		case "pending", "stopping":
			_, _ = db.Exec(`UPDATE tasks SET status='stopped', finished_at=?, started_at=COALESCE(NULLIF(started_at,''), created_at) WHERE id=?`, now, taskID)
			if clientID != "" {
				_, _ = db.Exec(`UPDATE clients SET current_task='' WHERE id=?`, clientID)
			}
		}
		// For peer tasks: also stop the linked peer task (source stops target and vice versa).
		if peerRole == "source" {
			// Stop target task that has this task as parent
			var targetID, targetStatus, targetClientID string
			_ = db.QueryRow(`SELECT id, status, client_id FROM tasks WHERE parent_task_id=? AND peer_role='target'`, taskID).
				Scan(&targetID, &targetStatus, &targetClientID)
			if targetID != "" {
				switch targetStatus {
				case "running":
					_, _ = db.Exec(`UPDATE tasks SET status='stopping' WHERE id=?`, targetID)
				case "pending":
					_, _ = db.Exec(`UPDATE tasks SET status='stopped', finished_at=?, started_at=COALESCE(NULLIF(started_at,''), created_at) WHERE id=?`, now, targetID)
					if targetClientID != "" {
						_, _ = db.Exec(`UPDATE clients SET current_task='' WHERE id=?`, targetClientID)
					}
				}
			}
		} else if peerRole == "target" {
			// Stop the source task (parent) - use a JOIN for efficiency
			var sourceID, sourceStatus, sourceClientID string
			_ = db.QueryRow(`SELECT t.id, t.status, t.client_id FROM tasks t
				INNER JOIN tasks child ON t.id = child.parent_task_id WHERE child.id=?`, taskID).
				Scan(&sourceID, &sourceStatus, &sourceClientID)
			if sourceID != "" {
				switch sourceStatus {
				case "running":
					_, _ = db.Exec(`UPDATE tasks SET status='stopping' WHERE id=?`, sourceID)
				case "pending":
					_, _ = db.Exec(`UPDATE tasks SET status='stopped', finished_at=?, started_at=COALESCE(NULLIF(started_at,''), created_at) WHERE id=?`, now, sourceID)
					if sourceClientID != "" {
						_, _ = db.Exec(`UPDATE clients SET current_task='' WHERE id=?`, sourceClientID)
					}
				}
			}
		}
		broker.Publish("tasks")
		if strings.Contains(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") &&
			!strings.Contains(r.Header.Get("Accept"), "application/json") {
			http.Redirect(w, r, panelPath, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func handleStopAllTasks(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Format(time.RFC3339)
		type stopTaskItem struct {
			id        string
			status    string
			clientID  string
			createdAt string
			startedAt string
		}
		tx, err := db.Begin()
		if err != nil {
			http.Error(w, "failed to begin transaction", http.StatusInternalServerError)
			return
		}
		rows, err := tx.Query(`SELECT id, status, client_id, created_at, started_at FROM tasks WHERE status IN ('running', 'pending')`)
		if err != nil {
			_ = tx.Rollback()
			http.Error(w, "failed to query tasks", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var items []stopTaskItem
		for rows.Next() {
			var taskID, status, clientID, createdAt string
			var startedAt sql.NullString
			if err := rows.Scan(&taskID, &status, &clientID, &createdAt, &startedAt); err != nil {
				_ = tx.Rollback()
				http.Error(w, "failed to scan task row", http.StatusInternalServerError)
				return
			}
			items = append(items, stopTaskItem{
				id:        taskID,
				status:    status,
				clientID:  clientID,
				createdAt: createdAt,
				startedAt: nullStringValue(startedAt),
			})
		}
		if err := rows.Err(); err != nil {
			_ = tx.Rollback()
			http.Error(w, "failed to iterate tasks", http.StatusInternalServerError)
			return
		}
		rows.Close()
		for _, item := range items {
			switch item.status {
			case "running":
				if _, err := tx.Exec(`UPDATE tasks SET status='stopping' WHERE id=?`, item.id); err != nil {
					_ = tx.Rollback()
					http.Error(w, "failed to update running task", http.StatusInternalServerError)
					return
				}
			case "pending":
				effectiveStartedAt := item.startedAt
				if effectiveStartedAt == "" {
					effectiveStartedAt = item.createdAt
				}
				if _, err := tx.Exec(`UPDATE tasks SET status='stopped', finished_at=?, started_at=? WHERE id=?`, now, effectiveStartedAt, item.id); err != nil {
					_ = tx.Rollback()
					http.Error(w, "failed to stop queued task", http.StatusInternalServerError)
					return
				}
				if item.clientID != "" {
					if _, err := tx.Exec(`UPDATE clients SET current_task='' WHERE id=?`, item.clientID); err != nil {
						_ = tx.Rollback()
						http.Error(w, "failed to clear client task", http.StatusInternalServerError)
						return
					}
				}
			}
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "failed to commit task updates", http.StatusInternalServerError)
			return
		}
		broker.Publish("tasks")
		if strings.Contains(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") &&
			!strings.Contains(r.Header.Get("Accept"), "application/json") {
			http.Redirect(w, r, panelPath, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func handleClearHistory(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = db.Exec(`DELETE FROM tasks WHERE status IN ('stopped', 'done')`)
		broker.Publish("tasks")
		if strings.Contains(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") &&
			!strings.Contains(r.Header.Get("Accept"), "application/json") {
			http.Redirect(w, r, panelPath, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

// ── Peer Transfer Handlers ────────────────────────────────────────────────────

// handlePeerAddr is called by the source client to report its listening address.
// The server stores this address in the linked target task so that the target
// client can connect to it directly.
func handlePeerAddr(db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ClientID    string `json:"client_id"`
			ClientToken string `json:"client_token"`
			TaskID      string `json:"task_id"`
			Port        string `json:"port"` // port the source is listening on
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// Authenticate the client
		var token string
		var approved int
		err := db.QueryRow(`SELECT token, approved FROM clients WHERE id=?`, req.ClientID).
			Scan(&token, &approved)
		if err != nil || token != req.ClientToken || approved != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		// Verify this task belongs to this client and is a peer source task
		var peerRole, clientID string
		err = db.QueryRow(`SELECT peer_role, client_id FROM tasks WHERE id=?`, req.TaskID).
			Scan(&peerRole, &clientID)
		if err != nil || clientID != req.ClientID || peerRole != "source" {
			http.Error(w, "task not found or not a peer source task", 400)
			return
		}
		// Build the peer address from the client's known remote IP + reported port
		var remoteIP string
		_ = db.QueryRow(`SELECT remote_ip FROM clients WHERE id=?`, req.ClientID).Scan(&remoteIP)
		// remoteIP may include port (from r.RemoteAddr), strip it
		host, _, err2 := net.SplitHostPort(remoteIP)
		if err2 != nil {
			host = remoteIP // already plain IP
		}
		peerAddr := net.JoinHostPort(host, req.Port)
		// Update the target task's peer_addr so it can be dispatched
		_, _ = db.Exec(`UPDATE tasks SET peer_addr=? WHERE parent_task_id=? AND peer_role='target'`, peerAddr, req.TaskID)
		broker.Publish("tasks")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "peer_addr": peerAddr})
	}
}

// handleCreatePeerTask creates a pair of linked peer tasks (source + target).
func handleCreatePeerTask(panelPath string, cfg *Config, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sourceClientID := strings.TrimSpace(r.Form.Get("source_client_id"))
		targetClientID := strings.TrimSpace(r.Form.Get("target_client_id"))
		if sourceClientID == "" || targetClientID == "" {
			http.Error(w, "source_client_id and target_client_id are required", 400)
			return
		}
		if sourceClientID == targetClientID {
			http.Error(w, "source and target must be different clients", 400)
			return
		}
		mbps, _ := strconv.Atoi(r.Form.Get("mbps"))
		if mbps <= 0 {
			mbps = 10
		}
		const maxMbps = 100000
		if mbps > maxMbps {
			http.Error(w, fmt.Sprintf("mbps out of range [1, %d]", maxMbps), 400)
			return
		}
		durVal, _ := strconv.Atoi(r.Form.Get("duration_val"))
		durUnit := r.Form.Get("duration_unit")
		dur := durationToSec(durVal, durUnit)
		const minDurSec = 5
		const maxDurSec = 86400 * 30
		if dur < minDurSec || dur > maxDurSec {
			http.Error(w, fmt.Sprintf("duration out of range [%ds, %ds]", minDurSec, maxDurSec), 400)
			return
		}
		// Verify both clients are approved
		var srcApproved, dstApproved int
		_ = db.QueryRow(`SELECT approved FROM clients WHERE id=?`, sourceClientID).Scan(&srcApproved)
		_ = db.QueryRow(`SELECT approved FROM clients WHERE id=?`, targetClientID).Scan(&dstApproved)
		if srcApproved != 1 || dstApproved != 1 {
			http.Error(w, "both clients must be approved", 400)
			return
		}
		now := time.Now().Format(time.RFC3339)
		sourceID := genToken(8)
		targetID := genToken(8)
		// Create source task (listens for connection)
		_, err := db.Exec(`INSERT INTO tasks(id,client_id,mode,up_mbps,down_mbps,duration_sec,density,status,created_at,scheduled_at,max_retries,retry_count,parent_task_id,template_name,peer_client_id,peer_role,peer_addr)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sourceID, sourceClientID, "peer", mbps, mbps, dur, 0, "pending", now, "", 0, 0, "", "", targetClientID, "source", "")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// Create target task (connects to source; parent_task_id links to source for addr lookup)
		_, err = db.Exec(`INSERT INTO tasks(id,client_id,mode,up_mbps,down_mbps,duration_sec,density,status,created_at,scheduled_at,max_retries,retry_count,parent_task_id,template_name,peer_client_id,peer_role,peer_addr)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			targetID, targetClientID, "peer", mbps, mbps, dur, 0, "pending", now, "", 0, 0, sourceID, "", sourceClientID, "target", "")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		broker.Publish("tasks")
		srcName := sourceClientID
		dstName := targetClientID
		_ = db.QueryRow(`SELECT name FROM clients WHERE id=?`, sourceClientID).Scan(&srcName)
		_ = db.QueryRow(`SELECT name FROM clients WHERE id=?`, targetClientID).Scan(&dstName)
		go barkPush(cfg.BarkURL, "对传任务已创建",
			fmt.Sprintf("%s ↔ %s 对传任务已创建\n速率:%dMbps 时长:%ds", srcName, dstName, mbps, dur))
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "source_id": sourceID, "target_id": targetID})
			return
		}
		http.Redirect(w, r, panelPath, http.StatusFound)
	}
}

func handlePushUpgrade(panelPath string, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, panelPath, http.StatusFound)
			return
		}
		_ = r.ParseForm()
		clientID := r.Form.Get("client_id")
		version := strings.TrimSpace(r.Form.Get("version"))
		if version == "" {
			version = getenv("BWPANEL_VERSION", Version)
		}
		if clientID == "all" {
			_, _ = db.Exec(`UPDATE clients SET upgrade_to=?`, version)
		} else {
			_, _ = db.Exec(`UPDATE clients SET upgrade_to=? WHERE id=?`, version, clientID)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": version})
	}
}

func handleBulkDeleteClients(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		clientIDs := splitCSV(r.Form.Get("client_ids"))
		for _, clientID := range clientIDs {
			_, _ = db.Exec(`DELETE FROM tasks WHERE client_id=?`, clientID)
			_, _ = db.Exec(`DELETE FROM clients WHERE id=?`, clientID)
		}
		broker.Publish("clients")
		broker.Publish("tasks")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "count": len(clientIDs)})
	}
}

func handleBulkRestartClients(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		clientIDs := splitCSV(r.Form.Get("client_ids"))
		for _, clientID := range clientIDs {
			_, _ = db.Exec(`UPDATE clients SET restart_requested=1 WHERE id=?`, clientID)
		}
		broker.Publish("clients")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "count": len(clientIDs)})
	}
}

func handleDeleteClient(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		clientID := r.Form.Get("client_id")
		_, _ = db.Exec(`DELETE FROM clients WHERE id=?`, clientID)
		broker.Publish("clients")
		if strings.Contains(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") &&
			!strings.Contains(r.Header.Get("Accept"), "application/json") {
			http.Redirect(w, r, panelPath, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

// ── Database Helpers ──────────────────────────────────────────────────────────

func mustClients(db *sql.DB) []Client {
	rows, err := db.Query(`SELECT id,name,remark,group_name,tags,approved,last_seen,remote_ip,current_task,upgrade_to,version,latency,ssh_attempts,restart_requested FROM clients`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Client
	for rows.Next() {
		var c Client
		var approved, restartRequested int
		_ = rows.Scan(&c.ID, &c.Name, &c.Remark, &c.GroupName, &c.Tags, &approved, &c.LastSeen, &c.RemoteIP, &c.CurrentTask, &c.UpgradeTo, &c.Version, &c.Latency, &c.SSHAttempts, &restartRequested)
		c.Approved = approved == 1
		c.RestartRequested = restartRequested == 1
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen > out[j].LastSeen })
	return out
}

func mustTasks(db *sql.DB) []Task {
	rows, err := db.Query(`SELECT id,client_id,mode,up_mbps,down_mbps,duration_sec,density,status,created_at,started_at,finished_at,scheduled_at,upload_bytes,download_bytes,logs,max_retries,retry_count,parent_task_id,template_name,peer_client_id,peer_role,peer_addr FROM tasks`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		_ = rows.Scan(&t.ID, &t.ClientID, &t.Mode, &t.UpMbps, &t.DownMbps, &t.DurationSec, &t.Density, &t.Status, &t.CreatedAt, &t.StartedAt, &t.FinishedAt, &t.ScheduledAt, &t.UploadBytes, &t.DownloadBytes, &t.Logs, &t.MaxRetries, &t.RetryCount, &t.ParentTaskID, &t.TemplateName, &t.PeerClientID, &t.PeerRole, &t.PeerAddr)
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

func mustTaskTemplates(db *sql.DB) []TaskTemplate {
	rows, err := db.Query(`SELECT id,name,mode,up_mbps,down_mbps,duration_sec,density,max_retries,created_at FROM task_templates ORDER BY created_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []TaskTemplate
	for rows.Next() {
		var t TaskTemplate
		_ = rows.Scan(&t.ID, &t.Name, &t.Mode, &t.UpMbps, &t.DownMbps, &t.DurationSec, &t.Density, &t.MaxRetries, &t.CreatedAt)
		out = append(out, t)
	}
	return out
}

func taskStatus(db *sql.DB, taskID string) string {
	var status string
	_ = db.QueryRow(`SELECT status FROM tasks WHERE id=?`, taskID).Scan(&status)
	return status
}

// ── HTTP Middleware ───────────────────────────────────────────────────────────

func jsonHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next(w, r)
	}
}

func basicAuth(cfg Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		// Use constant-time comparison to prevent timing-based attacks.
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(cfg.AdminUser)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(cfg.AdminPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="bwpanel"`)
			http.Error(w, "unauthorized", 401)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func realIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func genToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

func normalizeTags(raw string) string {
	return strings.Join(splitCSV(tagNormalizer.Replace(raw)), ",")
}

func mustMarshalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func nullStringValue(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}
