//go:build windows

package main

import (
	"log"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/windows/svc"
)

type bwagentService struct {
	done <-chan struct{}
}

func (s *bwagentService) Execute(_ []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				shutdown()
				// Wait for agent to finish (up to 10 s)
				select {
				case <-s.done:
				case <-time.After(10 * time.Second):
				}
				return false, 0
			}
		case <-s.done:
			return false, 0
		}
	}
}

// maybeRunAsWindowsService detects whether the process is running as a Windows
// service. If so, it starts runFn in a goroutine, hands control to the SCM,
// and returns true after the service stops. Returns false when running
// interactively so that the caller can proceed with a normal startup.
func maybeRunAsWindowsService(runFn func()) bool {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFn()
	}()
	if err := svc.Run("bwagent", &bwagentService{done: done}); err != nil {
		log.Printf("Windows service run error: %v", err)
	}
	return true
}

func restartAgentService(reason string) {
	log.Printf("[%s] 即将重启 bwagent 服务...", reason)
	time.Sleep(time.Second)
	go func() {
		_ = exec.Command("sc", "stop", "bwagent").Run()
		time.Sleep(2 * time.Second)
		_ = exec.Command("sc", "start", "bwagent").Run()
	}()
	os.Exit(0)
}
