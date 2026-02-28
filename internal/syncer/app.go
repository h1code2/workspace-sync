package syncer

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

func logf(format string, args ...any) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("[%s] %s", ts, fmt.Sprintf(format, args...))
}

type snapshotState struct {
	seen      map[string]struct{}
	startedAt time.Time
}

type pendingEvent struct {
	op   fsnotify.Op
	when time.Time
}

type App struct {
	cfg Config

	mu         sync.Mutex
	suppressTo map[string]time.Time
	snapshots  map[string]*snapshotState
}

func New(cfg Config) (*App, error) {
	abs, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	cfg.Dir = abs
	return &App{
		cfg:        cfg,
		suppressTo: make(map[string]time.Time),
		snapshots:  make(map[string]*snapshotState),
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	errCh := make(chan error, 2)

	if a.cfg.Mode == "receive" || a.cfg.Mode == "both" {
		go func() { errCh <- a.runReceiver(ctx) }()
	}
	if a.cfg.Mode == "send" || a.cfg.Mode == "both" {
		go func() { errCh <- a.runSender(ctx) }()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, context.Canceled) {
			return err
		}
		return err
	}
}

func (a *App) runReceiver(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.Listen)
	if err != nil {
		return err
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	logf("receiver listening %s\n", a.cfg.Listen)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		go a.handleConn(ctx, conn)
	}
}

func (a *App) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if err := serverAuth(conn, a.cfg.Token); err != nil {
		logf("auth failed from %s: %v\n", conn.RemoteAddr(), err)
		return
	}
	br := bufio.NewReader(conn)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		packet, err := readFrame(br)
		if err != nil {
			if err != io.EOF {
				logf("read frame error: %v\n", err)
			}
			return
		}
		evt, err := decrypt(a.cfg.Token, packet)
		if err != nil {
			logf("decrypt frame error: %v\n", err)
			continue
		}
		if err := a.applyEvent(evt); err != nil {
			logf("apply event error: %v\n", err)
		}
	}
}

func (a *App) applyEvent(evt fileEvent) error {
	switch evt.Type {
	case "snapshot_begin":
		if evt.Snapshot == "" {
			return fmt.Errorf("snapshot_begin without snapshot id")
		}
		a.snapshotBegin(evt.Snapshot)
		logf("<- snapshot begin %s\n", evt.Snapshot)
		return nil
	case "snapshot_end":
		if evt.Snapshot == "" {
			return fmt.Errorf("snapshot_end without snapshot id")
		}
		if err := a.snapshotEnd(evt.Snapshot); err != nil {
			return err
		}
		logf("<- snapshot end %s\n", evt.Snapshot)
		return nil
	}

	rel := filepath.Clean(evt.Path)
	if rel == "." || rel == "" || rel == ".." || filepath.IsAbs(rel) {
		return fmt.Errorf("unsafe path: %s", evt.Path)
	}
	if isExcluded(rel, a.cfg.Excludes) {
		return nil
	}
	full := filepath.Join(a.cfg.Dir, rel)
	if !pathWithinBase(full, a.cfg.Dir) {
		return fmt.Errorf("path escape: %s", rel)
	}

	slashRel := filepath.ToSlash(rel)
	switch evt.Type {
	case "delete":
		if err := os.RemoveAll(full); err != nil && !os.IsNotExist(err) {
			return err
		}
		a.suppress(rel, 1200*time.Millisecond)
		logf("<- delete %s\n", rel)
		return nil
	case "upsert":
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		tmp := full + ".workspace-sync.tmp"
		if err := os.WriteFile(tmp, evt.Data, os.FileMode(evt.Mode)); err != nil {
			return err
		}
		if err := os.Rename(tmp, full); err != nil {
			return err
		}
		if evt.ModTime > 0 {
			mt := time.UnixMilli(evt.ModTime)
			_ = os.Chtimes(full, mt, mt)
		}
		a.suppress(rel, 1200*time.Millisecond)
		a.snapshotMarkSeen(evt.Snapshot, slashRel)
		logf("<- upsert %s (%dB)\n", rel, len(evt.Data))
		return nil
	default:
		return fmt.Errorf("unknown event type: %s", evt.Type)
	}
}

func pathWithinBase(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	return true
}

func (a *App) suppress(rel string, d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.suppressTo[filepath.ToSlash(rel)] = time.Now().Add(d)
}

func (a *App) isSuppressed(rel string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	rel = filepath.ToSlash(rel)
	t, ok := a.suppressTo[rel]
	if !ok {
		return false
	}
	if time.Now().After(t) {
		delete(a.suppressTo, rel)
		return false
	}
	return true
}

func (a *App) snapshotBegin(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.snapshots[id] = &snapshotState{seen: make(map[string]struct{}), startedAt: time.Now()}
}

func (a *App) snapshotMarkSeen(id, rel string) {
	if id == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.snapshots[id]
	if !ok {
		return
	}
	st.seen[filepath.ToSlash(rel)] = struct{}{}
}

func (a *App) snapshotEnd(id string) error {
	a.mu.Lock()
	st, ok := a.snapshots[id]
	if ok {
		delete(a.snapshots, id)
	}
	a.mu.Unlock()
	if !ok {
		return nil
	}

	removedFiles := 0
	err := filepath.WalkDir(a.cfg.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(a.cfg.Dir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." {
			return nil
		}
		if isExcluded(rel, a.cfg.Excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if _, seen := st.seen[rel]; seen {
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil
		}
		a.suppress(rel, 1200*time.Millisecond)
		removedFiles++
		logf("<- snapshot prune %s\n", rel)
		return nil
	})
	if err != nil {
		return err
	}
	prunedDirs := a.pruneEmptyDirs()
	logf("snapshot reconcile done id=%s removed_files=%d removed_dirs=%d age=%s\n", id, removedFiles, prunedDirs, time.Since(st.startedAt).Round(time.Second))
	return nil
}

