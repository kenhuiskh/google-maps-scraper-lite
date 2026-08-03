package gmaps

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PageDiagnosticSnapshot is a passive view of a browser page. Implementations
// must return it without issuing browser-driver or CDP calls so it stays useful
// when the driver itself is wedged.
type PageDiagnosticSnapshot struct {
	PageID           string
	Engine           string
	Age              time.Duration
	Closed           bool
	Crashed          bool
	TargetURL        string
	LastHTTPStatus   int
	ActiveOperation  string
	OperationElapsed time.Duration
	LastOperation    string
	LastDuration     time.Duration
	LastError        string
	PageClass        string
	ContentBytes     int
	Title            string
}

// PageDiagnosticSource is implemented by browser adapters that expose passive
// page state. It is optional so test pages and third-party Page implementations
// do not need to change.
type PageDiagnosticSource interface {
	DiagnosticSnapshot() PageDiagnosticSnapshot
}

// PageDiagnosticObserver accepts metadata derived from content that the
// scraper already reads. It never requests additional page data.
type PageDiagnosticObserver interface {
	ObservePageDiagnostics(class string, contentBytes int, title string)
}

// PoolDiagnosticSnapshot is a passive view of browser page-pool health.
type PoolDiagnosticSnapshot struct {
	Engine              string
	Capacity            int
	Created             int
	Idle                int
	Creating            int
	OldestCreateElapsed time.Duration
	Retirements         int64
	Replacements        int64
	ReplacementFailures int64
	BrowserPID          int
}

// PoolDiagnosticSource is optionally implemented by a PagePool.
type PoolDiagnosticSource interface {
	DiagnosticSnapshot() PoolDiagnosticSnapshot
}

type activePageOperation struct {
	name    string
	started time.Time
}

// PageDiagnosticsState supplies the concurrency-safe bookkeeping shared by
// the Playwright and go-rod page adapters.
type PageDiagnosticsState struct {
	mu sync.Mutex

	id        string
	engine    string
	createdAt time.Time
	nextOp    uint64
	active    map[uint64]activePageOperation

	closed         bool
	crashed        bool
	targetURL      string
	lastHTTPStatus int
	lastOperation  string
	lastDuration   time.Duration
	lastError      string
	pageClass      string
	contentBytes   int
	title          string
}

var diagnosticPageSequence atomic.Uint64

func NewPageDiagnosticsState(engine string) *PageDiagnosticsState {
	n := diagnosticPageSequence.Add(1)
	return &PageDiagnosticsState{
		id:        fmt.Sprintf("%s-%d", engine, n),
		engine:    engine,
		createdAt: time.Now(),
		active:    make(map[uint64]activePageOperation),
	}
}

// BeginOperation records a driver operation and returns its completion hook.
// Multiple operations may coexist when a watchdog closes a page concurrently;
// snapshots report the oldest operation, preserving the original wedged call.
func (s *PageDiagnosticsState) BeginOperation(name, targetURL string) func(status int, err error) {
	if s == nil {
		return func(int, error) {}
	}
	started := time.Now()
	s.mu.Lock()
	s.nextOp++
	opID := s.nextOp
	s.active[opID] = activePageOperation{name: name, started: started}
	if targetURL != "" {
		s.targetURL = targetURL
	}
	s.mu.Unlock()

	return func(status int, err error) {
		s.mu.Lock()
		delete(s.active, opID)
		s.lastOperation = name
		s.lastDuration = time.Since(started)
		if status > 0 {
			s.lastHTTPStatus = status
		}
		if err != nil {
			s.lastError = truncateDiagnostic(err.Error(), 240)
		} else {
			s.lastError = ""
		}
		if name == "close" {
			s.closed = true
		}
		s.mu.Unlock()
	}
}

func (s *PageDiagnosticsState) MarkClosed() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *PageDiagnosticsState) MarkCrashed() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.crashed = true
	s.closed = true
	s.mu.Unlock()
}

func (s *PageDiagnosticsState) ObservePage(class string, contentBytes int, title string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.pageClass = class
	s.contentBytes = contentBytes
	s.title = truncateDiagnostic(strings.TrimSpace(title), 160)
	s.mu.Unlock()
}

