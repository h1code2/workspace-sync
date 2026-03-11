package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"workspace-sync/internal/syncer"
)

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func defaultPidFile() string {
	baseDir, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(baseDir) == "" {
		baseDir = os.TempDir()
	}
	return filepath.Join(baseDir, "workspace-sync", "workspace-sync.pid")
}

func defaultLogFile() string {
	baseDir, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(baseDir) == "" {
		baseDir = os.TempDir()
	}
	return filepath.Join(baseDir, "workspace-sync", "workspace-sync.log")
}

func defaultLogMaxBytes() int64 {
	if v := strings.TrimSpace(os.Getenv("WORKSPACE_SYNC_LOG_MAX_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil && n > 0 {
			return n
		}
	}
	return 10 * 1024 * 1024 // 10MB
}

func trimLogFile(path string, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if st.Size() <= maxBytes {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	start := st.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, maxBytes)
	n, err := f.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return err
	}

	wf, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer wf.Close()

	if n <= 0 {
		return nil
	}
	_, err = wf.Write(buf[:n])
	return err
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func readPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err != nil {
		return 0, err
	}
	return pid, nil
}

func writePID(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", pid)), 0o600)
}

func doStatus(pidFile string) int {
	pid, err := readPID(pidFile)
	if err != nil {
		fmt.Printf("status: stopped (pid file not found: %s)\n", pidFile)
		return 1
	}
	if isProcessRunning(pid) {
		fmt.Printf("status: running (pid=%d)\n", pid)
		return 0
	}
	fmt.Printf("status: stopped (stale pid=%d)\n", pid)
	return 1
}

func doStop(pidFile string) int {
	pid, err := readPID(pidFile)
	if err != nil {
		fmt.Printf("stop: no running service (missing pid file: %s)\n", pidFile)
		return 1
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		fmt.Printf("stop: invalid pid %d\n", pid)
		return 1
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		fmt.Printf("stop: failed to signal pid %d: %v\n", pid, err)
		return 1
	}
	_ = os.Remove(pidFile)
	fmt.Printf("stop: signal sent to pid %d\n", pid)
	return 0
}

func isProcessRunningFromPIDFile(pidFile string) bool {
	pid, err := readPID(pidFile)
	if err != nil {
		return false
	}
	return isProcessRunning(pid)
}

func startBackground(pidFile, logFile string, logMaxBytes int64, childArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	_ = trimLogFile(logFile, logMaxBytes)
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	args := append([]string{}, childArgs...)
	args = append(args,
		"--log-file="+logFile,
		"--log-max-bytes="+strconv.FormatInt(logMaxBytes, 10),
		"--background-child=true",
	)
	cmd := exec.Command(exe, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return err
	}
	if err := writePID(pidFile, cmd.Process.Pid); err != nil {
		_ = f.Close()
		return err
	}
	_ = f.Close()
	fmt.Printf("workspace-sync started in background, pid=%d\n", cmd.Process.Pid)
	fmt.Printf("pid file: %s\n", pidFile)
	fmt.Printf("log file: %s (max %d bytes)\n", logFile, logMaxBytes)
	return nil
}

func normalizeListen(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ":7070"
	}
	if strings.HasPrefix(v, ":") {
		return v
	}
	if _, _, err := net.SplitHostPort(v); err == nil {
		return v
	}
	return ":" + v
}

func normalizePeer(peer, listen string) string {
	peer = strings.TrimSpace(peer)
	if peer == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(peer); err == nil {
		return peer
	}
	listen = normalizeListen(listen)
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return peer
	}
	return net.JoinHostPort(peer, port)
}

func buildRunArgs(mode, dir, listen, peer, token string, debounce, resync time.Duration, excludes []string, pidFile string, chunkSize int, ackTimeout time.Duration, maxRetries int, sendWorkers int, metricsInterval time.Duration, pingInterval time.Duration, enableResume bool, partialTTL time.Duration, conflictPolicy string) []string {
	args := []string{
		"--mode=" + mode,
		"--dir=" + dir,
		"--listen=" + normalizeListen(listen),
		"--peer=" + normalizePeer(peer, listen),
		"--token=" + token,
		"--debounce=" + debounce.String(),
		"--resync=" + resync.String(),
		"--pid-file=" + pidFile,
		"--chunk-size=" + strconv.Itoa(chunkSize),
		"--ack-timeout=" + ackTimeout.String(),
		"--max-retries=" + strconv.Itoa(maxRetries),
		"--send-workers=" + strconv.Itoa(sendWorkers),
		"--metrics-interval=" + metricsInterval.String(),
		"--ping-interval=" + pingInterval.String(),
		"--enable-resume=" + strconv.FormatBool(enableResume),
		"--partial-ttl=" + partialTTL.String(),
		"--conflict-policy=" + conflictPolicy,
	}
	for _, ex := range excludes {
		args = append(args, "--exclude="+ex)
	}
	return args
}

func reorderArgsForSubcommand(args []string) []string {
	if len(args) < 2 {
		return args
	}
	sub := strings.ToLower(strings.TrimSpace(args[1]))
	switch sub {
	case "start", "status", "stop":
		out := make([]string, 0, len(args))
		out = append(out, args[0])
		out = append(out, args[2:]...)
		out = append(out, sub)
		return out
	default:
		return args
	}
}

