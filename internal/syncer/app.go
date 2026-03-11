package syncer

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

type partialWrite struct {
	tmpPath   string
	file      *os.File
	mode      uint32
	modTime   int64
	snapshot  string
	received  int64
	chunkSeen int
	discard   bool
	updatedAt time.Time
}

type fileFingerprint struct {
	Size    int64
	ModTime int64
	Hash    string
}

type applyResult struct {
	resumeFrom int64
}

type metrics struct {
	sentEvents       int64
	recvEvents       int64
	retriedEvents    int64
	ackedEvents      int64
	failedEvents     int64
	bytesSent        int64
	bytesRecv        int64
	skippedByStop    int64
	snapshotSent     int64
	snapshotKept     int64
	snapshotPruned   int64
	moveEvents       int64
	resumedTransfers int64
	conflictSkips    int64
}

type App struct {
	cfg Config

	mu         sync.Mutex
	suppressTo map[string]time.Time
	snapshots  map[string]*snapshotState
	partials   map[string]*partialWrite

	lastIndex  map[string]fileFingerprint
	watchedDir map[string]struct{}

	// senderConn is used only within sendPacketAndWaitAck to avoid ack mismatch in concurrent sends.
	senderConn net.Conn
	senderW    *bufio.Writer
	senderR    *bufio.Reader
	sendMu     sync.Mutex

	receiverCaps []string

	senderQueue     chan pendingEventTask
	senderCloseOnce sync.Once

	metrics metrics
}

func New(cfg Config) (*App, error) {
	abs, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	cfg.Dir = abs
	app := &App{
		cfg:        cfg,
		suppressTo: make(map[string]time.Time),
		snapshots:  make(map[string]*snapshotState),
		partials:   make(map[string]*partialWrite),
		lastIndex:  make(map[string]fileFingerprint),
		watchedDir: make(map[string]struct{}),
	}
	app.senderQueue = make(chan pendingEventTask, 4096)
	return app, nil
}

func (a *App) Run(ctx context.Context) error {
	errCh := make(chan error, 2)
	go a.metricsLoop(ctx)
	if a.cfg.PartialTTL > 0 {
		go a.cleanupLoop(ctx)
	}

	if a.cfg.Mode == "receive" || a.cfg.Mode == "both" {
		go func() { errCh <- a.runReceiver(ctx) }()
	}
	if a.cfg.Mode == "send" || a.cfg.Mode == "both" {
		go func() { errCh <- a.runSender(ctx) }()
		go func() { errCh <- a.runSenderWorker(ctx) }()
	}

	select {
	case <-ctx.Done():
		a.closeSenderConn()
		return ctx.Err()
	case err := <-errCh:
		a.closeSenderConn()
		if errors.Is(err, context.Canceled) {
			return err
		}
		return err
	}
}

func (a *App) metricsLoop(ctx context.Context) {
	tk := time.NewTicker(a.cfg.MetricsInterval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			logf("metrics sent=%d acked=%d retried=%d failed=%d recv=%d bytes_sent=%d bytes_recv=%d stop_skips=%d snap(sent=%d keep=%d prune=%d) move=%d resume=%d conflict_skip=%d\n",
				atomic.LoadInt64(&a.metrics.sentEvents),
				atomic.LoadInt64(&a.metrics.ackedEvents),
				atomic.LoadInt64(&a.metrics.retriedEvents),
				atomic.LoadInt64(&a.metrics.failedEvents),
				atomic.LoadInt64(&a.metrics.recvEvents),
				atomic.LoadInt64(&a.metrics.bytesSent),
				atomic.LoadInt64(&a.metrics.bytesRecv),
				atomic.LoadInt64(&a.metrics.skippedByStop),
				atomic.LoadInt64(&a.metrics.snapshotSent),
				atomic.LoadInt64(&a.metrics.snapshotKept),
				atomic.LoadInt64(&a.metrics.snapshotPruned),
				atomic.LoadInt64(&a.metrics.moveEvents),
				atomic.LoadInt64(&a.metrics.resumedTransfers),
				atomic.LoadInt64(&a.metrics.conflictSkips),
			)
		}
	}
}

