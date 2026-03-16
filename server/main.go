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
}

type Client struct {
	ID          string
	Name        string
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
	}

	db := mustInitDB(cfg.DBPath)
	broker := NewBroker()

	log.Printf("panel=%s data=%s db=%s", cfg.PanelAddr, cfg.DataAddr, cfg.DBPath)

	go runDataServer(cfg, db)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", jsonHandler(handleRegister(cfg, db, broker)))
	mux.HandleFunc("/api/heartbeat", jsonHandler(handleHeartbeat(db)))
	mux.HandleFunc("/api/task/next", jsonHandler(handleNextTask(cfg, db, broker)))
	mux.HandleFunc("/api/task/result", jsonHandler(handleTaskResult(db, broker)))
	mux.HandleFunc("/api/task/control", jsonHandler(handleTaskControl(db)))

	mux.Handle("/admin", basicAuth(cfg, http.HandlerFunc(handleAdmin(db))))
	mux.Handle("/admin/approve", basicAuth(cfg, http.HandlerFunc(handleApprove(db, broker))))
	mux.Handle("/admin/task/create", basicAuth(cfg, http.HandlerFunc(handleCreateTask(db, broker))))
	mux.Handle("/admin/task/stop", basicAuth(cfg, http.HandlerFunc(handleStopTask(db, broker))))
	mux.Handle("/admin/events", basicAuth(cfg, http.HandlerFunc(handleEvents(broker))))

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
			token TEXT NOT NULL,
			approved INTEGER NOT NULL DEFAULT 0,
			last_seen TEXT NOT NULL,
			remote_ip TEXT NOT NULL DEFAULT '',
			current_task TEXT NOT NULL DEFAULT ''
		);`,
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
			log.Fatal(err)
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

	deadline := time.Now().Add(time.Duration(duration) * time.Second)

	switch mode {
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
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
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
				INSERT INTO clients(id,name,token,approved,last_seen,remote_ip,current_task)
				VALUES(?,?,?,?,?,?,?)`,
				req.ClientID, req.Name, req.ClientToken, 0, now, realIP(r), "")
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

func handleAdmin(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clients := mustClients(db)
		tasks := mustTasks(db)

		const page = `
<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>BW Panel</title>
<style>
body{font-family:Arial,sans-serif;margin:20px;background:#f7f7f9;color:#222}
h2{margin-top:28px}
.card{background:#fff;border:1px solid #ddd;border-radius:10px;padding:14px;margin-bottom:16px}
table{border-collapse:collapse;width:100%;background:#fff}
th,td{border:1px solid #e5e5e5;padding:8px;font-size:14px}
input,select,button{padding:8px;margin:4px}
.badge{padding:2px 8px;border-radius:12px;font-size:12px}
.ok{background:#d4f7d9}
.no{background:#ffd9d9}
.running{background:#d9ecff}
.done{background:#e8f5e9}
.stopped{background:#fff0c9}
.pending{background:#f0f0f0}
.stopping{background:#ffe5cc}
</style>
</head>
<body>
<div class="card">
<h1>BW Panel</h1>
<p>Client management, task control and live status.</p>
</div>

<h2>Clients</h2>
<table>
<tr><th>ID</th><th>Name</th><th>Approved</th><th>Last Seen</th><th>Remote IP</th><th>Current Task</th><th>Action</th></tr>
{{range .Clients}}
<tr>
<td>{{.ID}}</td>
<td>{{.Name}}</td>
<td>{{if .Approved}}<span class="badge ok">yes</span>{{else}}<span class="badge no">no</span>{{end}}</td>
<td>{{.LastSeen}}</td>
<td>{{.RemoteIP}}</td>
<td>{{.CurrentTask}}</td>
<td>
{{if not .Approved}}
<form method="post" action="/admin/approve">
<input type="hidden" name="client_id" value="{{.ID}}">
<button type="submit">Approve</button>
</form>
{{end}}
</td>
</tr>
{{end}}
</table>

<h2>Create Task</h2>
<div class="card">
<form method="post" action="/admin/task/create">
<input name="client_id" placeholder="client_id" required>
<select name="mode">
<option value="upload">upload</option>
<option value="download">download</option>
<option value="both">both</option>
</select>
<input name="up_mbps" placeholder="up_mbps" value="10">
<input name="down_mbps" placeholder="down_mbps" value="10">
<input name="duration_sec" placeholder="duration_sec" value="60">
<button type="submit">Create Task</button>
</form>
</div>

<h2>Tasks</h2>
<table>
<tr><th>ID</th><th>Client</th><th>Mode</th><th>Up Mbps</th><th>Down Mbps</th><th>Duration</th><th>Status</th><th>Upload Bytes</th><th>Download Bytes</th><th>Started</th><th>Finished</th><th>Action</th></tr>
{{range .Tasks}}
<tr>
<td>{{.ID}}</td>
<td>{{.ClientID}}</td>
<td>{{.Mode}}</td>
<td>{{.UpMbps}}</td>
<td>{{.DownMbps}}</td>
<td>{{.DurationSec}} s</td>
<td><span class="badge {{.Status}}">{{.Status}}</span></td>
<td>{{.UploadBytes}}</td>
<td>{{.DownloadBytes}}</td>
<td>{{.StartedAt}}</td>
<td>{{.FinishedAt}}</td>
<td>
{{if eq .Status "running"}}
<form method="post" action="/admin/task/stop">
<input type="hidden" name="task_id" value="{{.ID}}">
<button type="submit">Stop</button>
</form>
{{end}}
</td>
</tr>
{{end}}
</table>

<script>
const es = new EventSource('/admin/events');
es.onmessage = function() { location.reload(); };
</script>
</body>
</html>`
		tpl := template.Must(template.New("page").Parse(page))
		_ = tpl.Execute(w, map[string]any{
			"Clients": clients,
			"Tasks":   tasks,
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

func handleApprove(db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		clientID := r.Form.Get("client_id")
		_, _ = db.Exec(`UPDATE clients SET approved=1 WHERE id=?`, clientID)
		broker.Publish("clients")
		http.Redirect(w, r, "/admin", http.StatusFound)
	}
}

func handleCreateTask(db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()

		clientID := r.Form.Get("client_id")
		mode := r.Form.Get("mode")
		up, _ := strconv.Atoi(r.Form.Get("up_mbps"))
		down, _ := strconv.Atoi(r.Form.Get("down_mbps"))
		dur, _ := strconv.Atoi(r.Form.Get("duration_sec"))
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
		http.Redirect(w, r, "/admin", http.StatusFound)
	}
}

func handleStopTask(db *sql.DB, broker *Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		taskID := r.Form.Get("task_id")
		_, _ = db.Exec(`UPDATE tasks SET status='stopping' WHERE id=? AND status='running'`, taskID)
		broker.Publish("tasks")
		http.Redirect(w, r, "/admin", http.StatusFound)
	}
}

func mustClients(db *sql.DB) []Client {
	rows, err := db.Query(`SELECT id,name,approved,last_seen,remote_ip,current_task FROM clients`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Client
	for rows.Next() {
		var c Client
		var approved int
		_ = rows.Scan(&c.ID, &c.Name, &approved, &c.LastSeen, &c.RemoteIP, &c.CurrentTask)
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
