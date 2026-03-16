package main

import (
	"bufio"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Config struct {
	PanelAddr  string
	DataAddr   string
	ServerHost string
	InitToken  string
	AdminUser  string
	AdminPass  string
	DBPath     string
	PanelPath  string
}

type Client struct {
	ID          string
	Name        string
	Remark      string
	Approved    bool
	LastSeen    string
	RemoteIP    string
	CurrentTask string
}

type Task struct {
	ID            string
	ClientID      string
	Mode          string
	UpMbps        int
	DownMbps      int
	DurationSec   int
	Status        string
	CreatedAt     string
	StartedAt     string
	FinishedAt    string
	UploadBytes   int64
	DownloadBytes int64
}

type RegisterReq struct {
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
	Name        string `json:"name"`
	InitToken   string `json:"init_token"`
}

type HeartbeatReq struct {
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
}

type ResultReq struct {
	ClientID      string `json:"client_id"`
	ClientToken   string `json:"client_token"`
	TaskID        string `json:"task_id"`
	Status        string `json:"status"`
	UploadBytes   int64  `json:"upload_bytes"`
	DownloadBytes int64  `json:"download_bytes"`
}

type ProgressReq struct {
	ClientID      string `json:"client_id"`
	ClientToken   string `json:"client_token"`
	TaskID        string `json:"task_id"`
	UploadBytes   int64  `json:"upload_bytes"`
	DownloadBytes int64  `json:"download_bytes"`
}

type AssignResp struct {
	ID          string `json:"id"`
	Mode        string `json:"mode"`
	UpMbps      int    `json:"up_mbps"`
	DownMbps    int    `json:"down_mbps"`
	DurationSec int    `json:"duration_sec"`
	DataAddr    string `json:"data_addr"`
}

type ControlResp struct {
	Status string `json:"status"`
}

type DataHello struct {
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
	TaskID      string `json:"task_id"`
	Mode        string `json:"mode"`
	DurationSec int    `json:"duration_sec"`
}

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
	}
	if cfg.PanelPath == "" {
		cfg.PanelPath = "/admin"
	}

	db := mustInitDB(cfg.DBPath)
	broker := NewBroker()

	log.Printf("panel=%s%s data=%s db=%s", cfg.PanelAddr, cfg.PanelPath, cfg.DataAddr, cfg.DBPath)

	go runDataServer(cfg, db)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", jsonHandler(handleRegister(cfg, db, broker)))
	mux.HandleFunc("/api/heartbeat", jsonHandler(handleHeartbeat(db)))
	mux.HandleFunc("/api/task/next", jsonHandler(handleNextTask(cfg, db, broker)))
	mux.HandleFunc("/api/task/result", jsonHandler(handleTaskResult(db, broker)))
	mux.HandleFunc("/api/task/control", jsonHandler(handleTaskControl(db)))
	mux.HandleFunc("/api/task/progress", jsonHandler(handleTaskProgress(db)))

	// /api/data: returns clients+tasks JSON for frontend polling (no auth needed beyond being on same panel)
	mux.HandleFunc("/api/data", jsonHandler(handleAPIData(db)))

	p := cfg.PanelPath
	mux.Handle(p, basicAuth(cfg, http.HandlerFunc(handleAdmin(cfg, db))))
	mux.Handle(p+"/approve", basicAuth(cfg, http.HandlerFunc(handleApprove(p, db, broker))))
	mux.Handle(p+"/client/edit", basicAuth(cfg, http.HandlerFunc(handleClientEdit(p, db, broker))))
	mux.Handle(p+"/task/create", basicAuth(cfg, http.HandlerFunc(handleCreateTask(p, db, broker))))
	mux.Handle(p+"/task/stop", basicAuth(cfg, http.HandlerFunc(handleStopTask(p, db, broker))))
	mux.Handle(p+"/events", basicAuth(cfg, http.HandlerFunc(handleEvents(broker))))

	if p != "/admin" {
		mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, p, http.StatusFound)
		})
	}

	log.Fatal(http.ListenAndServe(cfg.PanelAddr, mux))
}

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
			download_bytes INTEGER NOT NULL DEFAULT 0
		);`,
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
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
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

	// clear per-connection deadline now that handshake is done
	_ = conn.SetDeadline(time.Time{})

	deadline := time.Now().Add(time.Duration(duration) * time.Second)

	// resolve effective mode from hello (both mode client opens two connections)
	effMode := hello.Mode
	if effMode == "" {
		effMode = mode
	}

	switch effMode {
	case "upload":
		readDiscardLoop(conn, deadline, func() bool { return taskStatus(db, hello.TaskID) == "running" })
	case "download":
		_ = pacedWrite(conn, downMbps, deadline, func() bool { return taskStatus(db, hello.TaskID) == "running" })
	case "both":
		// legacy single-conn both: server reads and writes same conn
		done := make(chan struct{})
		go func() {
			readDiscardLoop(conn, deadline, func() bool { return taskStatus(db, hello.TaskID) == "running" })
			close(done)
		}()
		_ = pacedWrite(conn, downMbps, deadline, func() bool { return taskStatus(db, hello.TaskID) == "running" })
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
	perTick := bytesPerSec / 10
	if perTick < 1024 {
		perTick = 1024
	}

	buf := make([]byte, 32*1024)
	_, _ = rand.Read(buf)

	tk := time.NewTicker(100 * time.Millisecond)
	defer tk.Stop()

	for time.Now().Before(deadline) && keep() {
		left := perTick
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
		<-tk.C
	}
	return nil
}

func handleRegister(cfg Config, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
			_, err = db.Exec(`
				INSERT INTO clients(id,name,remark,token,approved,last_seen,remote_ip,current_task)
				VALUES(?,?,?,?,?,?,?,?)`,
				req.ClientID, req.Name, "", req.ClientToken, 0, now, realIP(r), "")
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			broker.Publish("clients")
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

		_, _ = db.Exec(`UPDATE clients SET last_seen=?, remote_ip=? WHERE id=?`,
			time.Now().Format(time.RFC3339), realIP(r), req.ClientID)

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

		if currentTask != "" && taskStatus(db, currentTask) == "running" {
			w.WriteHeader(204)
			return
		}

		var t Task
		err = db.QueryRow(`
			SELECT id,client_id,mode,up_mbps,down_mbps,duration_sec,status,created_at
			FROM tasks
			WHERE client_id=? AND status='pending'
			ORDER BY created_at ASC
			LIMIT 1`, clientID).
			Scan(&t.ID, &t.ClientID, &t.Mode, &t.UpMbps, &t.DownMbps, &t.DurationSec, &t.Status, &t.CreatedAt)
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

		addr := cfg.DataAddr
		if strings.HasPrefix(addr, ":") {
			addr = cfg.ServerHost + addr
		}

		_ = json.NewEncoder(w).Encode(AssignResp{
			ID:          t.ID,
			Mode:        t.Mode,
			UpMbps:      t.UpMbps,
			DownMbps:    t.DownMbps,
			DurationSec: t.DurationSec,
			DataAddr:    addr,
		})
	}
}

func handleTaskResult(db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ResultReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		var token, status, clientID string
		err := db.QueryRow(`
			SELECT c.token, t.status, t.client_id
			FROM clients c
			JOIN tasks t ON c.id=t.client_id
			WHERE c.id=? AND t.id=?`,
			req.ClientID, req.TaskID).
			Scan(&token, &status, &clientID)
		if err != nil || token != req.ClientToken || clientID != req.ClientID {
			http.Error(w, "unauthorized", 401)
			return
		}

		finalStatus := req.Status
		if finalStatus == "" {
			finalStatus = "done"
		}
		if status == "stopping" {
			finalStatus = "stopped"
		}

		_, _ = db.Exec(`UPDATE tasks SET status=?, finished_at=?, upload_bytes=?, download_bytes=? WHERE id=?`,
			finalStatus, time.Now().Format(time.RFC3339), req.UploadBytes, req.DownloadBytes, req.TaskID)
		_, _ = db.Exec(`UPDATE clients SET current_task='' WHERE id=?`, req.ClientID)

		broker.Publish("tasks")
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
			SELECT c.token, t.status
			FROM clients c
			JOIN tasks t ON c.id=t.client_id
			WHERE c.id=? AND t.id=?`,
			clientID, taskID).
			Scan(&token, &status)
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
			SELECT c.token, t.status
			FROM clients c
			JOIN tasks t ON c.id=t.client_id
			WHERE c.id=? AND t.id=?`,
			req.ClientID, req.TaskID).
			Scan(&token, &status)
		if err != nil || token != req.ClientToken {
			http.Error(w, "unauthorized", 401)
			return
		}
		if status != "running" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false})
			return
		}

		_, _ = db.Exec(`UPDATE tasks SET upload_bytes=?, download_bytes=? WHERE id=?`,
			req.UploadBytes, req.DownloadBytes, req.TaskID)

		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

// handleAPIData returns clients and tasks as JSON for frontend polling.
func handleAPIData(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clients := mustClients(db)
		tasks := mustTasks(db)

		type clientJSON struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Remark      string `json:"remark"`
			Approved    bool   `json:"approved"`
			LastSeen    string `json:"last_seen"`
			RemoteIP    string `json:"remote_ip"`
			CurrentTask string `json:"current_task"`
		}
		type taskJSON struct {
			ID            string  `json:"id"`
			ClientID      string  `json:"client_id"`
			ClientName    string  `json:"client_name"`
			Mode          string  `json:"mode"`
			UpMbps        int     `json:"up_mbps"`
			DownMbps      int     `json:"down_mbps"`
			DurationSec   int     `json:"duration_sec"`
			Status        string  `json:"status"`
			StartedAt     string  `json:"started_at"`
			FinishedAt    string  `json:"finished_at"`
			UploadGB      float64 `json:"upload_gb"`
			DownloadGB    float64 `json:"download_gb"`
		}

		clientNames := map[string]string{}
		for _, c := range clients {
			clientNames[c.ID] = c.Name
		}

		cj := make([]clientJSON, 0, len(clients))
		for _, c := range clients {
			cj = append(cj, clientJSON{
				ID: c.ID, Name: c.Name, Remark: c.Remark,
				Approved: c.Approved, LastSeen: c.LastSeen,
				RemoteIP: c.RemoteIP, CurrentTask: c.CurrentTask,
			})
		}

		tj := make([]taskJSON, 0, len(tasks))
		for _, t := range tasks {
			tj = append(tj, taskJSON{
				ID: t.ID, ClientID: t.ClientID,
				ClientName:  clientNames[t.ClientID],
				Mode:        t.Mode,
				UpMbps:      t.UpMbps,
				DownMbps:    t.DownMbps,
				DurationSec: t.DurationSec,
				Status:      t.Status,
				StartedAt:   t.StartedAt,
				FinishedAt:  t.FinishedAt,
				UploadGB:    float64(t.UploadBytes) / (1024 * 1024 * 1024),
				DownloadGB:  float64(t.DownloadBytes) / (1024 * 1024 * 1024),
			})
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"clients": cj,
			"tasks":   tj,
		})
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

func handleAdmin(cfg Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clients := mustClients(db)
		tasks := mustTasks(db)

		type approvedClient struct {
			ID   string
			Name string
		}

		type pageData struct {
			Clients         []Client
			Tasks           []Task
			ApprovedClients []approvedClient
			DefaultClientID string
			ClientNames     map[string]string
			PanelPath       string
			ServerHost      string
			InitToken       string
			Version         string
		}

		var approvedClients []approvedClient
		defaultClientID := ""
		clientNames := map[string]string{}
		for _, c := range clients {
			clientNames[c.ID] = c.Name
			if c.Approved {
				approvedClients = append(approvedClients, approvedClient{ID: c.ID, Name: c.Name})
				if defaultClientID == "" {
					defaultClientID = c.ID
				}
			}
		}

		version := getenv("BWPANEL_VERSION", "latest")

		const page = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>带宽测试面板</title>
<style>
:root{--bg:#f5f7fb;--card:#fff;--border:#e5e7eb;--text:#111827;--muted:#6b7280;--primary:#2563eb;--ph:#1d4ed8;--ok-bg:#dcfce7;--ok:#166534;--no-bg:#fee2e2;--no:#991b1b;--run-bg:#dbeafe;--run:#1d4ed8;--done-bg:#dcfce7;--done:#166534;--pend-bg:#f3f4f6;--pend:#374151;--warn-bg:#fef3c7;--warn:#92400e;--stop-bg:#ffedd5;--stop:#9a3412}
*{box-sizing:border-box}
body{margin:0;padding:20px;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif;background:var(--bg);color:var(--text)}
.wrap{max-width:1280px;margin:0 auto}
.card{background:var(--card);border:1px solid var(--border);border-radius:14px;padding:18px;margin-bottom:18px;box-shadow:0 1px 3px rgba(0,0,0,.05)}
h1{margin:0 0 6px;font-size:36px}h2{margin:0 0 14px;font-size:20px}p{margin:0;color:var(--muted);font-size:14px}
.toolbar{display:flex;flex-wrap:wrap;gap:8px;margin-top:12px;align-items:center}
.note{font-size:13px;color:var(--muted)}
#liveStatus{font-size:13px;color:var(--muted)}
btn,button,.btn{border:none;border-radius:9px;padding:9px 14px;background:var(--primary);color:#fff;cursor:pointer;font-size:13px;text-decoration:none;display:inline-block;white-space:nowrap}
button:hover,.btn:hover{background:var(--ph)}
button.sec{background:#e5e7eb;color:#111}button.sec:hover{background:#d1d5db}
button.danger{background:#ef4444;color:#fff}button.danger:hover{background:#dc2626}
form.inline{display:inline}
.tbl{overflow:auto}
table{width:100%;border-collapse:collapse}
th,td{padding:10px 9px;border-bottom:1px solid var(--border);text-align:left;vertical-align:middle;white-space:nowrap;font-size:13px}
th{background:#f9fafb;font-weight:700}
.badge{display:inline-block;padding:3px 9px;border-radius:999px;font-size:11px;font-weight:700}
.ok{background:var(--ok-bg);color:var(--ok)}.no{background:var(--no-bg);color:var(--no)}
.running{background:var(--run-bg);color:var(--run)}.done{background:var(--done-bg);color:var(--done)}
.pending{background:var(--pend-bg);color:var(--pend)}.stopped{background:var(--warn-bg);color:var(--warn)}
.stopping{background:var(--stop-bg);color:var(--stop)}
.grid{display:grid;grid-template-columns:2fr 1fr 1fr 1fr 1fr 1fr auto;gap:10px;align-items:end}
.dur-wrap{display:flex;gap:6px}
.dur-wrap input{flex:1}
.dur-wrap select{width:90px}
input,select,textarea{width:100%;padding:10px 12px;border:1px solid var(--border);border-radius:9px;font-size:13px;background:#fff}
textarea{resize:vertical;min-height:60px}
.tip{margin-top:10px;font-size:12px;color:var(--muted)}
.copy-box{display:flex;gap:8px;align-items:center;margin-top:10px}
.copy-box input{font-family:monospace;font-size:12px;background:#f9fafb}
.gen-grid{display:grid;grid-template-columns:1fr 1fr 1fr auto;gap:10px;align-items:end}
@media(max-width:960px){.grid{grid-template-columns:1fr 1fr}.gen-grid{grid-template-columns:1fr 1fr}}
@media(max-width:640px){body{padding:10px}.grid,.gen-grid{grid-template-columns:1fr}h1{font-size:26px}}
</style>
</head>
<body>
<div class="wrap">

<div class="card">
  <h1>带宽测试面板</h1>
  <p>客户端管理、任务下发和实时状态查看。</p>
  <div class="toolbar">
    <button type="button" onclick="location.reload()">手动刷新</button>
    <span id="liveStatus" class="note">数据每 5 秒自动刷新。</span>
  </div>
</div>

<div class="card">
  <h2>客户端列表</h2>
  <div class="tbl" id="clientTableWrap">
  <table>
    <tr><th>名称</th><th>备注</th><th>已批准</th><th>最后心跳</th><th>远程 IP</th><th>当前任务</th><th>操作</th></tr>
    {{range .Clients}}
    <tr>
      <td>{{.Name}}</td>
      <td>{{.Remark}}</td>
      <td>{{if .Approved}}<span class="badge ok">是</span>{{else}}<span class="badge no">否</span>{{end}}</td>
      <td>{{.LastSeen}}</td>
      <td>{{.RemoteIP}}</td>
      <td style="font-family:monospace;font-size:11px">{{.CurrentTask}}</td>
      <td>
        <div style="display:flex;gap:6px;flex-wrap:wrap">
        {{if not .Approved}}
        <form class="inline" method="post" action="{{$.PanelPath}}/approve">
          <input type="hidden" name="client_id" value="{{.ID}}">
          <button type="submit">批准</button>
        </form>
        {{end}}
        <button type="button" class="sec" onclick="openEdit('{{.ID}}','{{.Name}}','{{.Remark}}')">编辑</button>
        </div>
      </td>
    </tr>
    {{end}}
  </table>
  </div>
  <div class="tip">创建任务前请确认客户端已批准。</div>
</div>

<div id="editModal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,.45);z-index:999;align-items:center;justify-content:center">
  <div class="card" style="width:min(480px,95vw);margin:0">
    <h2>编辑客户端</h2>
    <form method="post" action="{{.PanelPath}}/client/edit">
      <input type="hidden" id="editID" name="client_id">
      <div style="margin-bottom:10px">
        <label style="font-size:13px;color:var(--muted)">名称</label>
        <input id="editName" name="name" placeholder="名称" style="margin-top:4px">
      </div>
      <div style="margin-bottom:14px">
        <label style="font-size:13px;color:var(--muted)">备注</label>
        <textarea id="editRemark" name="remark" placeholder="备注（可选）" style="margin-top:4px"></textarea>
      </div>
      <div style="display:flex;gap:8px">
        <button type="submit">保存</button>
        <button type="button" class="sec" onclick="closeEdit()">取消</button>
      </div>
    </form>
  </div>
</div>

<div class="card">
  <h2>创建任务</h2>
  <form method="post" action="{{.PanelPath}}/task/create">
    <div class="grid">
      <div>
        <label class="note">选择客户端</label>
        <select id="client_id" name="client_id" required style="margin-top:4px">
          {{range .ApprovedClients}}
          <option value="{{.ID}}" {{if eq .ID $.DefaultClientID}}selected{{end}}>{{.Name}}</option>
          {{end}}
        </select>
      </div>
      <div>
        <label class="note">模式</label>
        <select name="mode" style="margin-top:4px">
          <option value="upload">上传</option>
          <option value="download">下载</option>
          <option value="both">双向</option>
        </select>
      </div>
      <div>
        <label class="note">上传速度 Mbps</label>
        <input name="up_mbps" value="10" style="margin-top:4px">
      </div>
      <div>
        <label class="note">下载速度 Mbps</label>
        <input name="down_mbps" value="10" style="margin-top:4px">
      </div>
      <div>
        <label class="note">时长</label>
        <div class="dur-wrap" style="margin-top:4px">
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
      <div style="align-self:end">
        <button type="submit" style="width:100%">创建任务</button>
      </div>
    </div>
  </form>
  <div class="tip">上传/下载速度单位为 Mbps；时长默认选"分"，1 分 = 60 秒。</div>
</div>

<div class="card">
  <h2>生成客户端安装命令</h2>
  <div class="gen-grid">
    <div>
      <label class="note">客户端名称</label>
      <input id="genName" placeholder="例：香港-1号机" style="margin-top:4px">
    </div>
    <div>
      <label class="note">备注（可选）</label>
      <input id="genRemark" placeholder="可选备注" style="margin-top:4px">
    </div>
    <div>
      <label class="note">版本号</label>
      <input id="genVersion" value="{{.Version}}" style="margin-top:4px">
    </div>
    <div style="align-self:end">
      <button type="button" onclick="genCmd()" style="width:100%">生成命令</button>
    </div>
  </div>
  <div class="copy-box" id="cmdBox" style="display:none">
    <input id="cmdText" readonly>
    <button type="button" class="sec" onclick="copyCmd()">复制</button>
  </div>
  <div class="tip" id="cmdTip"></div>
</div>

<div class="card">
  <h2>任务列表 <span id="taskRefreshHint" style="font-size:12px;color:var(--muted);font-weight:400"></span></h2>
  <div class="tbl">
  <table id="taskTable">
    <thead><tr><th>客户端</th><th>模式</th><th>上传 Mbps</th><th>下载 Mbps</th><th>时长</th><th>状态</th><th>已上传</th><th>已下载</th><th>开始时间</th><th>结束时间</th><th>操作</th></tr></thead>
    <tbody id="taskBody">
    {{range .Tasks}}
    <tr data-task-id="{{.ID}}">
      <td>{{index $.ClientNames .ClientID}}</td>
      <td>{{.Mode}}</td>
      <td>{{.UpMbps}}</td>
      <td>{{.DownMbps}}</td>
      <td>{{.DurationSec}} 秒</td>
      <td><span class="badge {{.Status}}">{{.Status}}</span></td>
      <td class="up-col">-</td>
      <td class="down-col">-</td>
      <td>{{.StartedAt}}</td>
      <td>{{.FinishedAt}}</td>
      <td>
        {{if eq .Status "running"}}
        <form class="inline" method="post" action="{{$.PanelPath}}/task/stop">
          <input type="hidden" name="task_id" value="{{.ID}}">
          <button type="submit" class="danger">停止</button>
        </form>
        {{else}}<span class="note">-</span>{{end}}
      </td>
    </tr>
    {{end}}
    </tbody>
  </table>
  </div>
</div>

</div>

<script>
const PANEL_PATH = "{{.PanelPath}}";
const SERVER_HOST = "{{.ServerHost}}";
const INIT_TOKEN  = "{{.InitToken}}";
const PANEL_ADDR  = location.host;

function fmtGB(gb) {
  if (gb < 0.001) return '0.00 GB';
  return gb.toFixed(3) + ' GB';
}

// Poll /api/data every 5 seconds and update task rows + client table
function pollData() {
  fetch('/api/data')
    .then(r => r.json())
    .then(data => {
      // update task rows
      const tasks = data.tasks || [];
      tasks.forEach(t => {
        const row = document.querySelector('[data-task-id="' + t.id + '"]');
        if (!row) return;
        const upCol = row.querySelector('.up-col');
        const dnCol = row.querySelector('.down-col');
        if (upCol) upCol.textContent = fmtGB(t.upload_gb);
        if (dnCol) dnCol.textContent = fmtGB(t.download_gb);
        // update badge
        const badge = row.querySelector('.badge');
        if (badge) {
          badge.className = 'badge ' + t.status;
          badge.textContent = t.status;
        }
      });
      // update hint
      const hint = document.getElementById('taskRefreshHint');
      if (hint) hint.textContent = '(上次刷新: ' + new Date().toLocaleTimeString() + ')';

      // update client last_seen every 60s via same data (refresh client table every 60s)
    })
    .catch(() => {});
}

setInterval(pollData, 5000);
pollData();

// SSE for notifications
const es = new EventSource(PANEL_PATH + '/events');
const liveStatus = document.getElementById('liveStatus');
es.onmessage = function(e) {
  if (e.data !== 'ping' && e.data !== 'ready') {
    if (liveStatus) liveStatus.textContent = '检测到状态变化，数据将在下次轮询时自动更新。';
  }
};
es.onerror = function() {
  if (liveStatus) liveStatus.textContent = '实时消息流异常，仍会每 5 秒自动刷新数据。';
};

function openEdit(id, name, remark) {
  document.getElementById('editID').value = id;
  document.getElementById('editName').value = name;
  document.getElementById('editRemark').value = remark;
  document.getElementById('editModal').style.display = 'flex';
}
function closeEdit() {
  document.getElementById('editModal').style.display = 'none';
}

function genCmd() {
  const name    = document.getElementById('genName').value.trim();
  const remark  = document.getElementById('genRemark').value.trim();
  const version = document.getElementById('genVersion').value.trim() || 'latest';
  if (!name) { alert('请填写客户端名称'); return; }
  const panelUrl = location.protocol + '//' + PANEL_ADDR;
  let cmd = 'curl --proto \'=https\' --tlsv1.2 -fsSL https://raw.githubusercontent.com/ctsunny/bwtest/main/scripts/install_client.sh | bash -s --'
    + ' --server-url ' + panelUrl
    + ' --init-token \'' + INIT_TOKEN + '\''
    + ' --client-name \'' + name + '\''
    + ' --version ' + version;
  if (remark) cmd += ' --remark \'' + remark + '\'';
  document.getElementById('cmdText').value = cmd;
  document.getElementById('cmdBox').style.display = 'flex';
  document.getElementById('cmdTip').textContent = '将此命令复制到客户端 VPS 上执行即可完成安装与注册。';
}
function copyCmd() {
  const el = document.getElementById('cmdText');
  el.select();
  document.execCommand('copy');
  alert('已复制到剪贴板');
}
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
		}).Parse(page))
		_ = tpl.Execute(w, pageData{
			Clients:         clients,
			Tasks:           tasks,
			ApprovedClients: approvedClients,
			DefaultClientID: defaultClientID,
			ClientNames:     clientNames,
			PanelPath:       cfg.PanelPath,
			ServerHost:      cfg.ServerHost,
			InitToken:       cfg.InitToken,
			Version:         version,
		})
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

func handleApprove(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		clientID := r.Form.Get("client_id")
		_, _ = db.Exec(`UPDATE clients SET approved=1 WHERE id=?`, clientID)
		broker.Publish("clients")
		http.Redirect(w, r, panelPath, http.StatusFound)
	}
}

func handleClientEdit(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		clientID := r.Form.Get("client_id")
		name := strings.TrimSpace(r.Form.Get("name"))
		remark := strings.TrimSpace(r.Form.Get("remark"))
		if name != "" {
			_, _ = db.Exec(`UPDATE clients SET name=?, remark=? WHERE id=?`, name, remark, clientID)
		}
		broker.Publish("clients")
		http.Redirect(w, r, panelPath, http.StatusFound)
	}
}

func handleCreateTask(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()

		clientID := r.Form.Get("client_id")
		mode := r.Form.Get("mode")
		up, _ := strconv.Atoi(r.Form.Get("up_mbps"))
		down, _ := strconv.Atoi(r.Form.Get("down_mbps"))
		durVal, _ := strconv.Atoi(r.Form.Get("duration_val"))
		durUnit := r.Form.Get("duration_unit")
		dur := durationToSec(durVal, durUnit)
		if dur <= 0 {
			dur = 60
		}
		if up < 0 {
			up = 0
		}
		if down < 0 {
			down = 0
		}

		id := genToken(8)
		_, err := db.Exec(`
			INSERT INTO tasks(id,client_id,mode,up_mbps,down_mbps,duration_sec,status,created_at)
			VALUES(?,?,?,?,?,?,?,?)`,
			id, clientID, mode, up, down, dur, "pending", time.Now().Format(time.RFC3339))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		broker.Publish("tasks")
		http.Redirect(w, r, panelPath, http.StatusFound)
	}
}

func handleStopTask(panelPath string, db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		taskID := r.Form.Get("task_id")
		_, _ = db.Exec(`UPDATE tasks SET status='stopping' WHERE id=? AND status='running'`, taskID)
		broker.Publish("tasks")
		http.Redirect(w, r, panelPath, http.StatusFound)
	}
}

func mustClients(db *sql.DB) []Client {
	rows, err := db.Query(`SELECT id,name,remark,approved,last_seen,remote_ip,current_task FROM clients`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Client
	for rows.Next() {
		var c Client
		var approved int
		_ = rows.Scan(&c.ID, &c.Name, &c.Remark, &approved, &c.LastSeen, &c.RemoteIP, &c.CurrentTask)
		c.Approved = approved == 1
		out = append(out, c)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].LastSeen > out[j].LastSeen
	})
	return out
}

func mustTasks(db *sql.DB) []Task {
	rows, err := db.Query(`SELECT id,client_id,mode,up_mbps,down_mbps,duration_sec,status,created_at,started_at,finished_at,upload_bytes,download_bytes FROM tasks`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var t Task
		_ = rows.Scan(&t.ID, &t.ClientID, &t.Mode, &t.UpMbps, &t.DownMbps, &t.DurationSec, &t.Status, &t.CreatedAt, &t.StartedAt, &t.FinishedAt, &t.UploadBytes, &t.DownloadBytes)
		out = append(out, t)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out
}

func taskStatus(db *sql.DB, taskID string) string {
	var status string
	_ = db.QueryRow(`SELECT status FROM tasks WHERE id=?`, taskID).Scan(&status)
	return status
}

func jsonHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next(w, r)
	}
}

func basicAuth(cfg Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != cfg.AdminUser || p != cfg.AdminPass {
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