func (a *App) cleanupLoop(ctx context.Context) {
	if a.cfg.PartialTTL == 0 {
		return
	}
	interval := 10 * time.Minute
	if a.cfg.PartialTTL < interval {
		interval = a.cfg.PartialTTL
	}
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			a.cleanupStalePartialsOnDisk()
			a.cleanupStalePartialsInMemory()
		}
	}
}

func (a *App) cleanupStalePartialsOnDisk() {
	if a.cfg.PartialTTL == 0 {
		return
	}
	cutoff := time.Now().Add(-a.cfg.PartialTTL)
	_ = filepath.WalkDir(a.cfg.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := filepath.Base(path)
		if !strings.HasSuffix(name, ".workspace-sync.part") {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		if st.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
		return nil
	})
}

func (a *App) cleanupStalePartialsInMemory() {
	if a.cfg.PartialTTL == 0 {
		return
	}
	cutoff := time.Now().Add(-a.cfg.PartialTTL)
	a.mu.Lock()
	defer a.mu.Unlock()
	for rel, pw := range a.partials {
		if pw == nil {
			delete(a.partials, rel)
			continue
		}
		if !pw.updatedAt.IsZero() && pw.updatedAt.After(cutoff) {
			continue
		}
		if pw.file != nil {
			_ = pw.file.Close()
		}
		if pw.tmpPath != "" {
			_ = os.Remove(pw.tmpPath)
		}
		delete(a.partials, rel)
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
	bw := bufio.NewWriterSize(conn, 256*1024)

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
		atomic.AddInt64(&a.metrics.bytesRecv, int64(len(packet)))
		evt, err := decrypt(a.cfg.Token, packet)
		if err != nil {
			logf("decrypt frame error: %v\n", err)
			continue
		}
		atomic.AddInt64(&a.metrics.recvEvents, 1)

		if evt.Type == "hello" {
			ack := fileEvent{Type: "ack", EventID: evt.EventID, Protocol: 2, Caps: []string{"ack", "chunk", "resume", "move", "stop", "metrics", "ping"}}
			_ = a.writeEvent(bw, ack)
			continue
		}
		if evt.Type == "ping" {
			ack := fileEvent{Type: "ack", EventID: evt.EventID}
			_ = a.writeEvent(bw, ack)
			continue
		}

		res, applyErr := a.applyEvent(evt)
		ack := fileEvent{Type: "ack", EventID: evt.EventID, ResumeFrom: res.resumeFrom}
		if applyErr != nil {
			ack.Error = applyErr.Error()
			logf("apply event error: %v\n", applyErr)
		}
		_ = a.writeEvent(bw, ack)
	}
}

func (a *App) writeEvent(w *bufio.Writer, evt fileEvent) error {
	packet, err := encrypt(a.cfg.Token, evt)
	if err != nil {
		return err
	}
	if err := writeFrame(w, packet); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	atomic.AddInt64(&a.metrics.bytesSent, int64(len(packet)))
	return nil
}

func (a *App) shouldKeepLocalByConflict(full string, incomingModTime int64) bool {
	if a.cfg.ConflictPolicy != "keep-newer" || incomingModTime <= 0 {
		return false
	}
	st, err := os.Stat(full)
	if err != nil {
		return false
	}
	return st.ModTime().UnixMilli() > incomingModTime
}

