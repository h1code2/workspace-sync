package syncer

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	Mode            string
	Dir             string
	Listen          string
	Peer            string
	Token           string
	Debounce        time.Duration
	ResyncInterval  time.Duration
	Excludes        []string
	ChunkSize       int
	AckTimeout      time.Duration
	MaxRetries      int
	SendWorkers     int
	MetricsInterval time.Duration
	EnableResume    bool
	PartialTTL      time.Duration
	ConflictPolicy  string
	PingInterval    time.Duration
}

func (c Config) Validate() error {
	switch c.Mode {
	case "send", "receive", "both":
	default:
		return fmt.Errorf("invalid --mode: %s", c.Mode)
	}
	if c.Dir == "" {
		return fmt.Errorf("--dir is required")
	}
	abs, err := filepath.Abs(c.Dir)
	if err != nil {
		return err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("--dir must be a directory")
	}
	if c.Token == "" {
		return fmt.Errorf("--token is required")
	}
	if (c.Mode == "send" || c.Mode == "both") && c.Peer == "" {
		return fmt.Errorf("--peer is required in send/both mode")
	}
	if c.Debounce <= 0 {
		return fmt.Errorf("--debounce must be > 0")
	}
	if c.ResyncInterval < 0 {
		return fmt.Errorf("--resync must be >= 0")
	}
	if c.ChunkSize <= 0 {
		return fmt.Errorf("--chunk-size must be > 0")
	}
	if c.AckTimeout <= 0 {
		return fmt.Errorf("--ack-timeout must be > 0")
	}
	if c.MaxRetries < 0 {
		return fmt.Errorf("--max-retries must be >= 0")
	}
	if c.SendWorkers <= 0 {
		return fmt.Errorf("--send-workers must be > 0")
	}
	if c.MetricsInterval <= 0 {
		return fmt.Errorf("--metrics-interval must be > 0")
	}
	if c.PartialTTL < 0 {
		return fmt.Errorf("--partial-ttl must be >= 0")
	}
	if c.PingInterval <= 0 {
		return fmt.Errorf("--ping-interval must be > 0")
	}
	switch c.ConflictPolicy {
	case "", "sender-wins", "keep-newer":
	default:
		return fmt.Errorf("--conflict-policy must be sender-wins|keep-newer")
	}
	return nil
}