func main() {
	os.Args = reorderArgsForSubcommand(os.Args)
	var mode string
	var dir string
	var listen string
	var peer string
	var token string
	var debounce time.Duration
	var resync time.Duration
	var excludes multiFlag
	var pidFile string
	var logFile string
	var logMaxBytes int64
	var shortReceive bool
	var shortSend bool
	var backgroundChild bool
	var chunkSize int
	var ackTimeout time.Duration
	var maxRetries int
	var sendWorkers int
	var metricsInterval time.Duration
	var enableResume bool
	var partialTTL time.Duration
	var conflictPolicy string
	var pingInterval time.Duration

	flag.StringVar(&mode, "mode", "", "send | receive | both")
	flag.StringVar(&dir, "dir", ".", "Directory to watch/sync")
	flag.StringVar(&dir, "d", ".", "short for --dir")
	flag.StringVar(&listen, "listen", ":7070", "Listen addr for receive mode")
	flag.StringVar(&listen, "l", ":7070", "short for --listen")
	flag.StringVar(&peer, "peer", "", "Peer addr host:port for send mode")
	flag.StringVar(&token, "token", "", "Shared token")
	flag.StringVar(&token, "t", "", "short for --token")
	flag.DurationVar(&debounce, "debounce", 400*time.Millisecond, "Debounce window for fs events")
	flag.DurationVar(&resync, "resync", 60*time.Second, "Periodic full resync interval (0 to disable)")
	flag.Var(&excludes, "exclude", "Relative glob to exclude (repeatable)")
	flag.StringVar(&pidFile, "pid-file", defaultPidFile(), "PID file path for start/status/stop")
	flag.StringVar(&logFile, "log-file", defaultLogFile(), "Log file path for background mode")
	flag.Int64Var(&logMaxBytes, "log-max-bytes", defaultLogMaxBytes(), "Max log file size in bytes")
	flag.IntVar(&chunkSize, "chunk-size", 1024*1024, "Chunk size in bytes for file transfer")
	flag.DurationVar(&ackTimeout, "ack-timeout", 2*time.Second, "Timeout for waiting event ACK")
	flag.IntVar(&maxRetries, "max-retries", 4, "Max retries for sending an event before failing")
	flag.IntVar(&sendWorkers, "send-workers", 2, "Concurrent sender workers for file events (send is ordered per connection)")
	flag.DurationVar(&metricsInterval, "metrics-interval", 30*time.Second, "Metrics log output interval")
	flag.DurationVar(&pingInterval, "ping-interval", 30*time.Second, "Ping interval to keep connection alive")
	flag.BoolVar(&enableResume, "enable-resume", true, "Enable resumable chunk transfer")
	flag.DurationVar(&partialTTL, "partial-ttl", 2*time.Hour, "TTL for stale partial transfer files (0 to disable cleanup)")
	flag.StringVar(&conflictPolicy, "conflict-policy", "sender-wins", "Conflict policy on receiver: sender-wins|keep-newer")
	flag.BoolVar(&backgroundChild, "background-child", false, "internal: run as detached background child")

	// Short mode flags (requested UX)
	flag.BoolVar(&shortReceive, "r", false, "short for --mode=receive")
	flag.BoolVar(&shortSend, "s", false, "short for --mode=send")
	flag.StringVar(&listen, "p", ":7070", "short for --listen/port")

	flag.Parse()

	if shortReceive && shortSend {
		log.Fatal("-r and -s cannot be used together")
	}
	if shortReceive {
		mode = "receive"
	}
	if shortSend {
		mode = "send"
	}
	if mode == "" {
		mode = "send"
	}
	listen = normalizeListen(listen)
	peer = normalizePeer(peer, listen)
	if logMaxBytes <= 0 {
		log.Fatal("--log-max-bytes must be > 0")
	}
	if strings.TrimSpace(logFile) == "" {
		logFile = defaultLogFile()
	}
	_ = trimLogFile(logFile, logMaxBytes)
	if backgroundChild {
		lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			defer lf.Close()
			os.Stdout = lf
			os.Stderr = lf
			log.SetOutput(lf)
		}
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				_ = trimLogFile(logFile, logMaxBytes)
			}
		}()
	}

	if flag.NArg() > 0 {
		sub := strings.ToLower(strings.TrimSpace(flag.Arg(0)))
		switch sub {
		case "start":
			if isProcessRunningFromPIDFile(pidFile) {
				fmt.Printf("start: already running (%s)\n", pidFile)
				os.Exit(1)
			}
			if token == "" {
				log.Fatal("--token is required")
			}
			runArgs := buildRunArgs(mode, dir, listen, peer, token, debounce, resync, excludes, pidFile, chunkSize, ackTimeout, maxRetries, sendWorkers, metricsInterval, pingInterval, enableResume, partialTTL, conflictPolicy)
			if err := startBackground(pidFile, logFile, logMaxBytes, runArgs); err != nil {
				log.Fatal(err)
			}
			return
		case "status":
			os.Exit(doStatus(pidFile))
		case "stop":
			os.Exit(doStop(pidFile))
		default:
			log.Fatalf("unknown subcommand: %s (supported: start|status|stop)", sub)
		}
	}

	cfg := syncer.Config{
		Mode:            strings.TrimSpace(mode),
		Dir:             strings.TrimSpace(dir),
		Listen:          strings.TrimSpace(listen),
		Peer:            strings.TrimSpace(peer),
		Token:           token,
		Debounce:        debounce,
		ResyncInterval:  resync,
		Excludes:        excludes,
		ChunkSize:       chunkSize,
		AckTimeout:      ackTimeout,
		MaxRetries:      maxRetries,
		SendWorkers:     sendWorkers,
		MetricsInterval: metricsInterval,
		PingInterval:    pingInterval,
		EnableResume:    enableResume,
		PartialTTL:      partialTTL,
		ConflictPolicy:  strings.TrimSpace(conflictPolicy),
	}
	if len(cfg.Excludes) == 0 {
		cfg.Excludes = []string{".git/*", "node_modules/*", ".DS_Store"}
	}
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	app, err := syncer.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("workspace-sync starting mode=%s dir=%s\n", cfg.Mode, cfg.Dir)
	if err := app.Run(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}