func (a *App) applyEvent(evt fileEvent) (applyResult, error) {
	switch evt.Type {
	case "snapshot_begin":
		if evt.Snapshot == "" {
			return applyResult{}, fmt.Errorf("snapshot_begin without snapshot id")
		}
		a.snapshotBegin(evt.Snapshot)
		logf("<- snapshot begin %s\n", evt.Snapshot)
		return applyResult{}, nil
	case "snapshot_end":
		if evt.Snapshot == "" {
			return applyResult{}, fmt.Errorf("snapshot_end without snapshot id")
		}
		if err := a.snapshotEnd(evt.Snapshot); err != nil {
			return applyResult{}, err
		}
		logf("<- snapshot end %s\n", evt.Snapshot)
		return applyResult{}, nil
	}

	rel := filepath.ToSlash(filepath.Clean(evt.Path))
	if rel == "." || rel == "" || rel == ".." || filepath.IsAbs(rel) {
		return applyResult{}, fmt.Errorf("unsafe path: %s", evt.Path)
	}
	if isExcluded(rel, a.cfg.Excludes) {
		return applyResult{}, nil
	}
	if shouldIgnoreStopMarker(rel) {
		return applyResult{}, nil
	}
	if stopRoot, stopped := a.findStopRoot(rel); stopped {
		atomic.AddInt64(&a.metrics.skippedByStop, 1)
		logf("<- skipped by .nosync (%s): %s %s\n", stopRoot, evt.Type, rel)
		return applyResult{}, nil
	}
	full := filepath.Join(a.cfg.Dir, filepath.FromSlash(rel))
	if !pathWithinBase(full, a.cfg.Dir) {
		return applyResult{}, fmt.Errorf("path escape: %s", rel)
	}

	slashRel := rel
	switch evt.Type {
	case "snapshot_keep":
		if _, err := os.Stat(full); err != nil {
			if os.IsNotExist(err) {
				return applyResult{}, fmt.Errorf("missing_local")
			}
			return applyResult{}, err
		}
		a.snapshotMarkSeen(evt.Snapshot, slashRel)
		return applyResult{}, nil
	case "mkdir":
		if err := os.MkdirAll(full, 0o755); err != nil {
			return applyResult{}, err
		}
		a.suppress(rel, 1200*time.Millisecond)
		a.snapshotMarkSeen(evt.Snapshot, slashRel)
		logf("<- mkdir %s\n", rel)
		return applyResult{}, nil
	case "move":
		oldRel := filepath.ToSlash(filepath.Clean(evt.OldPath))
		if oldRel == "." || oldRel == "" || oldRel == ".." || filepath.IsAbs(oldRel) {
			return applyResult{}, fmt.Errorf("unsafe move old path: %s", evt.OldPath)
		}
		oldFull := filepath.Join(a.cfg.Dir, filepath.FromSlash(oldRel))
		if !pathWithinBase(oldFull, a.cfg.Dir) {
			return applyResult{}, fmt.Errorf("move old path escape: %s", oldRel)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return applyResult{}, err
		}
		if err := os.Rename(oldFull, full); err != nil {
			return applyResult{}, err
		}
		a.suppress(rel, 1200*time.Millisecond)
		a.snapshotMarkSeen(evt.Snapshot, slashRel)
		atomic.AddInt64(&a.metrics.moveEvents, 1)
		logf("<- move %s -> %s\n", oldRel, rel)
		return applyResult{}, nil
	case "delete":
		a.cleanupPartial(slashRel)
		if err := os.RemoveAll(full); err != nil && !os.IsNotExist(err) {
			return applyResult{}, err
		}
		a.suppress(rel, 1200*time.Millisecond)
		return applyResult{}, nil
	case "upsert":
		if a.shouldKeepLocalByConflict(full, evt.ModTime) {
			atomic.AddInt64(&a.metrics.conflictSkips, 1)
			logf("<- conflict keep-newer skip upsert %s\n", rel)
			a.snapshotMarkSeen(evt.Snapshot, slashRel)
			return applyResult{}, nil
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return applyResult{}, err
		}
		tmp := full + ".workspace-sync.tmp"
		if err := os.WriteFile(tmp, evt.Data, os.FileMode(evt.Mode)); err != nil {
			return applyResult{}, err
		}
		if err := os.Rename(tmp, full); err != nil {
			return applyResult{}, err
		}
		if evt.ModTime > 0 {
			mt := time.UnixMilli(evt.ModTime)
			_ = os.Chtimes(full, mt, mt)
		}
		a.suppress(rel, 1200*time.Millisecond)
		a.snapshotMarkSeen(evt.Snapshot, slashRel)
		return applyResult{}, nil
	case "upsert_begin":
		resumeFrom, err := a.beginPartialWrite(full, slashRel, evt)
		if err != nil {
			return applyResult{}, err
		}
		if resumeFrom > 0 {
			atomic.AddInt64(&a.metrics.resumedTransfers, 1)
		}
		return applyResult{resumeFrom: resumeFrom}, nil
	case "upsert_chunk":
		if err := a.writePartialChunk(slashRel, evt.Data); err != nil {
			return applyResult{}, err
		}
		return applyResult{}, nil
	case "upsert_end":
		if err := a.finishPartialWrite(full, slashRel); err != nil {
			return applyResult{}, err
		}
		a.suppress(rel, 1200*time.Millisecond)
		a.snapshotMarkSeen(evt.Snapshot, slashRel)
		return applyResult{}, nil
	default:
		return applyResult{}, fmt.Errorf("unknown event type: %s", evt.Type)
	}
}

