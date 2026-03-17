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
	"net/url"
	"os"
	"os/exec"
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
	BarkURL    string
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

func barkPush(barkURL, title, body string) {
	if barkURL == "" {
		return
	}
	base := strings.TrimRight(barkURL, "/")
	pushURL := fmt.Sprintf("%s/%s/%s", base, url.PathEscape(title), url.PathEscape(body))
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(pushURL)
	if err != nil {
		log.Printf("bark push error: %v", err)
		return
	}
	defer resp.Body.Close()
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
		return strings.TrimRight(token, "/")
	}
	return "https://api.day.app/" + token
}

func barkTokenFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, "/")
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
		cfg.BarkURL = saved
	}

	db := mustInitDB(cfg.DBPath)
	broker := NewBroker()
	resetStuckTasks(db)

	log.Printf("panel=%s%s data=%s db=%s bark=%v",
		cfg.PanelAddr, cfg.PanelPath, cfg.DataAddr, cfg.DBPath, cfg.BarkURL != "")

	go runDataServer(cfg, db)
	go watchStuckTasks(db, broker)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", jsonHandler(handleRegister(cfg, db, broker)))
	mux.HandleFunc("/api/heartbeat", jsonHandler(handleHeartbeat(db)))
	mux.HandleFunc("/api/task/next", jsonHandler(handleNextTask(cfg, db, broker)))
	mux.HandleFunc("/api/task/result", jsonHandler(handleTaskResult(cfg, db, broker)))
	mux.HandleFunc("/api/task/control", jsonHandler(handleTaskControl(db)))
	mux.HandleFunc("/api/task/progress", jsonHandler(handleTaskProgress(db)))
	mux.HandleFunc("/api/data", jsonHandler(handleAPIData(db)))

	p := cfg.PanelPath
	mux.Handle(p, basicAuth(cfg, http.HandlerFunc(handleAdmin(cfg, db))))
	mux.Handle(p+"/approve", basicAuth(cfg, http.HandlerFunc(handleApprove(p, cfg, db, broker))))
	mux.Handle(p+"/client/edit", basicAuth(cfg, http.HandlerFunc(handleClientEdit(p, db, broker))))
	mux.Handle(p+"/task/create", basicAuth(cfg, http.HandlerFunc(handleCreateTask(p, cfg, db, broker))))
	mux.Handle(p+"/task/stop", basicAuth(cfg, http.HandlerFunc(handleStopTask(p, db, broker))))
	mux.Handle(p+"/task/delete", basicAuth(cfg, http.HandlerFunc(handleDeleteTask(p, db, broker))))
	mux.Handle(p+"/gen/install-cmd", basicAuth(cfg, http.HandlerFunc(handleGenInstallCmd(p, cfg))))
	mux.Handle(p+"/settings", basicAuth(cfg, http.HandlerFunc(handleSettings(p, &cfg))))
	mux.Handle(p+"/events", basicAuth(cfg, http.HandlerFunc(handleEvents(broker))))
	mux.Handle(p+"/restart", basicAuth(cfg, http.HandlerFunc(handleRestart(p))))
	mux.Handle(p+"/server", basicAuth(cfg, http.HandlerFunc(handleServerPage(p, &cfg))))

	if p != "/admin" {
		mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, p, http.StatusFound)
		})
	}

	log.Fatal(http.ListenAndServe(cfg.PanelAddr, mux))
}

func resetStuckTasks(db *sql.DB) {
	res, err := db.Exec(`UPDATE tasks SET status='pending', started_at='' WHERE status='running'`)
	if err != nil {
		log.Printf("resetStuckTasks: %v", err)
		return
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("resetStuckTasks: reset %d running task(s) to pending", n)
	}
	_, _ = db.Exec(`UPDATE clients SET current_task=''`)
}

