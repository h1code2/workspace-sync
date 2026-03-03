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
	"time"

	"github.com/fsnotify/fsnotify"
)

const defaultChunkSize = 1 * 1024 * 1024 // 1MB

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
}

type fileFingerprint struct {
	Size    int64
	ModTime int64
	Hash    string
}

type App struct {
	cfg Config

	mu         sync.Mutex
	suppressTo map[string]time.Time
	snapshots  map[string]*snapshotState
	partials   map[string]*partialWrite

	lastIndex  map[string]fileFingerprint
	watchedDir map[string]struct{}

	senderConn net.Conn
	senderW    *bufio.Writer
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
		partials:   make(map[string]*partialWrite),
		lastIndex:  make(map[string]fileFingerprint),
		watchedDir: make(map[string]struct{}),
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
	if shouldIgnoreStopMarker(rel) {
		// receiver-local control file; never mutate it from remote
		return nil
	}
	if stopRoot, stopped := a.findStopRoot(rel); stopped {
		logf("<- skipped by .stop (%s): %s %s\n", stopRoot, evt.Type, rel)
		return nil
	}
	full := filepath.Join(a.cfg.Dir, rel)
	if !pathWithinBase(full, a.cfg.Dir) {
		return fmt.Errorf("path escape: %s", rel)
	}

	slashRel := filepath.ToSlash(rel)
	switch evt.Type {
	case "snapshot_keep":
		a.snapshotMarkSeen(evt.Snapshot, slashRel)
		return nil
	case "mkdir":
		if err := os.MkdirAll(full, 0o755); err != nil {
			return err
		}
		a.suppress(rel, 1200*time.Millisecond)
		a.snapshotMarkSeen(evt.Snapshot, slashRel)
		logf("<- mkdir %s\n", rel)
		return nil
	case "delete":
		a.cleanupPartial(slashRel)
		if err := os.RemoveAll(full); err != nil && !os.IsNotExist(err) {
			return err
		}
		a.suppress(rel, 1200*time.Millisecond)
		if evt.IsDir {
			logf("<- delete dir %s\n", rel)
		} else {
			logf("<- delete %s\n", rel)
		}
		return nil
	case "upsert": // backward-compatible single-frame upsert
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
	case "upsert_begin":
		if err := a.beginPartialWrite(full, slashRel, evt); err != nil {
			return err
		}
		return nil
	case "upsert_chunk":
		if err := a.writePartialChunk(slashRel, evt.Data); err != nil {
			return err
		}
		return nil
	case "upsert_end":
		if err := a.finishPartialWrite(full, slashRel); err != nil {
			return err
		}
		a.suppress(rel, 1200*time.Millisecond)
		a.snapshotMarkSeen(evt.Snapshot, slashRel)
		return nil
	default:
		return fmt.Errorf("unknown event type: %s", evt.Type)
	}
}