func (a *App) beginPartialWrite(full, rel string, evt fileEvent) (int64, error) {
	if a.shouldKeepLocalByConflict(full, evt.ModTime) {
		a.cleanupPartial(rel)
		a.mu.Lock()
		a.partials[rel] = &partialWrite{snapshot: evt.Snapshot, discard: true, updatedAt: time.Now()}
		a.mu.Unlock()
		atomic.AddInt64(&a.metrics.conflictSkips, 1)
		logf("<- conflict keep-newer skip upsert_begin %s\n", rel)
		return 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return 0, err
	}
	tmp := full + ".workspace-sync.part"
	resumeFrom := int64(0)
	openFlag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC

	if a.cfg.EnableResume {
		if st, err := os.Stat(tmp); err == nil {
			resumeFrom = st.Size()
			if resumeFrom < 0 {
				resumeFrom = 0
			}
			if evt.Size > 0 && resumeFrom > evt.Size {
				resumeFrom = 0
			}
			if resumeFrom > 0 {
				openFlag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
			}
		}
	}

	// Close any in-memory partial writer for this rel first.
	a.mu.Lock()
	if old, ok := a.partials[rel]; ok {
		if old.file != nil {
			_ = old.file.Close()
		}
		delete(a.partials, rel)
	}
	a.mu.Unlock()
	if resumeFrom == 0 {
		_ = os.Remove(tmp)
	}

	f, err := os.OpenFile(tmp, openFlag, os.FileMode(evt.Mode))
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	a.partials[rel] = &partialWrite{tmpPath: tmp, file: f, mode: evt.Mode, modTime: evt.ModTime, snapshot: evt.Snapshot, received: resumeFrom, updatedAt: time.Now()}
	a.mu.Unlock()
	return resumeFrom, nil
}

func (a *App) writePartialChunk(rel string, data []byte) error {
	a.mu.Lock()
	pw, ok := a.partials[rel]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("upsert_chunk without begin: %s", rel)
	}
	if pw.discard {
		pw.received += int64(len(data))
		pw.chunkSeen++
		pw.updatedAt = time.Now()
		return nil
	}
	n, err := pw.file.Write(data)
	if err != nil {
		return err
	}
	pw.received += int64(n)
	pw.chunkSeen++
	pw.updatedAt = time.Now()
	return nil
}

func (a *App) finishPartialWrite(full, rel string) error {
	a.mu.Lock()
	pw, ok := a.partials[rel]
	if ok {
		delete(a.partials, rel)
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("upsert_end without begin: %s", rel)
	}
	if pw.discard {
		return nil
	}
	if err := pw.file.Close(); err != nil {
		return err
	}
	if err := os.Chmod(pw.tmpPath, os.FileMode(pw.mode)); err != nil {
		return err
	}
	if err := os.Rename(pw.tmpPath, full); err != nil {
		return err
	}
	if pw.modTime > 0 {
		mt := time.UnixMilli(pw.modTime)
		_ = os.Chtimes(full, mt, mt)
	}
	return nil
}

