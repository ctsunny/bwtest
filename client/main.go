package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var Version = "v0.4.20"

type Config struct {
	ServerURL   string `json:"server_url"`
	Name        string `json:"name"`
	InitToken   string `json:"init_token"`
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
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

type Task struct {
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

type PeerAddrReq struct {
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
	TaskID      string `json:"task_id"`
	Port        string `json:"port"`
}

func normalizeTaskMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "both":
		return "traditional"
	default:
		return strings.TrimSpace(mode)
	}
}

var busy int32
var httpClient = &http.Client{Timeout: 15 * time.Second}
var sshAttemptsLastHour int64

// shutdownCh is closed when a shutdown signal is received.
var shutdownCh = make(chan struct{})
var shutdownOnce sync.Once

// shutdown closes shutdownCh exactly once; safe to call from multiple goroutines.
func shutdown() {
	shutdownOnce.Do(func() { close(shutdownCh) })
}

// defaultConfigPath returns the platform-appropriate config file path.
func defaultConfigPath() string {
	if runtime.GOOS == "windows" {
		programData := os.Getenv("ProgramData")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, "bwagent", "config.json")
	}
	return "/etc/bwagent/config.json"
}

type logBuffer struct {
	lines []string
	max   int
	mu    sync.Mutex
}

func (lb *logBuffer) Write(p []byte) (n int, err error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	s := string(p)
	lb.lines = append(lb.lines, s)
	if len(lb.lines) > lb.max {
		lb.lines = lb.lines[1:]
	}
	return len(p), nil
}

func (lb *logBuffer) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	var buf bytes.Buffer
	for _, l := range lb.lines {
		buf.WriteString(l)
	}
	return buf.String()
}

func (lb *logBuffer) Clear() {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.lines = nil
}

var logBuf = &logBuffer{max: 200}

func main() {
	cfgPath := defaultConfigPath()
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	// On Windows, detect if we are running as a Windows service and hand off
	// control to the SCM. maybeRunAsWindowsService blocks until the service
	// stops and returns true; it is a no-op (returns false) on non-Windows.
	if maybeRunAsWindowsService(func() { runAgent(cfgPath) }) {
		return
	}

	runAgent(cfgPath)
}

// runAgent initialises the config, sets up graceful shutdown, and runs the
// register / heartbeat / poll loops. It is called both from main() directly
// (interactive) and from the Windows service handler goroutine.
func runAgent(cfgPath string) {
	cfg, err := loadOrCreateConfig(cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	log.SetOutput(io.MultiWriter(os.Stdout, logBuf))
	log.Printf("bwagent %s starting...", Version)

	// Graceful shutdown: close shutdownCh on SIGINT / SIGTERM so that
	// all loops can exit cleanly without abruptly killing running tasks.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		s := <-sig
		log.Printf("bwagent: received %v, shutting down gracefully...", s)
		shutdown()
	}()

	// register with retry
	for {
		select {
		case <-shutdownCh:
			log.Printf("bwagent: shutdown before registration")
			return
		default:
		}
		if err := register(cfg); err != nil {
			log.Printf("register error: %v, retrying in 10s...", err)
			select {
			case <-time.After(10 * time.Second):
			case <-shutdownCh:
				return
			}
			continue
		}
		break
	}

	refreshSSHFailedAttempts()
	go sshMonitorLoop(cfg)
	go heartbeatLoop(cfg)
	pollLoop(cfg)
}

func loadOrCreateConfig(path string) (*Config, error) {
	if b, err := os.ReadFile(path); err == nil {
		// Strip UTF-8 BOM written by Windows PowerShell 5's Set-Content -Encoding UTF8.
		b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
		var cfg Config
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, err
		}
		if cfg.ClientID == "" {
			cfg.ClientID = token(8)
		}
		if cfg.ClientToken == "" {
			cfg.ClientToken = token(16)
		}
		b2, _ := json.MarshalIndent(cfg, "", "  ")
		_ = os.WriteFile(path, b2, 0600)
		return &cfg, nil
	}

	cfg := &Config{
		ServerURL:   getenv("SERVER_URL", "http://127.0.0.1:8080"),
		Name:        getenv("CLIENT_NAME", hostname()),
		InitToken:   getenv("INIT_TOKEN", ""),
		ClientID:    token(8),
		ClientToken: token(16),
	}

	_ = os.MkdirAll(filepath.Dir(path), 0755)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(path, b, 0600); err != nil {
		return nil, err
	}
	return cfg, nil
}

func register(cfg *Config) error {
	ver := Version
	if ver == "" {
		ver = getenv("BWAGENT_VERSION", "unknown")
	}
	return postJSON(cfg.ServerURL+"/api/register", RegisterReq{
		ClientID:    cfg.ClientID,
		ClientToken: cfg.ClientToken,
		Name:        cfg.Name,
		InitToken:   cfg.InitToken,
		Version:     ver,
	}, nil)
}

func heartbeatLoop(cfg *Config) {
	tk := time.NewTicker(15 * time.Second)
	defer tk.Stop()
	var lastLatency int
	for range tk.C {
		var resp struct {
			OK               bool   `json:"ok"`
			UpgradeTo        string `json:"upgrade_to"`
			RestartRequested bool   `json:"restart_requested"`
		}
		ver := Version
		if ver == "" {
			ver = getenv("BWAGENT_VERSION", "unknown")
		}
		req := HeartbeatReq{
			ClientID:    cfg.ClientID,
			ClientToken: cfg.ClientToken,
			Version:     ver,
			Latency:     lastLatency,
			SSHAttempts: int(atomic.LoadInt64(&sshAttemptsLastHour)),
		}

		start := time.Now()
		err := postJSON(cfg.ServerURL+"/api/heartbeat", req, &resp)
		if err == nil {
			lastLatency = int(time.Since(start).Milliseconds())
		} else {
			lastLatency = -1
		}

		if resp.UpgradeTo != "" {
			ver := Version
			if ver == "" {
				ver = getenv("BWAGENT_VERSION", "")
			}
			if resp.UpgradeTo != ver {
				log.Printf("[upgrade] 服务端要求升级到 %s，当前版本 %s，开始自动升级...", resp.UpgradeTo, ver)
				go selfUpgrade(resp.UpgradeTo)
			}
		}
		if resp.RestartRequested {
			log.Printf("[restart] 服务端要求重启 bwagent 服务")
			go restartAgentService("restart")
		}
	}
}

