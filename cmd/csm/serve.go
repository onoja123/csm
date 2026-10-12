package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var _ = json.Marshal

const (
	socketFile      = "csm.sock"
	servePIDFile    = "serve.pid"
	serveLogFile    = "serve.log"
	serveInfoFile   = "serve.json"
	remoteTokenFile = "remote-token"
)

// serveInfo is what a running service wrote about itself, so `csm serve pair` can describe it.
type serveInfo struct {
	PID       int       `json:"pid"`
	Socket    string    `json:"socket"`
	Listen    string    `json:"listen,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

func socketPath(home string) string { return filepath.Join(home, socketFile) }

func serveUsage() error {
	return errors.New("usage: csm serve [--detach] [--listen HOST:PORT] | csm serve stop | csm serve status | csm serve pair [--rotate]")
}

// cmdServe runs the local service the desktop app talks to. It always listens on a Unix socket inside ~/.csm; with --listen it also accepts
// token-authenticated connections from the local network, which is how the mobile app reaches it.
func cmdServe(log *Logger, home string, args []string) error {
	detach, rotate := false, false
	sub, listen := "", ""

	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--detach":
			detach = true
		case a == "--rotate":
			rotate = true
		case a == "--listen" && i+1 < len(args):
			i++
			listen = args[i]
		case strings.HasPrefix(a, "--listen="):
			listen = strings.TrimPrefix(a, "--listen=")
		case a == "stop" || a == "status" || a == "pair":
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
	case "pair":
		return servePair(log, home, rotate)
	}

	if detach {
		return serveDetach(log, home, listen)
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

	handler := d.handler()
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	defer stop()

	go d.watch(ctx)

	d.builds.start()

	errs := make(chan error, 2)
	go func() { errs <- srv.Serve(ln) }()

	log.Info("csm service listening on %s", displayPath(socketPath(home)))
	info := serveInfo{PID: os.Getpid(), Socket: socketPath(home), StartedAt: time.Now()}

	var remote *http.Server

	if listen != "" {
		token, err := remoteToken(home, false)
		if err != nil {
			return err
		}

		tcp, err := net.Listen("tcp", listen)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", listen, err)
		}

		defer tcp.Close()

		remote = &http.Server{Handler: requireToken(token, handler), ReadHeaderTimeout: 10 * time.Second}
		go func() { errs <- remote.Serve(tcp) }()

		info.Listen = tcp.Addr().String()
		log.Info("csm service also listening on %s (token required; pair a device with: csm serve pair)", info.Listen)
	}

	writeJSON(filepath.Join(home, serveInfoFile), info)

	defer os.Remove(filepath.Join(home, serveInfoFile))

	select {
	case <-ctx.Done():
	case err := <-errs:
		return err
	}

	log.Info("csm service stopping")
	d.builds.close()
	d.runs.stopAll(stopGracePeriod)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	defer cancel()

	if remote != nil {
		remote.Shutdown(shutdownCtx)
	}

	return srv.Shutdown(shutdownCtx)
}

// requireToken guards the network listener: every request carries the pairing token, compared in constant time. The hook endpoint stays socket-only.
func requireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/hooks/") {
			writeErr(w, http.StatusForbidden, errors.New("hooks are accepted on the local socket only"))

			return
		}

		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, errors.New("a valid pairing token is required; run: csm serve pair"))

			return
		}

		next.ServeHTTP(w, r)
	})
}

func remoteToken(home string, rotate bool) (string, error) {
	path := filepath.Join(home, remoteTokenFile)

	if !rotate {
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) >= 32 {
			return strings.TrimSpace(string(data)), nil
		}
	}

	var b [24]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	token := hex.EncodeToString(b[:])

	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}

	return token, nil
}

func lanAddresses() []string {
	var out []string
	ifaces, _ := net.Interfaces()

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, _ := iface.Addrs()

		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsPrivate() {
				out = append(out, ipn.IP.String())
			}
		}
	}

	return out
}

// servePair prints what a phone needs: where the service listens and the token. The token is a secret; it is shown here on purpose and nowhere else.
func servePair(log *Logger, home string, rotate bool) error {
	token, err := remoteToken(home, rotate)
	if err != nil {
		return err
	}

	var info serveInfo
	running := readJSON(filepath.Join(home, serveInfoFile), &info) == nil && processAlive(info.PID)

	log.Info("CSM mobile pairing")
	log.Blank()

	switch {
	case !running:
		log.Info("The service is not running. Start it with network access:\n\n    csm serve --detach --listen 0.0.0.0:7345")
	case info.Listen == "":
		log.Info("The service is running on the local socket only. Restart it with network access:\n\n    csm serve stop && csm serve --detach --listen 0.0.0.0:7345")
	default:
		_, port, _ := net.SplitHostPort(info.Listen)
		addrs := lanAddresses()

		if len(addrs) == 0 {
			addrs = []string{"<this machine's address>"}
		}

		for _, a := range addrs {
			log.Row("  Server", "http://"+net.JoinHostPort(a, port))
		}
	}

	log.Row("  Token", token)
	log.Blank()
	log.Info("Enter the server and token in CSM Mobile. Anyone with the token controls your agents; rotate it with: csm serve pair --rotate")

	return nil
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

func serveDetach(log *Logger, home, listen string) error {
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

	args := []string{"serve"}

	if listen != "" {
		args = append(args, "--listen", listen)
	}

	cmd := exec.Command(self, args...)
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

	var info serveInfo

	if readJSON(filepath.Join(home, serveInfoFile), &info) == nil && info.Listen != "" {
		log.Info("network: %s (token required)", info.Listen)
	}

	return nil
}