func (a *App) cleanupPartial(rel string) {
	a.mu.Lock()
	pw, ok := a.partials[rel]
	if ok {
		delete(a.partials, rel)
	}
	a.mu.Unlock()
	if !ok {
		return
	}
	_ = pw.file.Close()
	_ = os.Remove(pw.tmpPath)
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

func (a *App) findStopRoot(rel string) (string, bool) {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "" || rel == "." {
		if fileExists(filepath.Join(a.cfg.Dir, ".nosync")) {
			return ".", true
		}
		return "", false
	}
	parts := strings.Split(rel, "/")
	for i := len(parts); i >= 1; i-- {
		cand := strings.Join(parts[:i], "/")
		full := filepath.Join(a.cfg.Dir, filepath.FromSlash(cand))
		if fileExists(filepath.Join(full, ".nosync")) {
			return cand, true
		}
	}
	if fileExists(filepath.Join(a.cfg.Dir, ".nosync")) {
		return ".", true
	}
	return "", false
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func shouldIgnoreStopMarker(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	return rel == ".nosync" || strings.HasSuffix(rel, "/.nosync")
}

func (a *App) shouldSkipBySenderStop(rel string) bool {
	if rel == "" || rel == "." {
		return fileExists(filepath.Join(a.cfg.Dir, ".nosync"))
	}
	parts := strings.Split(rel, "/")
	for i := len(parts); i >= 1; i-- {
		cand := strings.Join(parts[:i], "/")
		if fileExists(filepath.Join(a.cfg.Dir, filepath.FromSlash(cand), ".nosync")) {
			return true
		}
	}
	return fileExists(filepath.Join(a.cfg.Dir, ".nosync"))
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
		if shouldIgnoreStopMarker(rel) {
			return nil
		}
		if stopRoot, stopped := a.findStopRoot(rel); stopped {
			if d.IsDir() && rel == stopRoot {
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
		if err := os.Remove(path); err == nil || os.IsNotExist(err) {
			a.suppress(rel, 1200*time.Millisecond)
			atomic.AddInt64(&a.metrics.snapshotPruned, 1)
		}
		return nil
	})
	if err != nil {
		return err
	}
	_ = a.pruneEmptyDirs()
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

	for {
		if err := a.initialSync(ctx); err != nil {
			logf("initial sync failed: %v\n", err)
			select {
			case <-ctx.Done():
				a.senderCloseOnce.Do(func() { close(a.senderQueue) })
				return ctx.Err()
			case <-time.After(5 * time.Second):
				continue
			}
		}
		break
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
	}
	pingTicker := time.NewTicker(a.cfg.PingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.senderCloseOnce.Do(func() { close(a.senderQueue) })
			return ctx.Err()
		case ev := <-watcher.Events:
			rel, err := filepath.Rel(a.cfg.Dir, ev.Name)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(filepath.Clean(rel))
			if rel == "." || rel == "" || isExcluded(rel, a.cfg.Excludes) || a.isSuppressed(rel) || shouldIgnoreStopMarker(rel) || a.shouldSkipBySenderStop(rel) {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
					a.markDir(rel)
					_ = a.addWatchRecursive(watcher, ev.Name)
					_ = a.sendMkdir(ctx, rel)
					a.queueDirFiles(ctx, q, ev.Name)
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
				if !a.enqueueTask(ctx, pendingEventTask{rel: rel, op: item.op}) {
					a.senderCloseOnce.Do(func() { close(a.senderQueue) })
					return ctx.Err()
				}
				delete(q, rel)
			}
		case <-resyncCh:
			if err := a.initialSync(ctx); err != nil {
				logf("periodic resync failed: %v\n", err)
			}
		case <-pingTicker.C:
			if err := a.sendPing(ctx); err != nil {
				logf("ping failed: %v\n", err)
			}
		}
	}
}

type pendingEventTask struct {
	rel string
	op  fsnotify.Op
}

func (a *App) enqueueTask(ctx context.Context, task pendingEventTask) bool {
	select {
	case a.senderQueue <- task:
		return true
	case <-ctx.Done():
		return false
	}
}

func (a *App) queueDirFiles(ctx context.Context, q map[string]pendingEvent, dir string) {
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(a.cfg.Dir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." || rel == "" || isExcluded(rel, a.cfg.Excludes) || a.isSuppressed(rel) || shouldIgnoreStopMarker(rel) || a.shouldSkipBySenderStop(rel) {
			if d.IsDir() && rel != "." && isExcluded(rel, a.cfg.Excludes) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			a.markDir(rel)
			return nil
		}
		if !a.enqueueTask(ctx, pendingEventTask{rel: rel, op: fsnotify.Write}) {
			return ctx.Err()
		}
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
		if rel != "." && (isExcluded(rel, a.cfg.Excludes) || a.shouldSkipBySenderStop(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			a.markDir(rel)
			if err := w.Add(path); err != nil {
				return nil
			}
		}
		return nil
	})
}