func (a *App) beginPartialWrite(full, rel string, evt fileEvent) error {
	a.cleanupPartial(rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp := full + ".workspace-sync.part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(evt.Mode))
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.partials[rel] = &partialWrite{
		tmpPath:  tmp,
		file:     f,
		mode:     evt.Mode,
		modTime:  evt.ModTime,
		snapshot: evt.Snapshot,
	}
	a.mu.Unlock()
	logf("<- upsert begin %s size=%d\n", rel, evt.Size)
	return nil
}

func (a *App) writePartialChunk(rel string, data []byte) error {
	a.mu.Lock()
	pw, ok := a.partials[rel]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("upsert_chunk without begin: %s", rel)
	}
	n, err := pw.file.Write(data)
	if err != nil {
		return err
	}
	pw.received += int64(n)
	pw.chunkSeen++
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
	logf("<- upsert end %s (%dB chunks=%d)\n", rel, pw.received, pw.chunkSeen)
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

// findStopRoot returns the nearest ancestor directory (or self, if dir)
// that contains a ".stop" marker file on receiver side.
func (a *App) findStopRoot(rel string) (string, bool) {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "" || rel == "." {
		if fileExists(filepath.Join(a.cfg.Dir, ".stop")) {
			return ".", true
		}
		return "", false
	}

	parts := strings.Split(rel, "/")
	for i := len(parts); i >= 1; i-- {
		cand := strings.Join(parts[:i], "/")
		full := filepath.Join(a.cfg.Dir, filepath.FromSlash(cand))
		if st, err := os.Stat(full); err == nil && !st.IsDir() {
			// if current candidate is a file path, check parent directory marker
			if i == len(parts) {
				continue
			}
		}
		if fileExists(filepath.Join(full, ".stop")) {
			return cand, true
		}
	}
	if fileExists(filepath.Join(a.cfg.Dir, ".stop")) {
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
	if rel == ".stop" {
		return true
	}
	return strings.HasSuffix(rel, "/.stop")
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
		if shouldIgnoreStopMarker(rel) {
			// receiver-local stop markers should never be removed by reconcile
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
					a.markDir(rel)
					_ = a.addWatchRecursive(watcher, ev.Name)
					_ = a.sendMkdir(ctx, rel)
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
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(a.cfg.Dir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." || rel == "" || isExcluded(rel, a.cfg.Excludes) || a.isSuppressed(rel) {
			if d.IsDir() && rel != "." && isExcluded(rel, a.cfg.Excludes) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			a.markDir(rel)
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
			a.markDir(rel)
			if err := w.Add(path); err != nil {
				return nil
			}
		}
		return nil
	})
}

func (a *App) sendOne(ctx context.Context, rel string, op fsnotify.Op) error {
	full := filepath.Join(a.cfg.Dir, rel)
	isDir := a.isKnownDir(rel)

	if op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		if _, err := os.Stat(full); err != nil {
			evt := fileEvent{Type: "delete", Path: rel, IsDir: isDir}
			if isDir {
				a.unmarkDir(rel)
			}
			delete(a.lastIndex, rel)
			return a.sendEvent(ctx, evt)
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
			return a.sendEvent(ctx, evt)
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
	evt := fileEvent{Type: "mkdir", Path: rel}
	return a.sendEvent(ctx, evt)
}

func (a *App) sendFileUpsert(ctx context.Context, rel, full string, st os.FileInfo, snapshot string) (fileFingerprint, error) {
	f, err := os.Open(full)
	if err != nil {
		return fileFingerprint{}, err
	}
	defer f.Close()

	mod := st.ModTime().UnixMilli()
	begin := fileEvent{
		Type:     "upsert_begin",
		Snapshot: snapshot,
		Path:     rel,
		Mode:     uint32(st.Mode().Perm()),
		ModTime:  mod,
		Size:     st.Size(),
	}
	if err := a.sendEvent(ctx, begin); err != nil {
		return fileFingerprint{}, err
	}

	h := sha256.New()
	buf := make([]byte, defaultChunkSize)
	for {
		n, rErr := f.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if _, err := h.Write(chunk); err != nil {
				return fileFingerprint{}, err
			}
			evt := fileEvent{Type: "upsert_chunk", Snapshot: snapshot, Path: rel, Data: chunk}
			if err := a.sendEvent(ctx, evt); err != nil {
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

	if err := a.sendEvent(ctx, fileEvent{Type: "upsert_end", Snapshot: snapshot, Path: rel}); err != nil {
		return fileFingerprint{}, err
	}

	return fileFingerprint{Size: st.Size(), ModTime: mod, Hash: hex.EncodeToString(h.Sum(nil))}, nil
}

func (a *App) sendEvent(ctx context.Context, evt fileEvent) error {
	packet, err := encrypt(a.cfg.Token, evt)
	if err != nil {
		return err
	}
	if err := a.sendPacket(ctx, packet); err != nil {
		return err
	}
	if evt.Type != "upsert_chunk" {
		if evt.Path != "" {
			logf("-> %s %s\n", evt.Type, evt.Path)
		} else {
			logf("-> %s\n", evt.Type)
		}
	}
	return nil
}

func (a *App) sendPacket(ctx context.Context, packet []byte) error {
	for attempt := 0; attempt < 2; attempt++ {
		if err := a.ensureSenderConn(ctx); err != nil {
			return err
		}
		if err := writeFrame(a.senderW, packet); err != nil {
			a.closeSenderConn()
			continue
		}
		if err := a.senderW.Flush(); err != nil {
			a.closeSenderConn()
			continue
		}
		return nil
	}
	return fmt.Errorf("send failed after reconnect")
}

func (a *App) ensureSenderConn(ctx context.Context) error {
	if a.senderConn != nil && a.senderW != nil {
		return nil
	}
	conn, err := dialWithAuth(ctx, a.cfg.Peer, a.cfg.Token)
	if err != nil {
		return err
	}
	a.senderConn = conn
	a.senderW = bufio.NewWriterSize(conn, 256*1024)
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
}

func (a *App) initialSync(ctx context.Context) error {
	snapID, err := newSnapshotID()
	if err != nil {
		return err
	}

	if err := a.sendEvent(ctx, fileEvent{Type: "snapshot_begin", Snapshot: snapID}); err != nil {
		return err
	}

	newIndex := make(map[string]fileFingerprint, len(a.lastIndex))
	sent := 0
	kept := 0

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
			a.markDir(rel)
			return nil
		}
		st, statErr := d.Info()
		if statErr != nil {
			return nil
		}

		fp, fpErr := fingerprintPath(path, st)
		if fpErr != nil {
			return nil
		}
		newIndex[rel] = fp

		if prev, ok := a.lastIndex[rel]; ok && prev == fp {
			if err := a.sendEvent(ctx, fileEvent{Type: "snapshot_keep", Snapshot: snapID, Path: rel}); err != nil {
				return err
			}
			kept++
			return nil
		}

		if _, sendErr := a.sendFileUpsert(ctx, rel, path, st, snapID); sendErr != nil {
			return sendErr
		}
		sent++
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	if err := a.sendEvent(ctx, fileEvent{Type: "snapshot_end", Snapshot: snapID}); err != nil {
		return err
	}
	a.lastIndex = newIndex
	logf("initial snapshot sent id=%s sent=%d keep=%d\n", snapID, sent, kept)
	return nil
}

func fingerprintPath(path string, st os.FileInfo) (fileFingerprint, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileFingerprint{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fileFingerprint{}, err
	}
	return fileFingerprint{
		Size:    st.Size(),
		ModTime: st.ModTime().UnixMilli(),
		Hash:    hex.EncodeToString(h.Sum(nil)),
	}, nil
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
