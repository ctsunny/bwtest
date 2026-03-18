package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var Version = "v0.4.14"

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
}

type Task struct {
	ID          string `json:"id"`
	Mode        string `json:"mode"`
	UpMbps      int    `json:"up_mbps"`
	DownMbps    int    `json:"down_mbps"`
	DurationSec int    `json:"duration_sec"`
	DataAddr    string `json:"data_addr"`
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

type DataHello struct {
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
	TaskID      string `json:"task_id"`
	Mode        string `json:"mode"`
	DurationSec int    `json:"duration_sec"`
}

var busy int32
var httpClient = &http.Client{Timeout: 15 * time.Second}

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
	cfgPath := "/etc/bwagent/config.json"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := loadOrCreateConfig(cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	log.SetOutput(io.MultiWriter(os.Stdout, logBuf))
	log.Printf("bwagent %s starting...", Version)

	// register with retry
	for {
		if err := register(cfg); err != nil {
			log.Printf("register error: %v, retrying in 10s...", err)
			time.Sleep(10 * time.Second)
			continue
		}
		break
	}

	go heartbeatLoop(cfg)
	pollLoop(cfg)
}

func loadOrCreateConfig(path string) (*Config, error) {
	if b, err := os.ReadFile(path); err == nil {
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

	_ = os.MkdirAll("/etc/bwagent", 0755)
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
			OK        bool   `json:"ok"`
			UpgradeTo string `json:"upgrade_to"`
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
	}
}

func pollLoop(cfg *Config) {
	tk := time.NewTicker(5 * time.Second)
	defer tk.Stop()

	for range tk.C {
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
				// 只有连续失败3次（约6-10秒）才认为任务被外部终止，增加对 transient API 错误的鲁棒性
				if failCount >= 3 {
					log.Printf("[task %s] control check error (consecutive %d): %v, stopping", t.ID, failCount, err)
					atomic.StoreInt32(&stopFlag, 1)
					return
				}
				log.Printf("[task %s] control check transient error: %v, retrying...", t.ID, err)
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

	for {
		if time.Now().After(deadline) {
			return totalUp, totalDown, "done"
		}
		if atomic.LoadInt32(&stopFlag) == 1 {
			return totalUp, totalDown, "stopped"
		}

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

// runTaskOnce attempts a single connection and runs until done/stopped/error.
// totalUp/totalDown are the running accumulators (passed in, updated in place).
func runTaskOnce(cfg *Config, t *Task, deadline time.Time, stopFlag *int32, totalUp, totalDown *int64) (int64, int64, string) {
	conn, err := net.DialTimeout("tcp", t.DataAddr, 10*time.Second)
	if err != nil {
		log.Printf("dial error: %v", err)
		return *totalUp, *totalDown, "connfail"
	}
	defer conn.Close()

	hello, _ := json.Marshal(DataHello{
		ClientID:    cfg.ClientID,
		ClientToken: cfg.ClientToken,
		TaskID:      t.ID,
		Mode:        t.Mode,
		DurationSec: t.DurationSec,
	})
	if _, err := conn.Write(append(hello, '\n')); err != nil {
		return *totalUp, *totalDown, "connfail"
	}

	shouldStop := func() bool {
		return time.Now().After(deadline) || atomic.LoadInt32(stopFlag) == 1
	}

	switch t.Mode {
	case "upload":
		up := pacedUpload(conn, t.UpMbps, shouldStop, totalUp)
		_ = up
		if atomic.LoadInt32(stopFlag) == 1 {
			return *totalUp, *totalDown, "stopped"
		}
		if time.Now().After(deadline) {
			return *totalUp, *totalDown, "done"
		}
		return *totalUp, *totalDown, "connfail"

	case "download":
		down := readCount(conn, shouldStop, totalDown)
		_ = down
		if atomic.LoadInt32(stopFlag) == 1 {
			return *totalUp, *totalDown, "stopped"
		}
		if time.Now().After(deadline) {
			return *totalUp, *totalDown, "done"
		}
		return *totalUp, *totalDown, "connfail"

	case "both":
		conn2, err2 := net.DialTimeout("tcp", t.DataAddr, 10*time.Second)
		if err2 != nil {
			log.Printf("dial2 error: %v", err2)
			up := pacedUpload(conn, t.UpMbps, shouldStop, totalUp)
			_ = up
			if atomic.LoadInt32(stopFlag) == 1 {
				return *totalUp, *totalDown, "stopped"
			}
			if time.Now().After(deadline) {
				return *totalUp, *totalDown, "done"
			}
			return *totalUp, *totalDown, "connfail"
		}
		defer conn2.Close()
		hello2, _ := json.Marshal(DataHello{
			ClientID:    cfg.ClientID,
			ClientToken: cfg.ClientToken,
			TaskID:      t.ID,
			Mode:        "download",
			DurationSec: t.DurationSec,
		})
		if _, err := conn2.Write(append(hello2, '\n')); err != nil {
			return *totalUp, *totalDown, "connfail"
		}
		done := make(chan struct{})
		go func() {
			readCount(conn2, shouldStop, totalDown)
			close(done)
		}()
		pacedUpload(conn, t.UpMbps, shouldStop, totalUp)
		<-done
		if atomic.LoadInt32(stopFlag) == 1 {
			return *totalUp, *totalDown, "stopped"
		}
		if time.Now().After(deadline) {
			return *totalUp, *totalDown, "done"
		}
		return *totalUp, *totalDown, "connfail"
	default:
		return *totalUp, *totalDown, "failed"
	}
}

func pacedUpload(w io.Writer, mbps int, stop func() bool, counter *int64) int64 {
	if mbps <= 0 {
		return 0
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
	var total int64
	for !stop() {
		left := perTick
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
		<-tk.C
	}
	return total
}

func readCount(conn net.Conn, stop func() bool, counter *int64) int64 {
	buf := make([]byte, 64*1024)
	var total int64
	for !stop() {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
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
// 流程：下载临时文件 → 验证 → 原子替换旧二进制 → 重启服务
func selfUpgrade(version string) {
	// 确定下载架构
	arch := runtime.GOARCH // "amd64" / "arm64"
	if arch != "amd64" && arch != "arm64" {
		log.Printf("[upgrade] 不支持的架构 %s，跳过升级", arch)
		return
	}

	dlURL := fmt.Sprintf(
		"https://github.com/ctsunny/bwtest/releases/download/%s/bwagent-linux-%s",
		version, arch,
	)
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
	log.Printf("[upgrade] 二进制替换成功，即将退出由 systemd 以新版本重启...")

	// 延迟 1 秒确保当前心跳响应已处理完毕
	time.Sleep(time.Second)

	// 优先尝试 sudo systemctl restart（需要 sudoers 配置）
	// 如果无权限则直接 Exit(0) — systemd Restart=always 会自动重新拉起
	if err := exec.Command("sudo", "-n", "systemctl", "restart", "bwagent").Run(); err != nil {
		log.Printf("[upgrade] sudo systemctl restart 不可用(%v)，通过 os.Exit(0) 触发 systemd respawn", err)
	}
	os.Exit(0)
}