func (a *App) runSenderWorker(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := 0; i < a.cfg.SendWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case t, ok := <-a.senderQueue:
					if !ok {
						return
					}
					if err := a.sendOne(ctx, t.rel, t.op); err != nil {
						logf("send error (%s): %v\n", t.rel, err)
					}
				}
			}
		}()
	}
	wg.Wait()
	return nil
}

func (a *App) sendOne(ctx context.Context, rel string, op fsnotify.Op) error {
	full := filepath.Join(a.cfg.Dir, filepath.FromSlash(rel))
	if shouldIgnoreStopMarker(rel) || a.shouldSkipBySenderStop(rel) {
		return nil
	}
	isDir := a.isKnownDir(rel)

	if op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		if _, err := os.Stat(full); err != nil {
			evt := fileEvent{Type: "delete", Path: rel, IsDir: isDir}
			if isDir {
				a.unmarkDir(rel)
			}
			delete(a.lastIndex, rel)
			_, err := a.sendEvent(ctx, evt)
			return err
		}
	}

	st, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			evt := fileEvent{Type: "delete", Path: rel, IsDir: isDir}
			if isDir {
				a.unmarkDir(rel)
			}
			delete(a.lastIndex, rel)
			_, err := a.sendEvent(ctx, evt)
			return err
		}
		return err
	}
	if st.IsDir() {
		a.markDir(rel)
		return a.sendMkdir(ctx, rel)
	}
	fp, err := a.sendFileUpsert(ctx, rel, full, st, "")
	if err != nil {
		return err
	}
	a.lastIndex[rel] = fp
	return nil
}

func (a *App) sendMkdir(ctx context.Context, rel string) error {
	_, err := a.sendEvent(ctx, fileEvent{Type: "mkdir", Path: rel})
	return err
}

func (a *App) sendPing(ctx context.Context) error {
	_, err := a.sendEvent(ctx, fileEvent{Type: "ping"})
	return err
}

func (a *App) sendFileUpsert(ctx context.Context, rel, full string, st os.FileInfo, snapshot string) (fileFingerprint, error) {
	f, err := os.Open(full)
	if err != nil {
		return fileFingerprint{}, err
	}
	defer f.Close()

	mod := st.ModTime().UnixMilli()
	begin := fileEvent{Type: "upsert_begin", Snapshot: snapshot, Path: rel, Mode: uint32(st.Mode().Perm()), ModTime: mod, Size: st.Size()}
	ack, err := a.sendEvent(ctx, begin)
	if err != nil {
		return fileFingerprint{}, err
	}

	h := sha256.New()
	if ack.ResumeFrom > 0 {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			if _, err := io.CopyN(h, f, ack.ResumeFrom); err == nil {
				_, _ = f.Seek(ack.ResumeFrom, io.SeekStart)
			}
		}
	}
	buf := make([]byte, a.cfg.ChunkSize)
	for {
		n, rErr := f.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			_, _ = h.Write(chunk)
			if _, err := a.sendEvent(ctx, fileEvent{Type: "upsert_chunk", Snapshot: snapshot, Path: rel, Data: chunk}); err != nil {
				return fileFingerprint{}, err
			}
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			return fileFingerprint{}, rErr
		}
	}

	if _, err := a.sendEvent(ctx, fileEvent{Type: "upsert_end", Snapshot: snapshot, Path: rel}); err != nil {
		return fileFingerprint{}, err
	}
	return fileFingerprint{Size: st.Size(), ModTime: mod, Hash: hex.EncodeToString(h.Sum(nil))}, nil
}