func pollLoop(cfg *Config) {
	tk := time.NewTicker(5 * time.Second)
	defer tk.Stop()

	for {
		select {
		case <-shutdownCh:
			// Wait for any in-flight task to finish before exiting.
			log.Printf("pollLoop: shutdown signal received, waiting for in-flight task...")
			for atomic.LoadInt32(&busy) == 1 {
				time.Sleep(500 * time.Millisecond)
			}
			log.Printf("pollLoop: all tasks done, exiting")
			return
		case <-tk.C:
		}

		if atomic.LoadInt32(&busy) == 1 {
			continue
		}

		task, err := getNextTask(cfg)
		if err != nil || task == nil || task.ID == "" {
			continue
		}

		atomic.StoreInt32(&busy, 1)
		go func(t *Task) {
			defer atomic.StoreInt32(&busy, 0)
			logBuf.Clear() // 任务开始前清空日志，确保只上报当前任务运行的日志
			up, down, status := runTaskWithRetry(cfg, t)
			_ = postJSON(cfg.ServerURL+"/api/task/result", ResultReq{
				ClientID:      cfg.ClientID,
				ClientToken:   cfg.ClientToken,
				TaskID:        t.ID,
				Status:        status,
				UploadBytes:   up,
				DownloadBytes: down,
				Logs:          logBuf.String(),
			}, nil)
		}(task)
	}
}

func getNextTask(cfg *Config) (*Task, error) {
	url := fmt.Sprintf("%s/api/task/next?client_id=%s&client_token=%s", cfg.ServerURL, cfg.ClientID, cfg.ClientToken)
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 204 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}

	var task Task
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return nil, err
	}
	return &task, nil
}

func taskControl(cfg *Config, taskID string) (string, error) {
	url := fmt.Sprintf("%s/api/task/control?client_id=%s&client_token=%s&task_id=%s",
		cfg.ServerURL, cfg.ClientID, cfg.ClientToken, taskID)
	resp, err := httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("status=%d", resp.StatusCode)
	}

	var c ControlResp
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return "", err
	}
	return c.Status, nil
}

func progressLoop(cfg *Config, taskID string, upBytes, downBytes *int64, stop <-chan struct{}) {
	tk := time.NewTicker(5 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			_ = postJSON(cfg.ServerURL+"/api/task/progress", ProgressReq{
				ClientID:      cfg.ClientID,
				ClientToken:   cfg.ClientToken,
				TaskID:        taskID,
				UploadBytes:   atomic.LoadInt64(upBytes),
				DownloadBytes: atomic.LoadInt64(downBytes),
				Logs:          logBuf.String(),
			}, nil)
		}
	}
}

// runTaskWithRetry wraps runTask: if the data connection drops before deadline,
// it waits 5s and reconnects, accumulating total bytes, until the task expires or is stopped.
func runTaskWithRetry(cfg *Config, t *Task) (int64, int64, string) {
	deadline := time.Now().Add(time.Duration(t.DurationSec) * time.Second)
	var stopFlag int32

	// global control poller shared across reconnects
	go func() {
		tk := time.NewTicker(2 * time.Second)
		defer tk.Stop()
		var failCount int
		for range tk.C {
			if time.Now().After(deadline) {
				return
			}
			status, err := taskControl(cfg, t.ID)
			if err != nil {
				failCount++
				// 连续失败5次（约10秒）才认为任务被外部终止，增加对 transient API 错误的鲁棒性
				if failCount >= 5 {
					log.Printf("[task %s] control check error (consecutive %d): %v, stopping", t.ID, failCount, err)
					atomic.StoreInt32(&stopFlag, 1)
					return
				}
				log.Printf("[task %s] control check transient error (%d/5): %v, retrying...", t.ID, failCount, err)
				continue
			}
			failCount = 0
			// stopping: 服务端请求停止; stopped/done: 服务端已直接终止（如重启重置、看门狗超时）
			if status == "stopping" || status == "stopped" || status == "done" {
				log.Printf("[task %s] server requested %s", t.ID, status)
				atomic.StoreInt32(&stopFlag, 1)
				return
			}
		}
	}()

	var totalUp, totalDown int64
	progressStop := make(chan struct{})
	go progressLoop(cfg, t.ID, &totalUp, &totalDown, progressStop)
	defer close(progressStop)

	// Generate intermission schedule if density > 0
	var schedule []timeSlot
	if t.Density > 0 {
		schedule = buildIntermissionSchedule(time.Now(), deadline, t.Density)
		log.Printf("[task %s] density=%d%%, generated %d intermission slots", t.ID, t.Density, countPauses(schedule))
	}

	for {
		if time.Now().After(deadline) {
			return totalUp, totalDown, "done"
		}
		if atomic.LoadInt32(&stopFlag) == 1 {
			return totalUp, totalDown, "stopped"
		}

		// If density schedule is active, check if we should pause
		if len(schedule) > 0 {
			now := time.Now()
			slot := currentSlot(schedule, now)
			if slot != nil && !slot.active {
				// We are in a pause slot — sleep until slot ends
				pauseEnd := slot.end
				if pauseEnd.After(deadline) {
					pauseEnd = deadline
				}
				log.Printf("[task %s] intermission pause until %s (%.0fs)",
					t.ID, pauseEnd.Format("15:04:05"), time.Until(pauseEnd).Seconds())
				for time.Now().Before(pauseEnd) {
					if atomic.LoadInt32(&stopFlag) == 1 {
						return totalUp, totalDown, "stopped"
					}
					if time.Now().After(deadline) {
						return totalUp, totalDown, "done"
					}
					time.Sleep(500 * time.Millisecond)
				}
				continue
			}
			// Find active slot end to limit sub-task duration
			if slot != nil && slot.active {
				activeEnd := slot.end
				if activeEnd.After(deadline) {
					activeEnd = deadline
				}
				remainSec := int(time.Until(activeEnd).Seconds())
				if remainSec <= 0 {
					continue
				}
				sub := *t
				sub.DurationSec = remainSec
				sub.Density = 0 // sub-task doesn't manage density

				up, down, status := runTaskOnce(cfg, &sub, activeEnd, &stopFlag, &totalUp, &totalDown)
				totalUp = up
				totalDown = down

				if status == "stopped" {
					return totalUp, totalDown, "stopped"
				}
				if time.Now().After(deadline) {
					return totalUp, totalDown, "done"
				}
				if status != "done" {
					// connection error — wait and retry
					log.Printf("[task %s] connection lost, retrying in 5s... (remain %.0fs)", t.ID, time.Until(deadline).Seconds())
					for i := 0; i < 5; i++ {
						time.Sleep(time.Second)
						if time.Now().After(deadline) || atomic.LoadInt32(&stopFlag) == 1 {
							break
						}
					}
				}
				continue
			}
		}

		// No density or outside scheduled slots: normal execution
		remainSec := int(time.Until(deadline).Seconds())
		if remainSec <= 0 {
			return totalUp, totalDown, "done"
		}

		// build a sub-task with remaining duration
		sub := *t
		sub.DurationSec = remainSec

		up, down, status := runTaskOnce(cfg, &sub, deadline, &stopFlag, &totalUp, &totalDown)
		totalUp = up
		totalDown = down

		if status == "stopped" {
			return totalUp, totalDown, "stopped"
		}
		if status == "done" || time.Now().After(deadline) {
			return totalUp, totalDown, "done"
		}

		// connection error — wait and retry
		log.Printf("[task %s] connection lost, retrying in 5s... (remain %.0fs)", t.ID, time.Until(deadline).Seconds())
		for i := 0; i < 5; i++ {
			time.Sleep(time.Second)
			if time.Now().After(deadline) || atomic.LoadInt32(&stopFlag) == 1 {
				break
			}
		}
	}
}

