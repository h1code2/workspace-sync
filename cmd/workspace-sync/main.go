package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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

func startBackground(pidFile string, childArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, childArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := writePID(pidFile, cmd.Process.Pid); err != nil {
		return err
	}
	fmt.Printf("workspace-sync started in background, pid=%d\n", cmd.Process.Pid)
	fmt.Printf("pid file: %s\n", pidFile)
	return nil
}

func buildRunArgs(mode, dir, listen, peer, token string, debounce time.Duration, excludes []string, pidFile string) []string {
	args := []string{
		"--mode=" + mode,
		"--dir=" + dir,
		"--listen=" + listen,
		"--peer=" + peer,
		"--token=" + token,
		"--debounce=" + debounce.String(),
		"--pid-file=" + pidFile,
	}
	for _, ex := range excludes {
		args = append(args, "--exclude="+ex)
	}
	return args
}

func main() {
	var mode string
	var dir string
	var listen string
	var peer string
	var token string
	var debounce time.Duration
	var excludes multiFlag
	var pidFile string

	flag.StringVar(&mode, "mode", "send", "send | receive | both")
	flag.StringVar(&dir, "dir", ".", "Directory to watch/sync")
	flag.StringVar(&listen, "listen", ":7070", "Listen addr for receive mode")
	flag.StringVar(&peer, "peer", "", "Peer addr host:port for send mode")
	flag.StringVar(&token, "token", "", "Shared token")
	flag.DurationVar(&debounce, "debounce", 400*time.Millisecond, "Debounce window for fs events")
	flag.Var(&excludes, "exclude", "Relative glob to exclude (repeatable)")
	flag.StringVar(&pidFile, "pid-file", defaultPidFile(), "PID file path for start/status/stop")
	flag.Parse()

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
			runArgs := buildRunArgs(mode, dir, listen, peer, token, debounce, excludes, pidFile)
			if err := startBackground(pidFile, runArgs); err != nil {
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
		Mode:     strings.TrimSpace(mode),
		Dir:      strings.TrimSpace(dir),
		Listen:   strings.TrimSpace(listen),
		Peer:     strings.TrimSpace(peer),
		Token:    token,
		Debounce: debounce,
		Excludes: excludes,
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