func (s *PageDiagnosticsState) Snapshot(driverClosed bool) PageDiagnosticSnapshot {
	if s == nil {
		return PageDiagnosticSnapshot{}
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := PageDiagnosticSnapshot{
		PageID:         s.id,
		Engine:         s.engine,
		Age:            now.Sub(s.createdAt),
		Closed:         s.closed || driverClosed,
		Crashed:        s.crashed,
		TargetURL:      s.targetURL,
		LastHTTPStatus: s.lastHTTPStatus,
		LastOperation:  s.lastOperation,
		LastDuration:   s.lastDuration,
		LastError:      s.lastError,
		PageClass:      s.pageClass,
		ContentBytes:   s.contentBytes,
		Title:          s.title,
	}
	var oldest activePageOperation
	for _, op := range s.active {
		if oldest.started.IsZero() || op.started.Before(oldest.started) {
			oldest = op
		}
	}
	if !oldest.started.IsZero() {
		snap.ActiveOperation = oldest.name
		snap.OperationElapsed = now.Sub(oldest.started)
	}
	return snap
}

func truncateDiagnostic(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

type claimTrace struct {
	mu sync.Mutex

	worker  int
	urlID   int64
	url     string
	lang    string
	attempt int
	started time.Time

	phase        string
	phaseStarted time.Time
	lastPhase    string
	lastDuration time.Duration
	stage        string
	stageStarted time.Time
	lastStage    string
	lastStageDur time.Duration
	detail       string
}

type claimTraceSnapshot struct {
	Worker       int
	URLID        int64
	URL          string
	Lang         string
	Attempt      int
	Elapsed      time.Duration
	Phase        string
	PhaseElapsed time.Duration
	LastPhase    string
	LastPhaseDur time.Duration
	Stage        string
	StageElapsed time.Duration
	LastStage    string
	LastStageDur time.Duration
	Detail       string
}

func newClaimTrace(worker int, claimed *ClaimedURL, lang string) *claimTrace {
	now := time.Now()
	return &claimTrace{
		worker:       worker,
		urlID:        claimed.ID,
		url:          claimed.URL,
		lang:         lang,
		attempt:      claimed.Attempts,
		started:      now,
		phase:        "claimed",
		phaseStarted: now,
	}
}

func (t *claimTrace) setPhase(phase string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	if t.phase != "" {
		t.lastPhase = t.phase
		t.lastDuration = now.Sub(t.phaseStarted)
	}
	t.phase = phase
	t.phaseStarted = now
	t.mu.Unlock()
}

func (t *claimTrace) setStage(stage, detail string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	if t.stage == stage {
		t.detail = truncateDiagnostic(detail, 200)
		t.mu.Unlock()
		return
	}
	if t.stage != "" {
		t.lastStage = t.stage
		t.lastStageDur = now.Sub(t.stageStarted)
	}
	t.stage = stage
	t.stageStarted = now
	t.detail = truncateDiagnostic(detail, 200)
	t.mu.Unlock()
}

func (t *claimTrace) finishStage(stage string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	if t.stage == stage {
		t.lastStage = stage
		t.lastStageDur = now.Sub(t.stageStarted)
		t.stage = ""
		t.stageStarted = time.Time{}
		t.detail = ""
	}
	t.mu.Unlock()
}

func (t *claimTrace) snapshot() claimTraceSnapshot {
	if t == nil {
		return claimTraceSnapshot{}
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	snap := claimTraceSnapshot{
		Worker:       t.worker,
		URLID:        t.urlID,
		URL:          t.url,
		Lang:         t.lang,
		Attempt:      t.attempt,
		Elapsed:      now.Sub(t.started),
		Phase:        t.phase,
		LastPhase:    t.lastPhase,
		LastPhaseDur: t.lastDuration,
		Stage:        t.stage,
		LastStage:    t.lastStage,
		LastStageDur: t.lastStageDur,
		Detail:       t.detail,
	}
	if !t.phaseStarted.IsZero() {
		snap.PhaseElapsed = now.Sub(t.phaseStarted)
	}
	if !t.stageStarted.IsZero() {
		snap.StageElapsed = now.Sub(t.stageStarted)
	}
	return snap
}

type placeTraceContextKey struct{}

func withPlaceTrace(ctx context.Context, trace *claimTrace) context.Context {
	return context.WithValue(ctx, placeTraceContextKey{}, trace)
}

func claimTraceFromContext(ctx context.Context) *claimTrace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(placeTraceContextKey{}).(*claimTrace)
	return trace
}

// claimContextDiagnostic formats the stable claim identity used by per-place
// diagnostics. A missing trace is valid for direct/unit calls, so the nil
// receiver behavior of claimTrace.snapshot supplies zero values consistently
// with the stage helpers.
func claimContextDiagnostic(ctx context.Context) string {
	claim := claimTraceFromContext(ctx).snapshot()
	return fmt.Sprintf(
		"worker=%d url_id=%d attempt=%d lang=%q url=%q",
		claim.Worker,
		claim.URLID,
		claim.Attempt,
		truncateDiagnostic(claim.Lang, 80),
		truncateDiagnostic(claim.URL, 300),
	)
}

func tracePlaceStage(ctx context.Context, stage string) func() {
	return tracePlaceStageDetail(ctx, stage, "")
}

func tracePlaceStageDetail(ctx context.Context, stage, detail string) func() {
	trace := claimTraceFromContext(ctx)
	trace.setStage(stage, detail)
	started := time.Now()
	return func() {
		trace.finishStage(stage)
		logStageTiming(stage, started)
	}
}

func updatePlaceStageDetail(ctx context.Context, stage, detail string) {
	trace := claimTraceFromContext(ctx)
	trace.setStage(stage, detail)
}

type runtimeHealthSnapshot struct {
	Goroutines  int
	HeapAlloc   uint64
	HeapSys     uint64
	Sys         uint64
	NumGC       uint32
	RSSBytes    uint64
	OpenFDs     int
	CgroupBytes uint64
	CgroupLimit uint64
}

func readRuntimeHealth() runtimeHealthSnapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	snap := runtimeHealthSnapshot{
		Goroutines: runtime.NumGoroutine(),
		HeapAlloc:  ms.HeapAlloc,
		HeapSys:    ms.HeapSys,
		Sys:        ms.Sys,
		NumGC:      ms.NumGC,
		OpenFDs:    -1,
	}
	if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
		snap.OpenFDs = len(entries)
	}
	if data, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					kb, _ := strconv.ParseUint(fields[1], 10, 64)
					snap.RSSBytes = kb * 1024
				}
				break
			}
		}
	}
	snap.CgroupBytes = readUintFile("/sys/fs/cgroup/memory.current")
	snap.CgroupLimit = readUintFile("/sys/fs/cgroup/memory.max")
	if snap.CgroupBytes == 0 {
		snap.CgroupBytes = readUintFile("/sys/fs/cgroup/memory/memory.usage_in_bytes")
	}
	if snap.CgroupLimit == 0 {
		snap.CgroupLimit = readUintFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	}
	return snap
}