func watchStuckTasks(db *sql.DB, broker *Broker) {
	tk := time.NewTicker(30 * time.Second)
	defer tk.Stop()
	for range tk.C {
		threshold := time.Now().Add(-90 * time.Second).Format(time.RFC3339)
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
			_, _ = db.Exec(`UPDATE tasks SET status='pending', started_at='' WHERE id=?`, p.tid)
			_, _ = db.Exec(`UPDATE clients SET current_task='' WHERE id=?`, p.cid)
			log.Printf("watchStuckTasks: reset task %s (client %s) to pending due to heartbeat timeout", p.tid, p.cid)
		}
		if len(pairs) > 0 {
			broker.Publish("tasks")
		}
	}
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
	_ = conn.SetDeadline(time.Time{})
	deadline := time.Now().Add(time.Duration(hello.DurationSec) * time.Second)
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
			_, err = db.Exec(`INSERT INTO clients(id,name,remark,token,approved,last_seen,remote_ip,current_task) VALUES(?,?,?,?,?,?,?,?)`,
				req.ClientID, req.Name, "", req.ClientToken, 0, now, realIP(r), "")
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
			FROM tasks WHERE client_id=? AND status='pending'
			ORDER BY created_at ASC LIMIT 1`, clientID).
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
		clientName := clientID
		_ = db.QueryRow(`SELECT name FROM clients WHERE id=?`, clientID).Scan(&clientName)
		go barkPush(cfg.BarkURL, "任务开始",
			fmt.Sprintf("客户端 %s 开始执行任务\n模式:%s 上传:%dMbps 下载:%dMbps 时长:%ds",
				clientName, t.Mode, t.UpMbps, t.DownMbps, t.DurationSec))
		addr := cfg.DataAddr
		if strings.HasPrefix(addr, ":") {
			addr = cfg.ServerHost + addr
		}
		_ = json.NewEncoder(w).Encode(AssignResp{
			ID: t.ID, Mode: t.Mode, UpMbps: t.UpMbps, DownMbps: t.DownMbps,
			DurationSec: t.DurationSec, DataAddr: addr,
		})
	}
}

func handleTaskResult(cfg Config, db *sql.DB, broker *Broker) http.HandlerFunc {
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
		if status == "stopping" {
			finalStatus = "stopped"
		}
		_, _ = db.Exec(`UPDATE tasks SET status=?, finished_at=?, upload_bytes=?, download_bytes=? WHERE id=?`,
			finalStatus, time.Now().Format(time.RFC3339), req.UploadBytes, req.DownloadBytes, req.TaskID)
		_, _ = db.Exec(`UPDATE clients SET current_task='' WHERE id=?`, req.ClientID)
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
		_, _ = db.Exec(`UPDATE tasks SET upload_bytes=?, download_bytes=? WHERE id=?`,
			req.UploadBytes, req.DownloadBytes, req.TaskID)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

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
			UploadBytes   int64   `json:"upload_bytes"`
			DownloadBytes int64   `json:"download_bytes"`
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
				ClientName: clientNames[t.ClientID],
				Mode:       t.Mode, UpMbps: t.UpMbps, DownMbps: t.DownMbps,
				DurationSec: t.DurationSec, Status: t.Status,
				StartedAt: t.StartedAt, FinishedAt: t.FinishedAt,
				UploadGB:      float64(t.UploadBytes) / (1024 * 1024 * 1024),
				DownloadGB:    float64(t.DownloadBytes) / (1024 * 1024 * 1024),
				UploadBytes:   t.UploadBytes,
				DownloadBytes: t.DownloadBytes,
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
<title>服务器 Linux 管理</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif;
  background:#f5f7fb;color:#111827;margin:0;padding:20px}
.wrap{max-width:700px;margin:0 auto}
.card{background:#fff;border:1px solid #e5e7eb;border-radius:14px;padding:22px;margin-bottom:18px;
  box-shadow:0 1px 3px rgba(0,0,0,.05)}
h1{margin:0 0 6px;font-size:26px}h2{margin:0 0 14px;font-size:18px}
.row{display:flex;justify-content:space-between;padding:9px 0;border-bottom:1px solid #f3f4f6;font-size:14px}
.row:last-child{border-bottom:none}
.label{color:#6b7280}
.val{font-family:monospace;font-size:13px;color:#111827;word-break:break-all;text-align:right;max-width:70%%}
button,.btn{border:none;border-radius:9px;padding:10px 18px;background:#2563eb;color:#fff;cursor:pointer;
  font-size:14px;text-decoration:none;display:inline-block}
button:hover,.btn:hover{background:#1d4ed8}
button.danger{background:#ef4444}button.danger:hover{background:#dc2626}
button.sec{background:#e5e7eb;color:#111}button.sec:hover{background:#d1d5db}
.toolbar{display:flex;gap:10px;margin-top:16px;flex-wrap:wrap}
</style></head>
<body><div class="wrap">
<div class="card">
  <h1>🐧 服务器 Linux 管理</h1>
  <p style="color:#6b7280;font-size:14px;margin:0 0 16px">服务状态查看与系统操作</p>
  <a class="btn sec" href="%s">← 返回面板</a>
</div>

<div class="card">
  <h2>📋 运行信息</h2>
  <div class="row"><span class="label">面板监听地址</span><span class="val">%s</span></div>
  <div class="row"><span class="label">数据端口</span><span class="val">%s</span></div>
  <div class="row"><span class="label">服务器 Host</span><span class="val">%s</span></div>
  <div class="row"><span class="label">数据库路径</span><span class="val">%s</span></div>
  <div class="row"><span class="label">面板路径</span><span class="val">%s</span></div>
  <div class="row"><span class="label">Bark 推送</span><span class="val">%s</span></div>
  <div class="row"><span class="label">Bark Token</span><span class="val">%s</span></div>
</div>

<div class="card">
  <h2>⚙️ 系统操作</h2>
  <p style="font-size:13px;color:#6b7280;margin-bottom:14px">重启服务后面板约1秒内不可访问，会自动恢复。</p>
  <div class="toolbar">
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
			RunningTasks    []Task
			HistoryTasks    []Task
			ApprovedClients []approvedClient
			DefaultClientID string
			ClientNames     map[string]string
			PanelPath       string
			ServerHost      string
			InitToken       string
			Version         string
			BarkURL         string
			PanelPathJS     template.JS
			InitTokenJS     template.JS
			VersionJS       template.JS
			GenName         string
			GenRemark       string
			GenVersion      string
			GeneratedCmd    string
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

		var runningTasks, historyTasks []Task
		for _, t := range tasks {
			if t.Status == "running" || t.Status == "pending" || t.Status == "stopping" {
				runningTasks = append(runningTasks, t)
			} else {
				historyTasks = append(historyTasks, t)
			}
		}

		version := getenv("BWPANEL_VERSION", "latest")
		genName := strings.TrimSpace(r.URL.Query().Get("gen_name"))
		genRemark := strings.TrimSpace(r.URL.Query().Get("gen_remark"))
		genVersion := strings.TrimSpace(r.URL.Query().Get("gen_version"))
		generatedCmd := strings.TrimSpace(r.URL.Query().Get("gen_cmd"))
		if genVersion == "" {
			genVersion = version
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
:root{--bg:#f5f7fb;--card:#fff;--border:#e5e7eb;--text:#111827;--muted:#6b7280;--primary:#2563eb;--ph:#1d4ed8;--ok-bg:#dcfce7;--ok:#166534;--no-bg:#fee2e2;--no:#991b1b;--run-bg:#dbeafe;--run:#1d4ed8;--done-bg:#dcfce7;--done:#166534;--pend-bg:#f3f4f6;--pend:#374151;--warn-bg:#fef3c7;--warn:#92400e;--stop-bg:#ffedd5;--stop:#9a3412}
*{box-sizing:border-box}
body{margin:0;padding:20px;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif;background:var(--bg);color:var(--text)}
.wrap{max-width:1400px;margin:0 auto}
.card{background:var(--card);border:1px solid var(--border);border-radius:14px;padding:18px;margin-bottom:18px;box-shadow:0 1px 3px rgba(0,0,0,.05)}
h1{margin:0 0 4px;font-size:36px}h2{margin:0 0 14px;font-size:20px}p{margin:0;color:var(--muted);font-size:14px}
.ver-badge{display:inline-block;background:#dbeafe;color:#1d4ed8;border-radius:999px;padding:2px 10px;font-size:12px;font-weight:700;margin-left:10px;vertical-align:middle}
.toolbar{display:flex;flex-wrap:wrap;gap:8px;margin-top:12px;align-items:center}
.note{font-size:13px;color:var(--muted)}
#liveStatus{font-size:13px;color:var(--muted)}
button,.btn{border:none;border-radius:9px;padding:9px 14px;background:var(--primary);color:#fff;cursor:pointer;font-size:13px;text-decoration:none;display:inline-block;white-space:nowrap}
button:hover,.btn:hover{background:var(--ph)}
button.sec{background:#e5e7eb;color:#111}button.sec:hover{background:#d1d5db}
button.danger{background:#ef4444;color:#fff}button.danger:hover{background:#dc2626}
button.warn{background:#f59e0b;color:#fff}button.warn:hover{background:#d97706}
button.info{background:#0891b2;color:#fff}button.info:hover{background:#0e7490}
.tbl{overflow:auto}
table{width:100%;border-collapse:collapse}
th,td{padding:10px 9px;border-bottom:1px solid var(--border);text-align:left;vertical-align:middle;white-space:nowrap;font-size:13px}
th{background:#f9fafb;font-weight:700}
.badge{display:inline-block;padding:3px 9px;border-radius:999px;font-size:11px;font-weight:700}
.ok{background:var(--ok-bg);color:var(--ok)}.no{background:var(--no-bg);color:var(--no)}
.running{background:var(--run-bg);color:var(--run)}.done{background:var(--done-bg);color:var(--done)}
.pending{background:var(--pend-bg);color:var(--pend)}.stopped{background:var(--warn-bg);color:var(--warn)}
.stopping{background:var(--stop-bg);color:var(--stop)}
.ping-ok{color:#16a34a;font-weight:700}.ping-warn{color:#d97706;font-weight:700}.ping-dead{color:#dc2626;font-weight:700}
.grid{display:grid;grid-template-columns:2fr 1fr 1fr 1fr 1fr 1fr auto;gap:10px;align-items:end}
.dur-wrap{display:flex;gap:6px}.dur-wrap input{flex:1}.dur-wrap select{width:90px}
input,select,textarea{width:100%;padding:10px 12px;border:1px solid var(--border);border-radius:9px;font-size:13px;background:#fff}
textarea{resize:vertical;min-height:60px}
.tip{margin-top:10px;font-size:12px;color:var(--muted)}
.copy-box{display:flex;gap:8px;align-items:center;margin-top:10px}
.copy-box input{font-family:monospace;font-size:12px;background:#f9fafb}
.gen-grid{display:grid;grid-template-columns:1fr 1fr 1fr auto;gap:10px;align-items:end}
.modal-overlay{display:none;position:fixed;inset:0;background:rgba(0,0,0,.45);z-index:999;align-items:center;justify-content:center;pointer-events:none}
.modal-overlay.open{display:flex;pointer-events:auto}
.modal-inner{background:#fff;border-radius:14px;padding:22px;width:min(600px,95vw)}
.modal-inner input{font-family:monospace;font-size:12px;background:#f9fafb}
@media(max-width:960px){.grid{grid-template-columns:1fr 1fr}.gen-grid{grid-template-columns:1fr 1fr}}
@media(max-width:640px){body{padding:10px}.grid,.gen-grid{grid-template-columns:1fr}h1{font-size:26px}}
</style>
</head>
<body>
<div class="wrap">

<div class="card">
  <h1>带宽测试面板 <span class="ver-badge">{{.Version}}</span></h1>
  <p>客户端管理、任务下发和实时状态查看。</p>
  <div class="toolbar">
    <button type="button" id="reloadBtn">手动刷新</button>
    <button type="button" class="sec" id="toggleHistoryBtn">显示历史任务</button>
    <a class="btn sec" href="{{.PanelPath}}/settings">⚙️ Bark 设置</a>
    <a class="btn sec" href="{{.PanelPath}}/server">🐧 服务器 Linux</a>
    <span id="liveStatus" class="note">数据每 5 秒自动刷新。</span>
  </div>
</div>

<!-- 升级命令弹窗 -->
<div id="upgradeModal" class="modal-overlay">
  <div class="modal-inner">
    <h2>📦 客户端升级命令</h2>
    <p style="margin-bottom:12px;font-size:13px;color:var(--muted)">在对应客户端 VPS 上以 root 运行此命令，配置文件将自动保留。</p>
    <div class="copy-box">
      <input id="upgradeCmd" readonly>
      <button type="button" class="sec" id="copyUpgradeBtn">复制</button>
    </div>
    <div style="margin-top:16px;display:flex;gap:8px">
      <button type="button" class="sec" id="closeUpgradeBtn">关闭</button>
    </div>
  </div>
</div>

<!-- 编辑客户端弹窗 -->
<div id="editModal" class="modal-overlay">
  <div class="modal-inner" style="width:min(480px,95vw)">
    <h2>编辑客户端</h2>
    <div style="margin-bottom:10px">
      <label style="font-size:13px;color:var(--muted)">名称</label>
      <input id="editName" placeholder="名称" style="margin-top:4px">
    </div>
    <div style="margin-bottom:14px">
      <label style="font-size:13px;color:var(--muted)">备注</label>
      <textarea id="editRemark" placeholder="备注（可选）" style="margin-top:4px"></textarea>
    </div>
    <div style="display:flex;gap:8px">
      <button type="button" id="saveEditBtn">保存</button>
      <button type="button" class="sec" id="closeEditBtn">取消</button>
    </div>
  </div>
</div>

<div class="card">
  <h2>客户端列表</h2>
  <div class="tbl">
  <table>
    <thead><tr><th>名称</th><th>备注</th><th>已批准</th><th>心跳延迟</th><th>最后心跳</th><th>远程 IP</th><th>当前任务</th><th>操作</th></tr></thead>
    <tbody id="clientBody">
    {{range .Clients}}
    <tr data-client-id="{{.ID}}" data-last-seen="{{.LastSeen}}" data-name="{{.Name}}" data-remark="{{.Remark}}" data-approved="{{if .Approved}}1{{else}}0{{end}}">
      <td>{{.Name}}</td>
      <td>{{.Remark}}</td>
      <td>{{if .Approved}}<span class="badge ok">是</span>{{else}}<span class="badge no">否</span>{{end}}</td>
      <td class="ping-col">-</td>
      <td class="lastseen-col">{{.LastSeen}}</td>
      <td>{{.RemoteIP}}</td>
      <td class="curtask-col" style="font-family:monospace;font-size:11px">{{.CurrentTask}}</td>
      <td>
        <div style="display:flex;gap:6px;flex-wrap:wrap">
	        {{if (not .Approved)}}
	        <form method="post" action="{{$.PanelPath}}/approve" style="margin:0">
	          <input type="hidden" name="client_id" value="{{.ID}}">
	          <button type="submit">批准</button>
	        </form>
	        {{end}}
        <button type="button" class="sec edit-btn" data-id="{{.ID}}" data-name="{{.Name}}" data-remark="{{.Remark}}">编辑</button>
        <button type="button" class="info upgrade-btn" data-id="{{.ID}}" data-name="{{.Name}}">升级命令</button>
        </div>
      </td>
    </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  <div class="tip">心跳延迟：距上次心跳的秒数，超过 60s 变红。创建任务前请确认客户端已批准。</div>
</div>

<div class="card">
  <h2>创建任务</h2>
  <form method="post" action="{{.PanelPath}}/task/create">
    <div class="grid">
      <div>
        <label class="note">选择客户端</label>
        <select name="client_id" required style="margin-top:4px">
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
  <form method="post" action="{{.PanelPath}}/gen/install-cmd">
  <div class="gen-grid">
    <div>
      <label class="note">客户端名称</label>
      <input id="genName" name="gen_name" value="{{.GenName}}" placeholder="例：香港-1号机" style="margin-top:4px">
    </div>
    <div>
      <label class="note">备注（可选）</label>
      <input id="genRemark" name="gen_remark" value="{{.GenRemark}}" placeholder="可选备注" style="margin-top:4px">
    </div>
    <div>
      <label class="note">版本号</label>
      <input id="genVersion" name="gen_version" value="{{.GenVersion}}" style="margin-top:4px">
    </div>
    <div style="align-self:end">
      <button type="submit" id="genBtn" style="width:100%">生成命令</button>
    </div>
  </div>
  </form>
  <div class="copy-box" id="cmdBox" style="display:{{if .GeneratedCmd}}flex{{else}}none{{end}}">
    <input id="cmdText" readonly value="{{.GeneratedCmd}}">
    <button type="button" class="sec" id="copyCmdBtn">复制</button>
  </div>
  <div class="tip" id="cmdTip">{{if .GeneratedCmd}}将此命令复制到客户端 VPS 上执行即可完成安装与注册。{{end}}</div>
</div>

<!-- 正在运行的任务 -->
<div class="card">
  <h2>🟢 正在执行的任务 <span id="taskRefreshHint" style="font-size:12px;color:var(--muted);font-weight:400"></span></h2>
  <div class="tbl">
  <table>
    <thead><tr><th>客户端</th><th>模式</th><th>上传 Mbps</th><th>下载 Mbps</th><th>时长</th><th>状态</th><th>网络延迟</th><th>已上传</th><th>已下载</th><th>开始时间</th><th>操作</th></tr></thead>
    <tbody id="runningTaskBody">
    {{range .RunningTasks}}
    <tr data-task-id="{{.ID}}" data-status="{{.Status}}">
      <td>{{index $.ClientNames .ClientID}}</td>
      <td>{{.Mode}}</td>
      <td>{{.UpMbps}}</td>
      <td>{{.DownMbps}}</td>
      <td>{{.DurationSec}} 秒</td>
      <td><span class="badge {{.Status}}">{{.Status}}</span></td>
      <td class="rtt-col" data-client-id="{{.ClientID}}">-</td>
      <td class="up-col">-</td>
      <td class="down-col">-</td>
      <td>{{.StartedAt}}</td>
	      <td>
	        {{if eq .Status "running"}}
	        <form method="post" action="{{$.PanelPath}}/task/stop" style="margin:0" onsubmit="return confirm('确认停止此任务？')">
	          <input type="hidden" name="task_id" value="{{.ID}}">
	          <button type="submit" class="danger stop-btn">停止</button>
	        </form>
	        {{else}}<span class="note">-</span>{{end}}
	      </td>
    </tr>
    {{end}}
	    {{if eq (len .RunningTasks) 0}}<tr id="noRunningRow"><td colspan="11" style="text-align:center;color:var(--muted);padding:20px">暂无正在执行的任务</td></tr>{{end}}
    </tbody>
  </table>
  </div>
</div>

<!-- 历史任务 -->
<div class="card" id="historyCard" style="display:none">
  <h2>📋 历史任务</h2>
  <div class="tbl">
  <table>
    <thead><tr><th>客户端</th><th>模式</th><th>上传 Mbps</th><th>下载 Mbps</th><th>时长</th><th>状态</th><th>已上传</th><th>已下载</th><th>开始时间</th><th>结束时间</th><th>操作</th></tr></thead>
    <tbody id="historyTaskBody">
    {{range .HistoryTasks}}
    <tr data-task-id="{{.ID}}">
      <td>{{index $.ClientNames .ClientID}}</td>
      <td>{{.Mode}}</td>
      <td>{{.UpMbps}}</td>
      <td>{{.DownMbps}}</td>
      <td>{{.DurationSec}} 秒</td>
      <td><span class="badge {{.Status}}">{{.Status}}</span></td>
      <td>{{printf "%.3f" (divf .UploadBytes 1073741824)}} GB</td>
      <td>{{printf "%.3f" (divf .DownloadBytes 1073741824)}} GB</td>
      <td>{{.StartedAt}}</td>
      <td>{{.FinishedAt}}</td>
	      <td>
	        <form method="post" action="{{$.PanelPath}}/task/delete" style="margin:0" onsubmit="return confirm('确认删除此任务记录？')">
	          <input type="hidden" name="task_id" value="{{.ID}}">
	          <button type="submit" class="danger">删除</button>
	        </form>
	      </td>
    </tr>
    {{end}}
	    {{if eq (len .HistoryTasks) 0}}<tr id="noHistoryRow"><td colspan="11" style="text-align:center;color:var(--muted);padding:20px">暂无历史任务</td></tr>{{end}}
    </tbody>
  </table>
  </div>
</div>

</div>

<script>
(function(){
var PANEL_PATH = {{.PanelPathJS}};
var INIT_TOKEN  = {{.InitTokenJS}};
var PANEL_ADDR  = location.host;
var VERSION     = {{.VersionJS}};

// ── 通用 fetch 封装，自动带 credentials ──
function apiFetch(path, body) {
  var opts = {
    method: 'POST',
    credentials: 'include',
    headers: {'Content-Type': 'application/x-www-form-urlencoded'}
  };
  if (body) opts.body = body;
  return fetch(PANEL_PATH + path, opts);
}

var clientNameMap = {};
document.querySelectorAll('#clientBody tr[data-client-id]').forEach(function(row) {
  clientNameMap[row.dataset.clientId] = row.dataset.name || row.dataset.clientId;
});

function fmtGB(bytes) {
  if (!bytes || bytes === 0) return '0.000 GB';
  return (bytes / 1073741824).toFixed(3) + ' GB';
}
function calcPing(lastSeen) {
  if (!lastSeen) return null;
  var t = new Date(lastSeen);
  if (isNaN(t)) return null;
  return Date.now() - t.getTime();
}
function renderPing(ms) {
  if (ms === null) return { text: '?', cls: 'ping-warn' };
  if (ms < 30000) return { text: ms + ' ms', cls: 'ping-ok' };
  if (ms < 60000) return { text: ms + ' ms', cls: 'ping-warn' };
  return { text: ms + ' ms ⚠', cls: 'ping-dead' };
}
function syncRunningTaskLatency() {
  document.querySelectorAll('#runningTaskBody .rtt-col[data-client-id]').forEach(function(cell) {
    var cid = cell.dataset.clientId;
    var crow = document.querySelector('#clientBody tr[data-client-id="' + cid + '"]');
    var ms = crow ? calcPing(crow.dataset.lastSeen) : null;
    var r = renderPing(ms);
    cell.textContent = r.text;
    cell.className = 'rtt-col ' + r.cls;
  });
}
function tickPing() {
  document.querySelectorAll('#clientBody tr[data-client-id]').forEach(function(row) {
    var cell = row.querySelector('.ping-col');
    if (!cell) return;
    var ms = calcPing(row.dataset.lastSeen);
    var r = renderPing(ms);
    cell.textContent = r.text;
    cell.className = 'ping-col ' + r.cls;
  });
  syncRunningTaskLatency();
}
setInterval(tickPing, 15000);
tickPing();

var knownTaskStatus = {};

// 动态行：停止按钮输出为 form，保证无 JS 也可提交
function buildRunningRow(t) {
  var name = clientNameMap[t.client_id] || t.client_name || t.client_id;
	var stopBtn = t.status === 'running'
	  ? '<form method="post" action="' + PANEL_PATH + '/task/stop" style="margin:0" onsubmit="return confirm(\'确认停止此任务？\')">'
	    + '<input type="hidden" name="task_id" value="' + t.id + '">'
	    + '<button type="submit" class="danger stop-btn">停止</button></form>'
	  : '<span class="note">-</span>';
  return '<tr data-task-id="' + t.id + '" data-status="' + t.status + '">'
    + '<td>' + name + '</td><td>' + t.mode + '</td><td>' + t.up_mbps + '</td><td>' + t.down_mbps + '</td>'
    + '<td>' + t.duration_sec + ' 秒</td>'
    + '<td><span class="badge ' + t.status + '">' + t.status + '</span></td>'
    + '<td class="rtt-col" data-client-id="' + t.client_id + '">-</td>'
    + '<td class="up-col">' + fmtGB(t.upload_bytes) + '</td>'
    + '<td class="down-col">' + fmtGB(t.download_bytes) + '</td>'
    + '<td>' + t.started_at + '</td>'
    + '<td>' + stopBtn + '</td>'
    + '</tr>';
}

// 动态行：删除按钮输出为 form，保证无 JS 也可提交
function buildHistoryRow(t) {
  var name = clientNameMap[t.client_id] || t.client_name || t.client_id;
  return '<tr data-task-id="' + t.id + '">'
    + '<td>' + name + '</td><td>' + t.mode + '</td><td>' + t.up_mbps + '</td><td>' + t.down_mbps + '</td>'
    + '<td>' + t.duration_sec + ' 秒</td>'
    + '<td><span class="badge ' + t.status + '">' + t.status + '</span></td>'
    + '<td>' + fmtGB(t.upload_bytes) + '</td>'
    + '<td>' + fmtGB(t.download_bytes) + '</td>'
    + '<td>' + t.started_at + '</td>'
    + '<td>' + t.finished_at + '</td>'
	  + '<td><form method="post" action="' + PANEL_PATH + '/task/delete" style="margin:0" onsubmit="return confirm(\'确认删除此任务记录？\')">'
	  + '<input type="hidden" name="task_id" value="' + t.id + '">'
	  + '<button type="submit" class="danger">删除</button></form></td>'
	  + '</tr>';
}

function pollData() {
  fetch('/api/data', {credentials: 'include', cache: 'no-store'})
    .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
    .then(function(data) {
      var tasks = data.tasks || [];
      var runningStatuses = ['running', 'pending', 'stopping'];
      var runningTasks = tasks.filter(function(t) { return runningStatuses.indexOf(t.status) !== -1; });
      var historyTasks = tasks.filter(function(t) { return runningStatuses.indexOf(t.status) === -1; });

      var needFullRefresh = false;
      tasks.forEach(function(t) {
        var prev = knownTaskStatus[t.id];
        if (prev !== undefined && prev !== t.status) {
          if ((runningStatuses.indexOf(prev) !== -1) !== (runningStatuses.indexOf(t.status) !== -1)) {
            needFullRefresh = true;
          }
        }
        knownTaskStatus[t.id] = t.status;
      });
      runningTasks.forEach(function(t) {
        if (!document.querySelector('#runningTaskBody [data-task-id="' + t.id + '"]')) needFullRefresh = true;
      });
      historyTasks.forEach(function(t) {
        if (!document.querySelector('#historyTaskBody [data-task-id="' + t.id + '"]')) needFullRefresh = true;
      });
      document.querySelectorAll('#runningTaskBody tr[data-task-id]').forEach(function(row) {
        var id = row.dataset.taskId;
        if (!tasks.some(function(t) { return t.id === id; })) needFullRefresh = true;
      });
      document.querySelectorAll('#historyTaskBody tr[data-task-id]').forEach(function(row) {
        var id = row.dataset.taskId;
        if (!tasks.some(function(t) { return t.id === id; })) needFullRefresh = true;
      });

      if (needFullRefresh) {
        var rb = document.getElementById('runningTaskBody');
        if (rb) rb.innerHTML = runningTasks.length === 0
          ? '<tr id="noRunningRow"><td colspan="11" style="text-align:center;color:var(--muted);padding:20px">暂无正在执行的任务</td></tr>'
          : runningTasks.map(buildRunningRow).join('');
        var hb = document.getElementById('historyTaskBody');
        if (hb) hb.innerHTML = historyTasks.length === 0
          ? '<tr id="noHistoryRow"><td colspan="11" style="text-align:center;color:var(--muted);padding:20px">暂无历史任务</td></tr>'
          : historyTasks.map(buildHistoryRow).join('');
      } else {
        runningTasks.forEach(function(t) {
          var row = document.querySelector('#runningTaskBody [data-task-id="' + t.id + '"]');
          if (!row) return;
          var upCol = row.querySelector('.up-col');
          var dnCol = row.querySelector('.down-col');
          if (upCol) upCol.textContent = fmtGB(t.upload_bytes);
          if (dnCol) dnCol.textContent = fmtGB(t.download_bytes);
          var badge = row.querySelector('.badge');
          if (badge) { badge.className = 'badge ' + t.status; badge.textContent = t.status; }
          // 同步停止按钮状态：running 时显示按钮，否则隐藏
          var opCell = row.cells[row.cells.length - 1];
          if (opCell) {
            var existBtn = opCell.querySelector('.stop-btn');
            if (t.status === 'running' && !existBtn) {
              opCell.innerHTML = '<form method="post" action="' + PANEL_PATH + '/task/stop" style="margin:0" onsubmit="return confirm(\'确认停止此任务？\')"><input type="hidden" name="task_id" value="' + t.id + '"><button type="submit" class="danger stop-btn">停止</button></form>';
            } else if (t.status !== 'running' && existBtn) {
              opCell.innerHTML = '<span class="note">-</span>';
            }
          }
        });
      }

      var clients = data.clients || [];
      clients.forEach(function(c) {
        var row = document.querySelector('#clientBody [data-client-id="' + c.id + '"]');
        if (!row) return;
        if (c.last_seen) row.dataset.lastSeen = c.last_seen;
        var ctCell = row.querySelector('.curtask-col');
        if (ctCell) ctCell.textContent = c.current_task || '';
        if (c.name) clientNameMap[c.id] = c.name;
      });
      tickPing();

      var hint = document.getElementById('taskRefreshHint');
      if (hint) hint.textContent = '(上次刷新: ' + new Date().toLocaleTimeString() + ')';
    })
    .catch(function(){});
}
setInterval(pollData, 5000);
pollData();

var es = new EventSource(PANEL_PATH + '/events');
var liveStatus = document.getElementById('liveStatus');
es.onmessage = function(e) {
  if (e.data !== 'ping' && e.data !== 'ready') {
    pollData();
    if (liveStatus) liveStatus.textContent = '检测到状态变化，已立即刷新。';
  }
};
es.onerror = function() {
  if (liveStatus) liveStatus.textContent = '实时消息流异常，仍会每 5 秒自动刷新数据。';
};

// ── 编辑客户端弹窗 ──
var editClientId = '';
document.getElementById('closeEditBtn').addEventListener('click', function() {
  document.getElementById('editModal').classList.remove('open');
});
document.querySelectorAll('.edit-btn').forEach(function(btn) {
  btn.addEventListener('click', function() {
    editClientId = btn.dataset.id;
    document.getElementById('editName').value   = btn.dataset.name || '';
    document.getElementById('editRemark').value = btn.dataset.remark || '';
    document.getElementById('editModal').classList.add('open');
  });
});
document.getElementById('saveEditBtn').addEventListener('click', function() {
  var name   = document.getElementById('editName').value.trim();
  var remark = document.getElementById('editRemark').value.trim();
  if (!name) { alert('名称不能为空'); return; }
  var saveBtn = document.getElementById('saveEditBtn');
  saveBtn.disabled = true;
  apiFetch('/client/edit',
    'client_id=' + encodeURIComponent(editClientId) +
    '&name='      + encodeURIComponent(name) +
    '&remark='    + encodeURIComponent(remark))
    .then(function(r) {
      if (!r.ok) throw new Error(r.status);
      document.getElementById('editModal').classList.remove('open');
      location.reload();
    })
    .catch(function(err) {
      alert('保存失败: ' + err);
      saveBtn.disabled = false;
    });
});

// ── 升级命令弹窗 ──
document.getElementById('closeUpgradeBtn').addEventListener('click', function() {
  document.getElementById('upgradeModal').classList.remove('open');
});
document.getElementById('copyUpgradeBtn').addEventListener('click', function() {
  var el = document.getElementById('upgradeCmd');
  el.select();
  document.execCommand('copy');
  alert('已复制到剪贴板');
});
document.querySelectorAll('.upgrade-btn').forEach(function(btn) {
  btn.addEventListener('click', function() {
    var clientName = btn.dataset.name || '';
    var panelUrl   = location.protocol + '//' + PANEL_ADDR;
    var ver        = VERSION || 'latest';
    var cmd = "curl --proto '=https' --tlsv1.2 -fsSL "
      + "https://raw.githubusercontent.com/ctsunny/bwtest/main/scripts/install_client.sh"
      + " | bash -s -- "
      + " --server-url " + panelUrl
      + " --init-token " + INIT_TOKEN
      + " --client-name '" + clientName.replace(/'/g, "'\\''") + "'"
      + " --version " + ver;
    document.getElementById('upgradeCmd').value = cmd;
    document.getElementById('upgradeModal').classList.add('open');
  });
});

document.getElementById('copyCmdBtn').addEventListener('click', function() {
  var el = document.getElementById('cmdText');
  el.select();
  document.execCommand('copy');
  alert('已复制到剪贴板');
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
document.addEventListener('keydown', function(e) {
  if (e.key !== 'Escape') return;
  var opened = document.querySelectorAll('.modal-overlay.open');
  for (var i = 0; i < opened.length; i++) {
    opened[i].classList.remove('open');
  }
});

		// ── 手动刷新按钮事件监听 ──
		document.getElementById('reloadBtn').addEventListener('click', function() {
			pollData();
			if (liveStatus) liveStatus.textContent = '手动刷新完成: ' + new Date().toLocaleTimeString();
		});

    var historyOpen = false;
		document.getElementById('toggleHistoryBtn').addEventListener('click', function() {
      historyOpen = !historyOpen;
      var card = document.getElementById('historyCard');
      card.style.display = historyOpen ? 'block' : 'none';
      this.textContent = historyOpen ? '隐藏历史任务' : '显示历史任务';
    });

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
		}).Parse(page))
		_ = tpl.Execute(w, pageData{
			Clients:         clients,
			RunningTasks:    runningTasks,
			HistoryTasks:    historyTasks,
			ApprovedClients: approvedClients,
			DefaultClientID: defaultClientID,
			ClientNames:     clientNames,
			PanelPath:       cfg.PanelPath,
			ServerHost:      cfg.ServerHost,
			InitToken:       cfg.InitToken,
			Version:         version,
			BarkURL:         cfg.BarkURL,
			PanelPathJS:     jsStr(cfg.PanelPath),
			InitTokenJS:     jsStr(cfg.InitToken),
			VersionJS:       jsStr(version),
			GenName:         genName,
			GenRemark:       genRemark,
			GenVersion:      genVersion,
			GeneratedCmd:    generatedCmd,
		})
	}
}

func handleGenInstallCmd(panelPath string, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		name := strings.TrimSpace(r.Form.Get("gen_name"))
		remark := strings.TrimSpace(r.Form.Get("gen_remark"))
		version := strings.TrimSpace(r.Form.Get("gen_version"))
		if version == "" {
			version = "latest"
		}
		q := url.Values{}
		q.Set("gen_name", name)
		q.Set("gen_remark", remark)
		q.Set("gen_version", version)
		if name != "" {
			panelURL := fmt.Sprintf("%s://%s", requestScheme(r), r.Host)
			cmd := "curl --proto '=https' --tlsv1.2 -fsSL " +
				"https://raw.githubusercontent.com/ctsunny/bwtest/main/scripts/install_client.sh | bash -s --" +
				" --server-url " + panelURL +
				" --init-token " + cfg.InitToken +
				" --client-name '" + strings.ReplaceAll(name, "'", "'\\''") + "'" +
				" --version " + version
			if remark != "" {
				cmd += " --remark '" + strings.ReplaceAll(remark, "'", "'\\''") + "'"
			}
			q.Set("gen_cmd", cmd)
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
		_, _ = db.Exec(`DELETE FROM tasks WHERE id=? AND status NOT IN ('running','stopping')`, taskID)
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
<title>Bark 设置</title>
<style>body{font-family:system-ui,sans-serif;max-width:520px;margin:40px auto;padding:0 16px}
input{width:100%%;padding:10px;border:1px solid #e5e7eb;border-radius:8px;font-size:14px;box-sizing:border-box}
button{margin-top:12px;padding:10px 20px;background:#2563eb;color:#fff;border:none;border-radius:8px;cursor:pointer;font-size:14px}
a{color:#2563eb}label{font-size:13px;color:#6b7280}code{background:#f3f4f6;padding:2px 6px;border-radius:4px;font-size:12px}</style></head>
<body><h2>⚙️ Bark 推送设置</h2>
<p style="font-size:13px;color:#6b7280;margin-bottom:16px">只需填写 Bark Token，系统会自动拼接为 <code>https://api.day.app/你的token</code>；留空则关闭推送。</p>
<form method="post">
<label>Bark Token</label><br>
<input name="bark_token" value="%s" placeholder="填你的 Bark token，例：AbCdEfGhXxXx" style="margin-top:6px">
<br><button type="submit">保存</button>
<a href="%s" style="margin-left:12px;font-size:13px">← 返回</a>
</form>
<p style="font-size:12px;color:#9ca3af;margin-top:20px">保存后立即生效，无需重启服务端。配置持久化到 /opt/bwtest/bark_url 文件。</p>
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
		if name != "" {
			_, _ = db.Exec(`UPDATE clients SET name=?, remark=? WHERE id=?`, name, remark, clientID)
		}
		broker.Publish("clients")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func handleCreateTask(panelPath string, cfg Config, db *sql.DB, broker *Broker) http.HandlerFunc {
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
		_, err := db.Exec(`INSERT INTO tasks(id,client_id,mode,up_mbps,down_mbps,duration_sec,status,created_at) VALUES(?,?,?,?,?,?,?,?)`,
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
		if strings.Contains(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") &&
			!strings.Contains(r.Header.Get("Accept"), "application/json") {
			http.Redirect(w, r, panelPath, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
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
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen > out[j].LastSeen })
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
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
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
