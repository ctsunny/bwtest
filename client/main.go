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
	"sync/atomic"
	"time"
)

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
}

type HeartbeatReq struct {
	ClientID    string `json:"client_id"`
	ClientToken string `json:"client_token"`
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

func main() {
	cfgPath := "/etc/bwagent/config.json"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := loadOrCreateConfig(cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	if err := register(cfg); err != nil {
		log.Fatal(err)
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
		// persist generated ids if needed
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
	return postJSON(cfg.ServerURL+"/api/register", RegisterReq{
		ClientID:    cfg.ClientID,
		ClientToken: cfg.ClientToken,
		Name:        cfg.Name,
		InitToken:   cfg.InitToken,
	}, nil)
}

func heartbeatLoop(cfg *Config) {
	tk := time.NewTicker(20 * time.Second)
	defer tk.Stop()
	for range tk.C {
		_ = postJSON(cfg.ServerURL+"/api/heartbeat", HeartbeatReq{
			ClientID:    cfg.ClientID,
			ClientToken: cfg.ClientToken,
		}, nil)
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
			up, down, status := runTask(cfg, t)
			_ = postJSON(cfg.ServerURL+"/api/task/result", ResultReq{
				ClientID:      cfg.ClientID,
				ClientToken:   cfg.ClientToken,
				TaskID:        t.ID,
				Status:        status,
				UploadBytes:   up,
				DownloadBytes: down,
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

func runTask(cfg *Config, t *Task) (int64, int64, string) {
	conn, err := net.DialTimeout("tcp", t.DataAddr, 5*time.Second)
	if err != nil {
		log.Printf("dial error: %v", err)
		return 0, 0, "failed"
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
		return 0, 0, "failed"
	}

	deadline := time.Now().Add(time.Duration(t.DurationSec) * time.Second)
	var stopFlag int32

	go func() {
		tk := time.NewTicker(2 * time.Second)
		defer tk.Stop()
		for range tk.C {
			status, err := taskControl(cfg, t.ID)
			if err == nil && status == "stopping" {
				atomic.StoreInt32(&stopFlag, 1)
				return
			}
			if atomic.LoadInt32(&busy) == 0 {
				return
			}
		}
	}()

	shouldStop := func() bool {
		return time.Now().After(deadline) || atomic.LoadInt32(&stopFlag) == 1
	}

	switch t.Mode {
	case "upload":
		up := pacedUpload(conn, t.UpMbps, shouldStop)
		if atomic.LoadInt32(&stopFlag) == 1 {
			return up, 0, "stopped"
		}
		return up, 0, "done"
	case "download":
		down := readCount(conn, shouldStop)
		if atomic.LoadInt32(&stopFlag) == 1 {
			return 0, down, "stopped"
		}
		return 0, down, "done"
	case "both":
		var down int64
		done := make(chan struct{})
		go func() {
			down = readCount(conn, shouldStop)
			close(done)
		}()
		up := pacedUpload(conn, t.UpMbps, shouldStop)
		<-done
		if atomic.LoadInt32(&stopFlag) == 1 {
			return up, down, "stopped"
		}
		return up, down, "done"
	default:
		return 0, 0, "failed"
	}
}

func pacedUpload(w io.Writer, mbps int, stop func() bool) int64 {
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
			left -= int64(wr)
		}
		<-tk.C
	}
	return total
}

func readCount(conn net.Conn, stop func() bool) int64 {
	buf := make([]byte, 64*1024)
	var total int64
	for !stop() {
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, err := conn.Read(buf)
		if n > 0 {
			total += int64(n)
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			continue
		}
		if err != nil {
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