func readUintFile(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value := strings.TrimSpace(string(data))
	if value == "" || value == "max" {
		return 0
	}
	n, _ := strconv.ParseUint(value, 10, 64)
	return n
}

func pageDiagnosticSnapshot(page Page) PageDiagnosticSnapshot {
	if source, ok := page.(PageDiagnosticSource); ok {
		return source.DiagnosticSnapshot()
	}
	return PageDiagnosticSnapshot{}
}

func poolDiagnosticSnapshot(pool PagePool) PoolDiagnosticSnapshot {
	if source, ok := pool.(PoolDiagnosticSource); ok {
		return source.DiagnosticSnapshot()
	}
	return PoolDiagnosticSnapshot{}
}

func logClaimDiagnostic(event string, trace *claimTrace, page Page, pool PagePool, eventErr error) {
	claim := trace.snapshot()
	pageSnap := pageDiagnosticSnapshot(page)
	poolSnap := poolDiagnosticSnapshot(pool)
	errText := ""
	if eventErr != nil {
		errText = truncateDiagnostic(eventErr.Error(), 300)
	}
	log.Printf(
		"DIAG event=%s worker=%d url_id=%d attempt=%d lang=%q elapsed=%s phase=%s phase_elapsed=%s last_phase=%s last_phase_duration=%s stage=%s stage_elapsed=%s last_stage=%s last_stage_duration=%s detail=%q page_id=%s engine=%s page_age=%s page_closed=%t page_crashed=%t page_op=%s page_op_elapsed=%s page_last_op=%s page_last_duration=%s page_status=%d page_class=%s content_bytes=%d page_title=%q target_url=%q pool_created=%d pool_idle=%d pool_capacity=%d pool_creating=%d pool_create_elapsed=%s pool_retirements=%d pool_replacements=%d pool_replacement_failures=%d error=%q url=%q",
		event,
		claim.Worker,
		claim.URLID,
		claim.Attempt,
		claim.Lang,
		claim.Elapsed.Round(time.Millisecond),
		claim.Phase,
		claim.PhaseElapsed.Round(time.Millisecond),
		claim.LastPhase,
		claim.LastPhaseDur.Round(time.Millisecond),
		claim.Stage,
		claim.StageElapsed.Round(time.Millisecond),
		claim.LastStage,
		claim.LastStageDur.Round(time.Millisecond),
		claim.Detail,
		pageSnap.PageID,
		pageSnap.Engine,
		pageSnap.Age.Round(time.Millisecond),
		pageSnap.Closed,
		pageSnap.Crashed,
		pageSnap.ActiveOperation,
		pageSnap.OperationElapsed.Round(time.Millisecond),
		pageSnap.LastOperation,
		pageSnap.LastDuration.Round(time.Millisecond),
		pageSnap.LastHTTPStatus,
		pageSnap.PageClass,
		pageSnap.ContentBytes,
		pageSnap.Title,
		pageSnap.TargetURL,
		poolSnap.Created,
		poolSnap.Idle,
		poolSnap.Capacity,
		poolSnap.Creating,
		poolSnap.OldestCreateElapsed.Round(time.Millisecond),
		poolSnap.Retirements,
		poolSnap.Replacements,
		poolSnap.ReplacementFailures,
		errText,
		claim.URL,
	)
}