func (a *App) pruneEmptyDirs() int {
	var dirs []string
	_ = filepath.WalkDir(a.cfg.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(a.cfg.Dir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." || isExcluded(rel, a.cfg.Excludes) {
			if rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})

	removed := 0
	for i := len(dirs) - 1; i >= 0; i-- {
		entries, err := os.ReadDir(dirs[i])
		if err != nil || len(entries) > 0 {
			continue
		}
		if err := os.Remove(dirs[i]); err == nil {
			removed++
		}
	}
	return removed
}

func (a *App) runSender(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := a.addWatchRecursive(watcher, a.cfg.Dir); err != nil {
		return err
	}
	logf("watching %s\n", a.cfg.Dir)

	if err := a.initialSync(ctx); err != nil {
		return fmt.Errorf("initial sync failed: %w", err)
	}

	q := map[string]pendingEvent{}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	var resyncTicker *time.Ticker
	var resyncCh <-chan time.Time
	if a.cfg.ResyncInterval > 0 {
		resyncTicker = time.NewTicker(a.cfg.ResyncInterval)
		resyncCh = resyncTicker.C
		defer resyncTicker.Stop()
		logf("periodic resync enabled interval=%s\n", a.cfg.ResyncInterval)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-watcher.Events:
			rel, err := filepath.Rel(a.cfg.Dir, ev.Name)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(filepath.Clean(rel))
			if rel == "." || rel == "" || isExcluded(rel, a.cfg.Excludes) || a.isSuppressed(rel) {
				continue
			}

			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
					_ = a.addWatchRecursive(watcher, ev.Name)
					a.queueDirFiles(q, ev.Name)
					continue
				}
			}

			q[rel] = pendingEvent{op: ev.Op, when: time.Now()}
		case err := <-watcher.Errors:
			if err != nil {
				logf("watcher error: %v\n", err)
			}
		case <-tick.C:
			now := time.Now()
			for rel, item := range q {
				if now.Sub(item.when) < a.cfg.Debounce {
					continue
				}
				if err := a.sendOne(ctx, rel, item.op); err != nil {
					logf("send error (%s): %v\n", rel, err)
				}
				delete(q, rel)
			}
		case <-resyncCh:
			if err := a.initialSync(ctx); err != nil {
				logf("periodic resync failed: %v\n", err)
			} else {
				logf("periodic resync done\n")
			}
		}
	}
}

func (a *App) queueDirFiles(q map[string]pendingEvent, dir string) {
	now := time.Now()
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(a.cfg.Dir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." || rel == "" || isExcluded(rel, a.cfg.Excludes) || a.isSuppressed(rel) {
			return nil
		}
		q[rel] = pendingEvent{op: fsnotify.Write, when: now}
		return nil
	})
}

func (a *App) addWatchRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(a.cfg.Dir, path)
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel != "." && isExcluded(rel, a.cfg.Excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if err := w.Add(path); err != nil {
				return nil
			}
		}
		return nil
	})
}

func (a *App) sendOne(ctx context.Context, rel string, op fsnotify.Op) error {
	full := filepath.Join(a.cfg.Dir, rel)
	evt := fileEvent{Path: rel}

	if op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		if _, err := os.Stat(full); err != nil {
			evt.Type = "delete"
			return a.sendEvent(ctx, evt)
		}
	}

	st, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			evt.Type = "delete"
			return a.sendEvent(ctx, evt)
		}
		return err
	}
	if st.IsDir() {
		return nil
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	evt.Type = "upsert"
	evt.Data = data
	evt.Mode = uint32(st.Mode().Perm())
	evt.ModTime = st.ModTime().UnixMilli()
	return a.sendEvent(ctx, evt)
}

func (a *App) sendEvent(ctx context.Context, evt fileEvent) error {
	packet, err := encrypt(a.cfg.Token, evt)
	if err != nil {
		return err
	}
	conn, err := dialWithAuth(ctx, a.cfg.Peer, a.cfg.Token)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := writeFrame(conn, packet); err != nil {
		return err
	}
	logf("-> %s %s\n", evt.Type, evt.Path)
	return nil
}

func (a *App) initialSync(ctx context.Context) error {
	snapID, err := newSnapshotID()
	if err != nil {
		return err
	}
	conn, err := dialWithAuth(ctx, a.cfg.Peer, a.cfg.Token)
	if err != nil {
		return err
	}
	defer conn.Close()

	send := func(evt fileEvent) error {
		packet, err := encrypt(a.cfg.Token, evt)
		if err != nil {
			return err
		}
		return writeFrame(conn, packet)
	}

	if err := send(fileEvent{Type: "snapshot_begin", Snapshot: snapID}); err != nil {
		return err
	}

	count := 0
	err = filepath.WalkDir(a.cfg.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rel, relErr := filepath.Rel(a.cfg.Dir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." {
			return nil
		}
		if isExcluded(rel, a.cfg.Excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		st, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		evt := fileEvent{
			Type:     "upsert",
			Snapshot: snapID,
			Path:     rel,
			Data:     data,
			Mode:     uint32(st.Mode().Perm()),
			ModTime:  st.ModTime().UnixMilli(),
		}
		if sendErr := send(evt); sendErr != nil {
			return sendErr
		}
		count++
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if err := send(fileEvent{Type: "snapshot_end", Snapshot: snapID}); err != nil {
		return err
	}
	logf("initial snapshot sent id=%s files=%d\n", snapID, count)
	return nil
}

func newSnapshotID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