func (a *App) sendEvent(ctx context.Context, evt fileEvent) (fileEvent, error) {
	if evt.EventID == "" {
		evt.EventID = newEventID()
	}
	packet, err := encrypt(a.cfg.Token, evt)
	if err != nil {
		return fileEvent{}, err
	}
	for attempt := 0; attempt <= a.cfg.MaxRetries; attempt++ {
		ack, err := a.sendPacketAndWaitAck(ctx, packet, evt.EventID)
		if err == nil {
			atomic.AddInt64(&a.metrics.sentEvents, 1)
			if evt.Type != "upsert_chunk" && evt.Type != "ping" {
				if evt.Path != "" {
					logf("-> %s %s\n", evt.Type, evt.Path)
				}
			}
			return ack, nil
		}
		atomic.AddInt64(&a.metrics.retriedEvents, 1)
		a.closeSenderConn()
	}
	atomic.AddInt64(&a.metrics.failedEvents, 1)
	return fileEvent{}, fmt.Errorf("event send failed after retries: %s %s", evt.Type, evt.Path)
}

func (a *App) sendPacketAndWaitAck(ctx context.Context, packet []byte, eventID string) (fileEvent, error) {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if err := a.ensureSenderConn(ctx); err != nil {
		return fileEvent{}, err
	}
	if err := a.senderConn.SetDeadline(time.Now().Add(a.cfg.AckTimeout)); err != nil {
		return fileEvent{}, err
	}
	if err := writeFrame(a.senderW, packet); err != nil {
		return fileEvent{}, err
	}
	if err := a.senderW.Flush(); err != nil {
		return fileEvent{}, err
	}
	atomic.AddInt64(&a.metrics.bytesSent, int64(len(packet)))
	ackPacket, err := readFrame(a.senderR)
	if err != nil {
		return fileEvent{}, err
	}
	atomic.AddInt64(&a.metrics.bytesRecv, int64(len(ackPacket)))
	ack, err := decrypt(a.cfg.Token, ackPacket)
	if err != nil {
		return fileEvent{}, err
	}
	if ack.Type != "ack" {
		return fileEvent{}, fmt.Errorf("unexpected response type: %s", ack.Type)
	}
	if ack.EventID != eventID {
		return fileEvent{}, fmt.Errorf("ack id mismatch: got=%s want=%s", ack.EventID, eventID)
	}
	if ack.Error != "" {
		return fileEvent{}, fmt.Errorf("peer apply error: %s", ack.Error)
	}
	atomic.AddInt64(&a.metrics.ackedEvents, 1)
	_ = a.senderConn.SetDeadline(time.Time{})
	return ack, nil
}

func (a *App) ensureSenderConn(ctx context.Context) error {
	if a.senderConn != nil && a.senderW != nil && a.senderR != nil {
		return nil
	}
	conn, err := dialWithAuth(ctx, a.cfg.Peer, a.cfg.Token)
	if err != nil {
		return err
	}
	a.senderConn = conn
	a.senderW = bufio.NewWriterSize(conn, 256*1024)
	a.senderR = bufio.NewReaderSize(conn, 256*1024)

	hello := fileEvent{Type: "hello", EventID: newEventID(), Protocol: 2, Caps: []string{"ack", "chunk", "resume", "move", "stop", "metrics", "ping"}}
	packet, err := encrypt(a.cfg.Token, hello)
	if err != nil {
		a.closeSenderConn()
		return err
	}
	if err := a.senderConn.SetDeadline(time.Now().Add(a.cfg.AckTimeout)); err != nil {
		a.closeSenderConn()
		return err
	}
	if err := writeFrame(a.senderW, packet); err != nil {
		a.closeSenderConn()
		return err
	}
	if err := a.senderW.Flush(); err != nil {
		a.closeSenderConn()
		return err
	}
	ackPacket, err := readFrame(a.senderR)
	if err != nil {
		a.closeSenderConn()
		return err
	}
	ack, err := decrypt(a.cfg.Token, ackPacket)
	if err != nil {
		a.closeSenderConn()
		return err
	}
	if ack.Type != "ack" || ack.EventID != hello.EventID {
		a.closeSenderConn()
		return fmt.Errorf("hello ack mismatch")
	}
	a.receiverCaps = ack.Caps
	_ = a.senderConn.SetDeadline(time.Time{})
	return nil
}

