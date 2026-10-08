package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	socketFile   = "csm.sock"
	servePIDFile = "serve.pid"
	serveLogFile = "serve.log"
)

func socketPath(home string) string { return filepath.Join(home, socketFile) }

func serveUsage() error {
	return errors.New("usage: csm serve [--detach] | csm serve stop | csm serve status")
}

// cmdServe runs the local service the desktop app talks to. It only ever listens on a Unix socket inside ~/.csm.
func cmdServe(log *Logger, home string, args []string) error {
	detach := false
	sub := ""

	for _, a := range args {
		switch a {
		case "--detach":
			detach = true
		case "stop", "status":
			sub = a
		default:
			return serveUsage()
		}
	}

	switch sub {
	case "stop":
		return serveStop(log, home)
	case "status":
		return serveStatus(log, home)
	}

	if detach {
		return serveDetach(log, home)
	}

	if _, err := initState(home); err != nil {
		return err
	}

	csmPath, err := os.Executable()
	if err != nil {
		return err
	}

	d, err := newDaemon(home, csmPath, version)
	if err != nil {
		return err
	}

	ln, err := listenSocket(home)
	if err != nil {
		return err
	}

	defer ln.Close()
	defer os.Remove(socketPath(home))

	pidPath := filepath.Join(home, servePIDFile)

	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return err
	}

	defer os.Remove(pidPath)

	srv := &http.Server{Handler: d.handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	defer stop()

	go d.watch(ctx)

	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(ln) }()

	log.Info("csm service listening on %s", displayPath(socketPath(home)))

	select {
	case <-ctx.Done():
	case err := <-errs:
		return err
	}

	log.Info("csm service stopping")
	d.runs.stopAll(stopGracePeriod)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	defer cancel()

	return srv.Shutdown(shutdownCtx)
}

func listenSocket(home string) (net.Listener, error) {
	path := socketPath(home)

	if len(path) > 100 {
		return nil, fmt.Errorf("the socket path %s is too long for this system; set CSM_HOME to a shorter path", path)
	}

	if dialable(path) {
		return nil, fmt.Errorf("a csm service is already listening on %s", displayPath(path))
	}

	os.Remove(path)
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)

	if err != nil {
		return nil, err
	}

	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()

		return nil, err
	}

	return ln, nil
}

func dialable(path string) bool {
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}

	conn.Close()

	return true
}

func serveDetach(log *Logger, home string) error {
	if dialable(socketPath(home)) {
		log.Info("csm service already running on %s", displayPath(socketPath(home)))

		return nil
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}

	logFile, err := os.OpenFile(filepath.Join(home, serveLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}

	defer logFile.Close()

	cmd := exec.Command(self, "serve")
	cmd.Env = append(os.Environ(), "CSM_HOME="+home)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return err
	}

	pid := cmd.Process.Pid
	cmd.Process.Release()

	for i := 0; i < 50; i++ {
		if dialable(socketPath(home)) {
			log.Info("csm service started (pid %d) on %s", pid, displayPath(socketPath(home)))

			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("the csm service did not start; see %s", displayPath(filepath.Join(home, serveLogFile)))
}

func servePID(home string) (int, error) {
	data, err := os.ReadFile(filepath.Join(home, servePIDFile))
	if err != nil {
		return 0, err
	}

	pid, err := strconv.Atoi(string(trimNewline(data)))
	if err != nil || !processAlive(pid) {
		return 0, errors.New("no csm service is running")
	}

	return pid, nil
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}

	return b
}

func serveStop(log *Logger, home string) error {
	pid, err := servePID(home)
	if err != nil {
		return err
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}

	for i := 0; i < 100; i++ {
		if !processAlive(pid) {
			log.Info("csm service stopped")

			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return errors.New("the csm service did not stop within 10s")
}

func serveStatus(log *Logger, home string) error {
	if !dialable(socketPath(home)) {
		return errors.New("csm service: not running")
	}

	pid, _ := servePID(home)
	log.Info("csm service: running (pid %d) on %s", pid, displayPath(socketPath(home)))

	return nil
}