// ── Intermission schedule ─────────────────────────────────────────────────────

// timeSlot represents a time interval that is either active (running) or paused.
type timeSlot struct {
	start  time.Time
	end    time.Time
	active bool
}

// buildIntermissionSchedule creates a schedule of active/paused slots for the
// entire task duration. Within each hour, `densityPct`% of the time is paused
// in randomly-placed, non-continuous intervals using crypto/rand for randomness.
func buildIntermissionSchedule(start, deadline time.Time, densityPct int) []timeSlot {
	totalDur := deadline.Sub(start)
	if totalDur <= 0 {
		return nil
	}

	var slots []timeSlot
	cursor := start

	for cursor.Before(deadline) {
		// Work in 1-hour chunks (or remaining time if < 1 hour)
		chunkEnd := cursor.Add(time.Hour)
		if chunkEnd.After(deadline) {
			chunkEnd = deadline
		}
		chunkDur := chunkEnd.Sub(cursor)
		pauseTotal := time.Duration(float64(chunkDur) * float64(densityPct) / 100.0)

		// Split the pause time into 2–5 random pause intervals per hour
		numPauses := 2 + cryptoRandIntn(4) // 2 to 5 pauses
		if chunkDur < 30*time.Second {
			numPauses = 1
		}

		// Generate random pause start points within the chunk
		type interval struct {
			offsetStart time.Duration
			dur         time.Duration
		}
		pauses := make([]interval, 0, numPauses)
		remainPause := pauseTotal

		for i := 0; i < numPauses && remainPause > 0; i++ {
			var pd time.Duration
			if i == numPauses-1 {
				pd = remainPause
			} else {
				maxPart := remainPause / time.Duration(numPauses-i)
				msMax := safeMs(maxPart) + 1
				pd = time.Duration(cryptoRandIntn(msMax)) * time.Millisecond
				if pd < time.Second {
					pd = time.Second
				}
			}
			if pd > remainPause {
				pd = remainPause
			}
			// Random offset within chunk (leaving room for pause duration)
			maxOffset := chunkDur - pd
			if maxOffset < 0 {
				maxOffset = 0
			}
			offset := time.Duration(cryptoRandIntn(safeMs(maxOffset)+1)) * time.Millisecond
			pauses = append(pauses, interval{offsetStart: offset, dur: pd})
			remainPause -= pd
		}

		// Sort pauses by start offset, then merge overlapping ones
		sort.Slice(pauses, func(i, j int) bool {
			return pauses[i].offsetStart < pauses[j].offsetStart
		})

		// Merge overlapping intervals
		merged := make([]interval, 0, len(pauses))
		for _, p := range pauses {
			if len(merged) > 0 {
				last := &merged[len(merged)-1]
				lastEnd := last.offsetStart + last.dur
				if p.offsetStart <= lastEnd {
					newEnd := p.offsetStart + p.dur
					if newEnd > lastEnd {
						last.dur = newEnd - last.offsetStart
					}
					continue
				}
			}
			merged = append(merged, p)
		}

		// Convert merged pauses into active/pause slots
		slotCursor := cursor
		for _, p := range merged {
			pauseStart := cursor.Add(p.offsetStart)
			if pauseStart.After(slotCursor) {
				slots = append(slots, timeSlot{start: slotCursor, end: pauseStart, active: true})
			}
			pauseEnd := pauseStart.Add(p.dur)
			if pauseEnd.After(chunkEnd) {
				pauseEnd = chunkEnd
			}
			slots = append(slots, timeSlot{start: pauseStart, end: pauseEnd, active: false})
			slotCursor = pauseEnd
		}
		if slotCursor.Before(chunkEnd) {
			slots = append(slots, timeSlot{start: slotCursor, end: chunkEnd, active: true})
		}

		cursor = chunkEnd
	}

	return slots
}

// currentSlot returns the slot that covers time t, or nil if none found.
func currentSlot(schedule []timeSlot, t time.Time) *timeSlot {
	for i := range schedule {
		if !t.Before(schedule[i].start) && t.Before(schedule[i].end) {
			return &schedule[i]
		}
	}
	return nil
}