func (a *App) closeSenderConn() {
	if a.senderW != nil {
		_ = a.senderW.Flush()
	}
	if a.senderConn != nil {
		_ = a.senderConn.Close()
	}
	a.senderConn = nil
	a.senderW = nil
	a.senderR = nil
}

func (a *App) initialSync(ctx context.Context) error {
	snapID, err := newSnapshotID()
	if err != nil {
		return err
	}
	if _, err := a.sendEvent(ctx, fileEvent{Type: "snapshot_begin", Snapshot: snapID}); err != nil {
		return err
	}
	newIndex := make(map[string]fileFingerprint, len(a.lastIndex))

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
		if rel == "." || shouldIgnoreStopMarker(rel) || a.shouldSkipBySenderStop(rel) {
			return nil
		}
		if isExcluded(rel, a.cfg.Excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			a.markDir(rel)
			return nil
		}
		st, statErr := d.Info()
		if statErr != nil {
			return nil
		}

		if prev, ok := a.lastIndex[rel]; ok && prev.Size == st.Size() && prev.ModTime == st.ModTime().UnixMilli() {
			newIndex[rel] = prev
			if _, err := a.sendEvent(ctx, fileEvent{Type: "snapshot_keep", Snapshot: snapID, Path: rel}); err != nil {
				if strings.Contains(err.Error(), "missing_local") {
					if _, sendErr := a.sendFileUpsert(ctx, rel, path, st, snapID); sendErr != nil {
						return sendErr
					}
					atomic.AddInt64(&a.metrics.snapshotSent, 1)
					return nil
				}
				return err
			}
			atomic.AddInt64(&a.metrics.snapshotKept, 1)
			return nil
		}

		fp, fpErr := fingerprintPathSmart(path, st, a.lastIndex[rel])
		if fpErr != nil {
			return nil
		}
		newIndex[rel] = fp

		if prev, ok := a.lastIndex[rel]; ok && prev.Hash != "" && prev.Hash == fp.Hash {
			if _, err := a.sendEvent(ctx, fileEvent{Type: "snapshot_keep", Snapshot: snapID, Path: rel}); err != nil {
				if strings.Contains(err.Error(), "missing_local") {
					if _, sendErr := a.sendFileUpsert(ctx, rel, path, st, snapID); sendErr != nil {
						return sendErr
					}
					atomic.AddInt64(&a.metrics.snapshotSent, 1)
					return nil
				}
				return err
			}
			atomic.AddInt64(&a.metrics.snapshotKept, 1)
			return nil
		}

		if _, sendErr := a.sendFileUpsert(ctx, rel, path, st, snapID); sendErr != nil {
			return sendErr
		}
		atomic.AddInt64(&a.metrics.snapshotSent, 1)
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if _, err := a.sendEvent(ctx, fileEvent{Type: "snapshot_end", Snapshot: snapID}); err != nil {
		return err
	}
	a.lastIndex = newIndex
	return nil
}

func fingerprintPathSmart(path string, st os.FileInfo, prev fileFingerprint) (fileFingerprint, error) {
	if prev.Hash != "" && prev.Size == st.Size() && prev.ModTime == st.ModTime().UnixMilli() {
		return prev, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fileFingerprint{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fileFingerprint{}, err
	}
	return fileFingerprint{Size: st.Size(), ModTime: st.ModTime().UnixMilli(), Hash: hex.EncodeToString(h.Sum(nil))}, nil
}

func (a *App) markDir(rel string) {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == "" {
		return
	}
	a.watchedDir[rel] = struct{}{}
}

func (a *App) unmarkDir(rel string) {
	rel = filepath.ToSlash(filepath.Clean(rel))
	delete(a.watchedDir, rel)
}

func (a *App) isKnownDir(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	_, ok := a.watchedDir[rel]
	return ok
}

func newSnapshotID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func newEventID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