func logClaimLifecycle(event string, trace *claimTrace, page Page, eventErr error) {
	claim := trace.snapshot()
	pageSnap := pageDiagnosticSnapshot(page)
	errText := ""
	if eventErr != nil {
		errText = truncateDiagnostic(eventErr.Error(), 300)
	}
	log.Printf(
		"DIAG event=%s worker=%d url_id=%d attempt=%d lang=%q elapsed=%s phase=%s stage=%s page_id=%s engine=%s error=%q url=%q",
		event,
		claim.Worker,
		claim.URLID,
		claim.Attempt,
		claim.Lang,
		claim.Elapsed.Round(time.Millisecond),
		claim.Phase,
		claim.Stage,
		pageSnap.PageID,
		pageSnap.Engine,
		errText,
		claim.URL,
	)
}

func logHealthDiagnostic(pool PagePool, inflight string) {
	health := readRuntimeHealth()
	poolSnap := poolDiagnosticSnapshot(pool)
	log.Printf(
		"DIAG event=health goroutines=%d heap_alloc=%d heap_sys=%d go_sys=%d num_gc=%d rss=%d open_fds=%d cgroup_memory=%d cgroup_limit=%d engine=%s browser_pid=%d pool_created=%d pool_idle=%d pool_capacity=%d pool_creating=%d pool_create_elapsed=%s pool_retirements=%d pool_replacements=%d pool_replacement_failures=%d inflight=%s",
		health.Goroutines,
		health.HeapAlloc,
		health.HeapSys,
		health.Sys,
		health.NumGC,
		health.RSSBytes,
		health.OpenFDs,
		health.CgroupBytes,
		health.CgroupLimit,
		poolSnap.Engine,
		poolSnap.BrowserPID,
		poolSnap.Created,
		poolSnap.Idle,
		poolSnap.Capacity,
		poolSnap.Creating,
		poolSnap.OldestCreateElapsed.Round(time.Millisecond),
		poolSnap.Retirements,
		poolSnap.Replacements,
		poolSnap.ReplacementFailures,
		inflight,
	)
}