func countPauses(schedule []timeSlot) int {
	n := 0
	for _, s := range schedule {
		if !s.active {
			n++
		}
	}
	return n
}

// cryptoRandIntn returns a cryptographically random int in [0, n).
func cryptoRandIntn(n int) int {
	if n <= 0 {
		return 0
	}
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	v := int(buf[0])<<24 | int(buf[1])<<16 | int(buf[2])<<8 | int(buf[3])
	if v < 0 {
		v = -v
	}
	return v % n
}

// safeMs converts a time.Duration to milliseconds as int, capping at max safe value.
func safeMs(d time.Duration) int {
	ms := d / time.Millisecond
	if ms < 0 {
		return 0
	}
	const maxSafe = 1<<31 - 2 // avoid overflow when caller adds 1
	if ms > time.Duration(maxSafe) {
		return maxSafe
	}
	return int(ms)
}

func openTaskConn(cfg *Config, t *Task, mode string, mbps int, duration time.Duration) (net.Conn, error) {
	rateMbps := 0
	pathMode := "upload"
	if mode == "download" {
		rateMbps = mbps
		pathMode = mode
	}
	conn, err := net.DialTimeout("tcp", t.DataAddr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	hello, _ := json.Marshal(DataHello{
		ClientID:    cfg.ClientID,
		ClientToken: cfg.ClientToken,
		TaskID:      t.ID,
		Mode:        mode,
		RateMbps:    rateMbps,
		DurationSec: max(1, int(duration.Seconds())),
	})
	helloStr := string(hello) + "\n"
	reqHeader := fmt.Sprintf("POST /api/video/%s HTTP/1.1\r\nHost: cdn-local.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: keep-alive\r\n\r\n", pathMode, len(helloStr))
	if _, err := conn.Write(append([]byte(reqHeader), []byte(helloStr)...)); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func waitForPhase(phaseDeadline time.Time, stopFlag *int32) {
	for time.Now().Before(phaseDeadline) {
		if atomic.LoadInt32(stopFlag) == 1 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func runOneWayPhase(cfg *Config, t *Task, helloMode string, mbps int, phaseDeadline time.Time, stopFlag *int32, totalUp, totalDown *int64) bool {
	duration := time.Until(phaseDeadline)
	if duration <= 0 {
		return true
	}
	conn, err := openTaskConn(cfg, t, helloMode, mbps, duration)
	if err != nil {
		log.Printf("dial %s error: %v", helloMode, err)
		return false
	}
	defer conn.Close()

	shouldStop := func() bool {
		return time.Now().After(phaseDeadline) || atomic.LoadInt32(stopFlag) == 1
	}

	switch helloMode {
	case "upload":
		pacedUpload(conn, mbps, shouldStop, totalUp)
	case "download":
		readCount(conn, shouldStop, totalDown)
	default:
		return false
	}
	return true
}

func scaledRate(base int, minRatio, maxRatio float64) int {
	if base <= 0 {
		return 0
	}
	minBound := minRatio
	maxBound := maxRatio
	if minBound < 0 {
		minBound = 0
	}
	if maxBound > 1 {
		maxBound = 1
	}
	if maxBound < minBound {
		maxBound = minBound
	}
	ratio := minBound
	if maxBound > minBound {
		ratio += mrand.Float64() * (maxBound - minBound)
	}
	rate := int(float64(base) * ratio)
	if rate <= 0 {
		rate = 1
	}
	if rate > base {
		rate = base
	}
	return rate
}

func phaseDuration(remain time.Duration, minSec, maxSec int) time.Duration {
	if remain <= 0 {
		return 0
	}
	if maxSec < minSec {
		maxSec = minSec
	}
	sec := minSec
	if maxSec > minSec {
		sec += mrand.Intn(maxSec - minSec + 1)
	}
	d := time.Duration(sec) * time.Second
	if d > remain {
		return remain
	}
	return d
}

func runTraditionalMode(cfg *Config, t *Task, deadline time.Time, stopFlag *int32, totalUp, totalDown *int64) string {
	remain := time.Until(deadline)
	if remain <= 0 {
		return "done"
	}
	shouldStop := func() bool {
		return time.Now().After(deadline) || atomic.LoadInt32(stopFlag) == 1
	}

	if t.UpMbps <= 0 && t.DownMbps <= 0 {
		waitForPhase(deadline, stopFlag)
	} else if t.UpMbps <= 0 {
		if !runOneWayPhase(cfg, t, "download", t.DownMbps, deadline, stopFlag, totalUp, totalDown) {
			return "connfail"
		}
	} else if t.DownMbps <= 0 {
		if !runOneWayPhase(cfg, t, "upload", t.UpMbps, deadline, stopFlag, totalUp, totalDown) {
			return "connfail"
		}
	} else {
		upConn, err := openTaskConn(cfg, t, "upload", t.UpMbps, remain)
		if err != nil {
			log.Printf("dial upload error: %v", err)
			return "connfail"
		}
		defer upConn.Close()

		downConn, err := openTaskConn(cfg, t, "download", t.DownMbps, remain)
		if err != nil {
			log.Printf("dial download error: %v", err)
			return "connfail"
		}
		defer downConn.Close()

		done := make(chan struct{})
		go func() {
			readCount(downConn, shouldStop, totalDown)
			close(done)
		}()
		pacedUpload(upConn, t.UpMbps, shouldStop, totalUp)
		<-done
	}

	if atomic.LoadInt32(stopFlag) == 1 {
		return "stopped"
	}
	if time.Now().After(deadline) {
		return "done"
	}
	return "connfail"
}

func runProfileMode(cfg *Config, t *Task, deadline time.Time, stopFlag *int32, totalUp, totalDown *int64) string {
	for {
		if time.Now().After(deadline) {
			return "done"
		}
		if atomic.LoadInt32(stopFlag) == 1 {
			return "stopped"
		}

		remain := time.Until(deadline)
		switch normalizeTaskMode(t.Mode) {
		case "browse":
			if t.UpMbps > 0 {
				phase := time.Now().Add(phaseDuration(remain, 2, 4))
				if !runOneWayPhase(cfg, t, "upload", scaledRate(t.UpMbps, 0.05, 0.18), phase, stopFlag, totalUp, totalDown) {
					return "connfail"
				}
			}
			if atomic.LoadInt32(stopFlag) == 1 || time.Now().After(deadline) {
				continue
			}
			if t.DownMbps > 0 {
				phase := time.Now().Add(phaseDuration(time.Until(deadline), 5, 12))
				if !runOneWayPhase(cfg, t, "download", scaledRate(t.DownMbps, 0.55, 0.95), phase, stopFlag, totalUp, totalDown) {
					return "connfail"
				}
			}
			waitForPhase(time.Now().Add(phaseDuration(time.Until(deadline), 2, 7)), stopFlag)
		case "stream":
			if t.DownMbps > 0 {
				phase := time.Now().Add(phaseDuration(remain, 10, 24))
				if !runOneWayPhase(cfg, t, "download", scaledRate(t.DownMbps, 0.75, 1.0), phase, stopFlag, totalUp, totalDown) {
					return "connfail"
				}
			}
			if atomic.LoadInt32(stopFlag) == 1 || time.Now().After(deadline) {
				continue
			}
			if t.UpMbps > 0 {
				phase := time.Now().Add(phaseDuration(time.Until(deadline), 1, 3))
				if !runOneWayPhase(cfg, t, "upload", scaledRate(t.UpMbps, 0.03, 0.12), phase, stopFlag, totalUp, totalDown) {
					return "connfail"
				}
			}
			waitForPhase(time.Now().Add(phaseDuration(time.Until(deadline), 1, 4)), stopFlag)
		case "backup":
			if t.UpMbps > 0 {
				phase := time.Now().Add(phaseDuration(remain, 8, 20))
				if !runOneWayPhase(cfg, t, "upload", scaledRate(t.UpMbps, 0.7, 1.0), phase, stopFlag, totalUp, totalDown) {
					return "connfail"
				}
			}
			if atomic.LoadInt32(stopFlag) == 1 || time.Now().After(deadline) {
				continue
			}
			if t.DownMbps > 0 {
				phase := time.Now().Add(phaseDuration(time.Until(deadline), 1, 3))
				if !runOneWayPhase(cfg, t, "download", scaledRate(t.DownMbps, 0.03, 0.1), phase, stopFlag, totalUp, totalDown) {
					return "connfail"
				}
			}
			waitForPhase(time.Now().Add(phaseDuration(time.Until(deadline), 2, 6)), stopFlag)
		default:
			return "failed"
		}
	}
}

// runTaskOnce attempts a single connection/profile run and runs until done/stopped/error.
// totalUp/totalDown are the running accumulators (passed in, updated in place).
func runTaskOnce(cfg *Config, t *Task, deadline time.Time, stopFlag *int32, totalUp, totalDown *int64) (int64, int64, string) {
	switch normalizeTaskMode(t.Mode) {
	case "upload":
		if !runOneWayPhase(cfg, t, "upload", t.UpMbps, deadline, stopFlag, totalUp, totalDown) {
			return *totalUp, *totalDown, "connfail"
		}
	case "download":
		if !runOneWayPhase(cfg, t, "download", t.DownMbps, deadline, stopFlag, totalUp, totalDown) {
			return *totalUp, *totalDown, "connfail"
		}
	case "traditional":
		return *totalUp, *totalDown, runTraditionalMode(cfg, t, deadline, stopFlag, totalUp, totalDown)
	case "browse", "stream", "backup":
		return *totalUp, *totalDown, runProfileMode(cfg, t, deadline, stopFlag, totalUp, totalDown)
	case "peer":
		return *totalUp, *totalDown, runPeerMode(cfg, t, deadline, stopFlag, totalUp, totalDown)
	default:
		return *totalUp, *totalDown, "failed"
	}

	if atomic.LoadInt32(stopFlag) == 1 {
		return *totalUp, *totalDown, "stopped"
	}
	if time.Now().After(deadline) {
		return *totalUp, *totalDown, "done"
	}
	return *totalUp, *totalDown, "connfail"
}

func pacedUpload(w io.Writer, mbps int, stop func() bool, counter *int64) int64 {
	if mbps <= 0 {
		return 0
	}
	bytesPerSec := int64(mbps) * 1024 * 1024 / 8
	buf := make([]byte, 32*1024)
	_, _ = rand.Read(buf)

	var total int64
	for !stop() {
		sleepMs := mrand.Intn(1000) + 500  // Sleep 500ms ~ 1.5s
		for i := 0; i < sleepMs/200; i++ { // Poll stop() during sleep
			time.Sleep(200 * time.Millisecond)
			if stop() {
				return total
			}
		}
		if stop() {
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
		for left > 0 && !stop() {
			n := int64(len(buf))
			if n > left {
				n = left
			}
			wr, err := w.Write(buf[:n])
			if err != nil {
				return total
			}
			total += int64(wr)
			atomic.AddInt64(counter, int64(wr))
			left -= int64(wr)
		}
	}
	return total
}

func readCount(conn net.Conn, stop func() bool, counter *int64) int64 {
	buf := make([]byte, 64*1024)
	var total int64
	for !stop() {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := conn.Read(buf)
		if n > 0 {
			total += int64(n)
			atomic.AddInt64(counter, int64(n))
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return total
		}
	}
	return total
}

type sshLogEntry struct {
	Timestamp time.Time
	Message   string
}

// ── Peer Transfer ─────────────────────────────────────────────────────────────

// reportPeerAddr calls the server to register the local listen port for this
// source peer task.  The server will then fill in the target task's peer_addr.
func reportPeerAddr(cfg *Config, taskID, port string) error {
	return postJSON(cfg.ServerURL+"/api/peer/addr", PeerAddrReq{
		ClientID:    cfg.ClientID,
		ClientToken: cfg.ClientToken,
		TaskID:      taskID,
		Port:        port,
	}, nil)
}

// runPeerMode runs a direct peer-to-peer bandwidth test.
//
// Source role:
//   - Listens on a random TCP port.
//   - Reports the port to the server via /api/peer/addr.
//   - Waits up to 30 s for the target to connect.
//   - Once connected, both sides send and receive simultaneously at UpMbps.
//
// Target role:
//   - Connects to t.PeerAddr (filled in by the server from the source's report).
//   - Runs bidirectional transfer until deadline.
func runPeerMode(cfg *Config, t *Task, deadline time.Time, stopFlag *int32, totalUp, totalDown *int64) string {
	switch t.PeerRole {
	case "source":
		return runPeerSource(cfg, t, deadline, stopFlag, totalUp, totalDown)
	case "target":
		return runPeerTarget(cfg, t, deadline, stopFlag, totalUp, totalDown)
	default:
		log.Printf("[peer task %s] unknown peer_role %q", t.ID, t.PeerRole)
		return "failed"
	}
}

func runPeerSource(cfg *Config, t *Task, deadline time.Time, stopFlag *int32, totalUp, totalDown *int64) string {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Printf("[peer-src %s] listen error: %v", t.ID, err)
		return "connfail"
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	port := strconv.Itoa(addr.Port)
	log.Printf("[peer-src %s] listening on :%s, reporting to server...", t.ID, port)

	if err := reportPeerAddr(cfg, t.ID, port); err != nil {
		log.Printf("[peer-src %s] reportPeerAddr error: %v", t.ID, err)
		return "connfail"
	}
	log.Printf("[peer-src %s] address reported, waiting for target to connect (timeout 60s)...", t.ID)

	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(60 * time.Second))
	conn, err := ln.Accept()
	if err != nil {
		if atomic.LoadInt32(stopFlag) == 1 {
			return "stopped"
		}
		log.Printf("[peer-src %s] accept error (target did not connect in time): %v", t.ID, err)
		return "connfail"
	}
	defer conn.Close()
	log.Printf("[peer-src %s] target connected from %s, starting transfer", t.ID, conn.RemoteAddr())
	return runPeerBidirectional(t, conn, deadline, stopFlag, totalUp, totalDown)
}

func runPeerTarget(cfg *Config, t *Task, deadline time.Time, stopFlag *int32, totalUp, totalDown *int64) string {
	if t.PeerAddr == "" {
		log.Printf("[peer-tgt %s] no peer_addr from server", t.ID)
		return "connfail"
	}
	log.Printf("[peer-tgt %s] connecting to source at %s...", t.ID, t.PeerAddr)
	conn, err := net.DialTimeout("tcp", t.PeerAddr, 30*time.Second)
	if err != nil {
		log.Printf("[peer-tgt %s] dial error: %v", t.ID, err)
		return "connfail"
	}
	defer conn.Close()
	log.Printf("[peer-tgt %s] connected to source, starting transfer", t.ID)
	return runPeerBidirectional(t, conn, deadline, stopFlag, totalUp, totalDown)
}

// runPeerBidirectional transfers data in both directions simultaneously on conn.
func runPeerBidirectional(t *Task, conn net.Conn, deadline time.Time, stopFlag *int32, totalUp, totalDown *int64) string {
	stop := func() bool {
		return atomic.LoadInt32(stopFlag) == 1 || time.Now().After(deadline)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Upload: write data to peer
	go func() {
		defer wg.Done()
		if t.UpMbps > 0 {
			pacedUpload(conn, t.UpMbps, stop, totalUp)
		}
		// Signal EOF to the remote reader
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	// Download: read data from peer
	go func() {
		defer wg.Done()
		readCount(conn, stop, totalDown)
	}()

	wg.Wait()

	if atomic.LoadInt32(stopFlag) == 1 {
		return "stopped"
	}
	return "done"
}

type sshLoginEvent struct {
	At     time.Time
	IP     string
	User   string
	Method string
}

func (e sshLoginEvent) key() string {
	return strings.Join([]string{
		e.At.UTC().Format(time.RFC3339),
		e.IP,
		e.User,
		e.Method,
	}, "|")
}

func sshMonitorLoop(cfg *Config) {
	if runtime.GOOS != "linux" {
		return
	}
	successTicker := time.NewTicker(30 * time.Second)
	failedTicker := time.NewTicker(time.Minute)
	defer successTicker.Stop()
	defer failedTicker.Stop()

	lastSuccessScan := time.Now()
	sent := make(map[string]time.Time)

	for {
		select {
		case <-shutdownCh:
			return
		case <-failedTicker.C:
			refreshSSHFailedAttempts()
		case <-successTicker.C:
			now := time.Now()
			events, err := fetchSSHSuccessEventsSince(lastSuccessScan.Add(-2 * time.Second))
			if err != nil {
				log.Printf("[ssh] 扫描成功登录日志失败: %v", err)
				continue
			}

			allSent := true
			for _, ev := range events {
				key := ev.key()
				if _, ok := sent[key]; ok {
					continue
				}
				req := SSHLoginReq{
					ClientID:    cfg.ClientID,
					ClientToken: cfg.ClientToken,
					LoginIP:     ev.IP,
					LoginAt:     ev.At.Format(time.RFC3339),
					Username:    ev.User,
					Method:      ev.Method,
				}
				if err := postJSON(cfg.ServerURL+"/api/ssh/login", req, nil); err != nil {
					log.Printf("[ssh] 上报成功登录失败: %v", err)
					allSent = false
					continue
				}
				sent[key] = now
			}

			cutoff := now.Add(-2 * time.Hour)
			for key, seenAt := range sent {
				if seenAt.Before(cutoff) {
					delete(sent, key)
				}
			}
			if allSent {
				lastSuccessScan = now
			}
		}
	}
}

func refreshSSHFailedAttempts() {
	if runtime.GOOS != "linux" {
		return
	}
	count, err := countSSHFailedAttemptsSince(time.Now().Add(-time.Hour))
	if err != nil {
		log.Printf("[ssh] 统计最近 1 小时失败尝试失败: %v", err)
		return
	}
	atomic.StoreInt64(&sshAttemptsLastHour, int64(count))
}

func countSSHFailedAttemptsSince(since time.Time) (int, error) {
	entries, err := collectSSHLogEntriesSince(since)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if isSSHFailedAttemptMessage(entry.Message) {
			count++
		}
	}
	return count, nil
}

func fetchSSHSuccessEventsSince(since time.Time) ([]sshLoginEvent, error) {
	entries, err := collectSSHLogEntriesSince(since)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	events := make([]sshLoginEvent, 0)
	for _, entry := range entries {
		ev, ok := parseSSHSuccessEvent(entry)
		if !ok {
			continue
		}
		key := ev.key()
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		events = append(events, ev)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	return events, nil
}

func collectSSHLogEntriesSince(since time.Time) ([]sshLogEntry, error) {
	combined := make(map[string]sshLogEntry)
	var errs []string

	if entries, err := readSSHJournalEntriesSince(since); err == nil {
		for _, entry := range entries {
			combined[entry.Timestamp.UTC().Format(time.RFC3339Nano)+"|"+entry.Message] = entry
		}
	} else {
		errs = append(errs, "journalctl: "+err.Error())
	}

	if entries, err := readSSHFileEntriesSince(since); err == nil {
		for _, entry := range entries {
			combined[entry.Timestamp.UTC().Format(time.RFC3339Nano)+"|"+entry.Message] = entry
		}
	} else {
		errs = append(errs, "authlog: "+err.Error())
	}

	if len(combined) == 0 {
		if len(errs) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}

	out := make([]sshLogEntry, 0, len(combined))
	for _, entry := range combined {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

func readSSHJournalEntriesSince(since time.Time) ([]sshLogEntry, error) {
	commands := [][]string{
		{"--no-pager", "--since", since.Format(time.RFC3339), "-o", "short-iso", "SYSLOG_IDENTIFIER=sshd"},
		{"--no-pager", "--since", since.Format(time.RFC3339), "-o", "short-iso", "-t", "sshd"},
		{"--no-pager", "--since", since.Format(time.RFC3339), "-o", "short-iso", "-u", "ssh.service"},
		{"--no-pager", "--since", since.Format(time.RFC3339), "-o", "short-iso", "-u", "sshd.service"},
	}

	combined := make(map[string]sshLogEntry)
	var errs []string
	now := time.Now()
	for _, args := range commands {
		cmd := exec.Command("journalctl", args...)
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				errs = append(errs, fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(ee.Stderr))))
			} else {
				errs = append(errs, err.Error())
			}
			continue
		}
		for _, entry := range parseSSHLogOutput(string(out), since, now) {
			combined[entry.Timestamp.UTC().Format(time.RFC3339Nano)+"|"+entry.Message] = entry
		}
	}

	if len(combined) == 0 {
		if len(errs) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}

	out := make([]sshLogEntry, 0, len(combined))
	for _, entry := range combined {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

func readSSHFileEntriesSince(since time.Time) ([]sshLogEntry, error) {
	paths := []string{"/var/log/auth.log", "/var/log/secure"}
	var entries []sshLogEntry
	found := false
	now := time.Now()

	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		found = true
		entries = append(entries, parseSSHLogOutput(string(b), since, now)...)
	}

	if !found {
		return nil, fmt.Errorf("未找到 /var/log/auth.log 或 /var/log/secure")
	}
	return entries, nil
}

func parseSSHLogOutput(raw string, since, now time.Time) []sshLogEntry {
	lines := strings.Split(raw, "\n")
	entries := make([]sshLogEntry, 0, len(lines))
	for _, line := range lines {
		entry, ok := parseSSHLogLine(line, now)
		if !ok {
			continue
		}
		if entry.Timestamp.Before(since) {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

func parseSSHLogLine(line string, now time.Time) (sshLogEntry, bool) {
	line = strings.TrimSpace(line)
	if line == "" || !strings.Contains(line, "sshd") {
		return sshLogEntry{}, false
	}

	colon := strings.Index(line, ": ")
	if colon == -1 {
		return sshLogEntry{}, false
	}

	msg := strings.TrimSpace(line[colon+2:])
	if msg == "" {
		return sshLogEntry{}, false
	}

	ts, ok := parseSSHLogTimestamp(line, now)
	if !ok {
		return sshLogEntry{}, false
	}
	return sshLogEntry{Timestamp: ts, Message: msg}, true
}

func parseSSHLogTimestamp(line string, now time.Time) (time.Time, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return time.Time{}, false
	}

	journalLayouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05-0700",
		"2006-01-02T15:04:05.000000-0700",
		"2006-01-02T15:04:05.000000000-0700",
	}
	for _, layout := range journalLayouts {
		if ts, err := time.Parse(layout, fields[0]); err == nil {
			return ts, true
		}
	}

	if len(line) < 15 {
		return time.Time{}, false
	}
	ts, err := time.ParseInLocation("Jan _2 15:04:05", line[:15], now.Location())
	if err != nil {
		return time.Time{}, false
	}
	ts = time.Date(now.Year(), ts.Month(), ts.Day(), ts.Hour(), ts.Minute(), ts.Second(), 0, now.Location())
	if ts.After(now.Add(24 * time.Hour)) {
		ts = ts.AddDate(-1, 0, 0)
	}
	return ts, true
}

func isSSHFailedAttemptMessage(msg string) bool {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "failed password for"):
		return true
	case strings.Contains(lower, "failed publickey for"):
		return true
	case strings.Contains(lower, "failed none for"):
		return true
	case strings.Contains(lower, "failed keyboard-interactive"):
		return true
	case strings.Contains(lower, "authentication failure"):
		return true
	case strings.Contains(lower, "maximum authentication attempts exceeded"):
		return true
	default:
		return false
	}
}

func parseSSHSuccessEvent(entry sshLogEntry) (sshLoginEvent, bool) {
	msg := entry.Message
	if !strings.HasPrefix(msg, "Accepted ") || !strings.Contains(msg, " from ") {
		return sshLoginEvent{}, false
	}

	afterAccepted := strings.TrimPrefix(msg, "Accepted ")
	methodSep := strings.Index(afterAccepted, " for ")
	if methodSep == -1 {
		return sshLoginEvent{}, false
	}
	method := strings.TrimSpace(afterAccepted[:methodSep])
	rest := afterAccepted[methodSep+5:]

	fromSep := strings.Index(rest, " from ")
	if fromSep == -1 {
		return sshLoginEvent{}, false
	}
	user := strings.TrimSpace(rest[:fromSep])
	ipFields := strings.Fields(rest[fromSep+6:])
	if len(ipFields) == 0 {
		return sshLoginEvent{}, false
	}

	return sshLoginEvent{
		At:     entry.Timestamp,
		IP:     ipFields[0],
		User:   user,
		Method: method,
	}, true
}

func postJSON(url string, v any, out any) error {
	b, _ := json.Marshal(v)
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func token(n int) string {
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

func hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		return "bwagent"
	}
	return h
}

// selfUpgrade 安全地将自身升级到指定版本。
// 流程：下载临时文件 → SHA256 完整性校验 → 原子替换旧二进制 → 重启服务
func selfUpgrade(version string) {
	// 确定下载架构
	arch := runtime.GOARCH // "amd64" / "arm64"
	if arch != "amd64" && arch != "arm64" {
		log.Printf("[upgrade] 不支持的架构 %s，跳过升级", arch)
		return
	}

	var binaryName string
	if runtime.GOOS == "windows" {
		binaryName = fmt.Sprintf("bwagent-windows-%s.exe", arch)
	} else {
		binaryName = fmt.Sprintf("bwagent-%s-%s", runtime.GOOS, arch)
	}
	var dlURL, sha256SumsURL string
	if version == "latest" {
		dlURL = fmt.Sprintf("https://github.com/ctsunny/bwtest/releases/latest/download/%s", binaryName)
		sha256SumsURL = "https://github.com/ctsunny/bwtest/releases/latest/download/SHA256SUMS"
	} else {
		dlURL = fmt.Sprintf("https://github.com/ctsunny/bwtest/releases/download/%s/%s", version, binaryName)
		sha256SumsURL = fmt.Sprintf("https://github.com/ctsunny/bwtest/releases/download/%s/SHA256SUMS", version)
	}
	log.Printf("[upgrade] 开始下载 %s", dlURL)

	dlClient := &http.Client{Timeout: 5 * time.Minute}
	resp, err := dlClient.Get(dlURL)
	if err != nil {
		log.Printf("[upgrade] 下载失败: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("[upgrade] 下载失败: HTTP %d", resp.StatusCode)
		return
	}

	// 获取当前二进制路径
	exePath, err := os.Executable()
	if err != nil {
		log.Printf("[upgrade] 无法获取当前二进制路径: %v", err)
		return
	}

	// 写入临时文件 (在同一目录下，以防跨文件系统导致 rename 失败)
	tmpPath := exePath + ".new"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		log.Printf("[upgrade] 创建临时文件失败: %v", err)
		return
	}
	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil || n == 0 {
		log.Printf("[upgrade] 写入临时文件失败: %v (n=%d)", err, n)
		_ = os.Remove(tmpPath)
		return
	}
	log.Printf("[upgrade] 下载完成，文件大小 %d 字节", n)

	// SHA256 完整性校验：防止下载被篡改或传输损坏。
	// 如果此版本没有 SHA256SUMS 文件（例如旧版本发布），校验会被跳过并打印警告。
	if err := verifySHA256(tmpPath, sha256SumsURL, binaryName, dlClient); err != nil {
		log.Printf("[upgrade] SHA256 校验失败: %v，中止升级", err)
		_ = os.Remove(tmpPath)
		return
	}

	// 原子替换：先备份，再 rename
	backupPath := exePath + ".bak"
	_ = os.Remove(backupPath)
	if err := os.Rename(exePath, backupPath); err != nil {
		log.Printf("[upgrade] 备份旧二进制失败: %v", err)
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, exePath); err != nil {
		log.Printf("[upgrade] 替换二进制失败: %v，尝试还原...", err)
		_ = os.Rename(backupPath, exePath)
		_ = os.Remove(tmpPath)
		return
	}
	_ = os.Remove(backupPath)
	log.Printf("[upgrade] 二进制替换成功，即将退出由服务管理器以新版本重启...")

	// 延迟 1 秒确保当前心跳响应已处理完毕
	time.Sleep(time.Second)
	restartAgentService("upgrade")
}

// verifySHA256 downloads SHA256SUMS from sumsURL and verifies that the file at
// filePath matches the expected hash for binaryName.
// Returns nil if the SHA256SUMS file is not found (HTTP 404), enabling graceful
// fallback for releases that predate SHA256SUMS publishing.
func verifySHA256(filePath, sumsURL, binaryName string, client *http.Client) error {
	resp, err := client.Get(sumsURL)
	if err != nil {
		return fmt.Errorf("下载 SHA256SUMS 失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// SHA256SUMS 不存在（旧版本发布），跳过校验但记录警告。
		log.Printf("[upgrade] ⚠️  SHA256SUMS 不存在于此版本发布，跳过完整性校验")
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 SHA256SUMS 返回 HTTP %d", resp.StatusCode)
	}

	// 在 SHA256SUMS 中找到匹配 binaryName 的行
	// 格式：<hash>  <filename>  或  <hash> *<filename>
	var expectedHash string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			// 去掉 BSD 风格的 * 前缀
			name := strings.TrimPrefix(fields[1], "*")
			if name == binaryName {
				expectedHash = strings.ToLower(fields[0])
				break
			}
		}
	}
	if expectedHash == "" {
		return fmt.Errorf("SHA256SUMS 中未找到 %q 的校验值", binaryName)
	}

	// 计算已下载文件的 SHA256
	fp, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("打开文件失败: %v", err)
	}
	defer fp.Close()

	h := sha256.New()
	if _, err := io.Copy(h, fp); err != nil {
		return fmt.Errorf("计算 SHA256 失败: %v", err)
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	if actualHash != expectedHash {
		return fmt.Errorf("SHA256 不匹配: 期望 %s，实际 %s", expectedHash, actualHash)
	}
	log.Printf("[upgrade] ✓ SHA256 校验通过: %s", actualHash)
	return nil
}
