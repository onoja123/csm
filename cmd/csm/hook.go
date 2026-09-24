package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

// Only status-line may print (session-start stdout enters the agent's context); nothing here may fail the agent.
func runHook(event string, stdin io.Reader) {
	stateDir := os.Getenv("CSM_STATE_DIR")
	pid, _ := strconv.Atoi(os.Getenv("CSM_PID"))
	if stateDir == "" || pid <= 0 {
		return
	}
	data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return
	}

	var p provider.Claude
	switch event {
	case "status-line":
		runStatusLine(p, data)
	case "session-start":
		id, err := p.ParseSessionStart(data)
		if err == nil && id != "" {
			writeJSON(filepath.Join(stateDir, providerSessionFile), providerSession{SessionID: id})
		}
	case "failure":
		f, err := p.ParseFailure(data)
		if err != nil {
			return
		}
		if writeJSON(filepath.Join(stateDir, eventFile), f) == nil {
			syscall.Kill(pid, syscall.SIGUSR1)
		}
	}
}

func runStatusLine(p provider.Claude, data []byte) {
	account, home := os.Getenv("CSM_ACCOUNT"), os.Getenv("CSM_HOME")
	now := time.Now()
	usage, err := p.ParseUsage(data)
	if err == nil && !usage.Empty() && account != "" && home != "" {
		s := &State{Home: home}
		s.saveUsage(UsageRecord{Version: stateVersion, Account: account, UpdatedAt: now, Usage: usage})
	}

	if chain := os.Getenv("CSM_STATUS_LINE_CHAIN"); chain != "" {
		cmd := exec.Command("/bin/sh", "-c", chain)
		cmd.Stdin = bytes.NewReader(data)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Run()
		return
	}
	line := "csm · " + account
	if summary := shortUsage(usage, now); summary != "" {
		line += " · " + summary
	}
	fmt.Println(line)
}
