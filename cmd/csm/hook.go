package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/onoja123/csm/internal/provider"
)

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
		runStatusLine(data)

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

	case "pre-tool-use":
		runPreToolUse(stateDir, pid, data)
	}
}

// runPreToolUse asks the csm service whether the desktop wants to decide this tool call. Without a service, or when it declines, the agent asks the user itself.
func runPreToolUse(stateDir string, pid int, data []byte) {
	socket := os.Getenv("CSM_SOCKET")
	if socket == "" {
		return
	}

	body, err := json.Marshal(hookRequest{CSMPID: pid, Account: os.Getenv("CSM_ACCOUNT"), StateDir: stateDir, Payload: json.RawMessage(data)})
	if err != nil {
		return
	}

	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	resp, err := client.Post("http://csm/v1/hooks/pre-tool-use", "application/json", bytes.NewReader(body))

	if err != nil {
		return
	}

	defer resp.Body.Close()

	var out struct {
		Decision string `json:"decision"`
	}

	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return
	}

	if reply := hookResponse(out.Decision); reply != nil {
		os.Stdout.Write(reply)
	}
}

func runStatusLine(data []byte) {
	account, home := os.Getenv("CSM_ACCOUNT"), os.Getenv("CSM_HOME")
	now := time.Now()
	usage, err := provider.Claude{}.ParseUsage(data)
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
