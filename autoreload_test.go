package caddysnake

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

func TestAutoreloadableApp_HandleRequest(t *testing.T) {
	var handled bool
	mockApp := &mockAppServer{
		onHandleRequest: func(w http.ResponseWriter, r *http.Request) error {
			handled = true
			w.WriteHeader(200)
			return nil
		},
	}

	tempDir := t.TempDir()
	a, err := NewAutoreloadableApp(mockApp, []string{tempDir}, func() (AppServer, error) { return mockApp, nil }, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewAutoreloadableApp: %v", err)
	}
	defer a.Cleanup()

	w := &mockResponseWriter{headers: make(http.Header)}
	r := &http.Request{}
	err = a.HandleRequest(w, r)
	if err != nil {
		t.Errorf("HandleRequest: %v", err)
	}
	if !handled {
		t.Error("expected mock app HandleRequest to be called")
	}
}

func TestAutoreloadableApp_Cleanup(t *testing.T) {
	var cleaned bool
	mockApp := &mockAppServer{onCleanup: func() { cleaned = true }}

	tempDir := t.TempDir()
	a, err := NewAutoreloadableApp(mockApp, []string{tempDir}, func() (AppServer, error) { return mockApp, nil }, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewAutoreloadableApp: %v", err)
	}

	err = a.Cleanup()
	if err != nil {
		t.Errorf("Cleanup: %v", err)
	}
	if !cleaned {
		t.Error("expected underlying app Cleanup to be called")
	}
}

func TestIsPythonFileEvent(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		op     fsnotify.Op
		expect bool
	}{
		{"py write", "/tmp/app.py", fsnotify.Write, true},
		{"py create", "/tmp/foo.py", fsnotify.Create, true},
		{"py remove", "/x/y.py", fsnotify.Remove, true},
		{"py rename", "/a/b/c.py", fsnotify.Rename, true},
		{"txt write", "/tmp/app.txt", fsnotify.Write, false},
		{"no ext", "/tmp/script", fsnotify.Write, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := fsnotify.Event{Name: tt.path, Op: tt.op}
			got := isPythonFileEvent(ev)
			if got != tt.expect {
				t.Errorf("isPythonFileEvent(%q, %v) = %v, want %v", tt.path, tt.op, got, tt.expect)
			}
		})
	}
}

func TestHandleNewDirEvent_NotCreate(t *testing.T) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer watcher.Close()
	// handleNewDirEvent with Write event should return early without adding
	ev := fsnotify.Event{Name: "/tmp/foo", Op: fsnotify.Write}
	handleNewDirEvent(ev, watcher)
	// No panic and no-op
}

func TestHandleNewDirEvent_CreateFile(t *testing.T) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer watcher.Close()
	// Create event for a file (not dir) - should not add
	f := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	ev := fsnotify.Event{Name: f, Op: fsnotify.Create}
	handleNewDirEvent(ev, watcher)
	// No panic - file is not a dir so it returns early
}

func TestAutoreloadableApp_ReloadFailure_TerminatesWhenExitFuncSet(t *testing.T) {
	var exitCode int
	exitCalled := make(chan struct{})
	exitFunc := func(code int) {
		exitCode = code
		close(exitCalled)
	}

	mockApp := &mockAppServer{}
	reloadErr := errors.New("app deleted")
	a, err := NewAutoreloadableApp(mockApp, []string{t.TempDir()}, func() (AppServer, error) {
		return nil, reloadErr
	}, zap.NewNop(), exitFunc)
	if err != nil {
		t.Fatalf("NewAutoreloadableApp: %v", err)
	}
	defer a.Cleanup()

	// Trigger reload by calling reload() directly (simulating file change after debounce)
	a.reload()

	select {
	case <-exitCalled:
		if exitCode != 1 {
			t.Errorf("expected exit code 1, got %d", exitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exitOnReloadFailure was not called after reload failure")
	}
}

func TestErrorApp_Returns503(t *testing.T) {
	appErr := errors.New("syntax error in app.py")
	ea := &errorApp{err: appErr}

	w := &mockResponseWriter{headers: make(http.Header)}
	r := &http.Request{}
	err := ea.HandleRequest(w, r)
	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	if w.statusCode != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.statusCode)
	}
	if w.body != "Service temporarily unavailable" {
		t.Errorf("expected generic unavailable message, got: %s", w.body)
	}
}

func TestErrorApp_Cleanup(t *testing.T) {
	ea := &errorApp{err: errors.New("test")}
	if err := ea.Cleanup(); err != nil {
		t.Errorf("expected nil error from Cleanup, got: %v", err)
	}
}

func TestAutoreloadableApp_ReloadFailure_FallsBackToErrorApp(t *testing.T) {
	mockApp := &mockAppServer{}
	reloadErr := errors.New("syntax error")
	a, err := NewAutoreloadableApp(mockApp, []string{t.TempDir()}, func() (AppServer, error) {
		return nil, reloadErr
	}, zap.NewNop(), nil) // nil exitOnReloadFailure
	if err != nil {
		t.Fatalf("NewAutoreloadableApp: %v", err)
	}
	defer a.Cleanup()

	a.reload()

	// After failed reload, requests should get 503
	w := &mockResponseWriter{headers: make(http.Header)}
	r := &http.Request{}
	err = a.HandleRequest(w, r)
	if err != nil {
		t.Errorf("HandleRequest: %v", err)
	}
	if w.statusCode != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.statusCode)
	}
}

func TestAutoreloadableApp_ReloadRecovery(t *testing.T) {
	mockApp := &mockAppServer{
		onHandleRequest: func(w http.ResponseWriter, r *http.Request) error {
			w.WriteHeader(200)
			w.Write([]byte("recovered"))
			return nil
		},
	}
	failFirst := true
	a, err := NewAutoreloadableApp(&mockAppServer{}, []string{t.TempDir()}, func() (AppServer, error) {
		if failFirst {
			failFirst = false
			return nil, errors.New("temporary failure")
		}
		return mockApp, nil
	}, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewAutoreloadableApp: %v", err)
	}
	defer a.Cleanup()

	// First reload fails
	a.reload()
	w := &mockResponseWriter{headers: make(http.Header)}
	a.HandleRequest(w, &http.Request{})
	if w.statusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 after failed reload, got %d", w.statusCode)
	}

	// Second reload succeeds (developer fixed the error)
	a.reload()
	w2 := &mockResponseWriter{headers: make(http.Header)}
	a.HandleRequest(w2, &http.Request{})
	if w2.statusCode != 200 {
		t.Errorf("expected 200 after recovery, got %d", w2.statusCode)
	}
}

func TestAutoreloadableApp_FileChangeTriggersReload(t *testing.T) {
	reloadCalled := make(chan struct{}, 1)
	var reloadCount int32

	mockApp := &mockAppServer{}
	tempDir := t.TempDir()

	a, err := NewAutoreloadableApp(mockApp, []string{tempDir}, func() (AppServer, error) {
		atomic.AddInt32(&reloadCount, 1)
		select {
		case reloadCalled <- struct{}{}:
		default:
		}
		return mockApp, nil
	}, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewAutoreloadableApp: %v", err)
	}
	defer a.Cleanup()

	// Write a .py file to trigger the watcher
	pyFile := filepath.Join(tempDir, "test_trigger.py")
	if err := os.WriteFile(pyFile, []byte("x = 1"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	select {
	case <-reloadCalled:
		// reload was triggered by file change
	case <-time.After(5 * time.Second):
		t.Fatal("expected reload to be triggered by .py file change")
	}
}

func TestConcurrentReloadSerializesFactoryCalls(t *testing.T) {
	var concurrentFactory int64 // tracks current in-flight factory calls
	var maxConcurrent int64     // tracks peak concurrency

	mockApp := &mockAppServer{}

	a, err := NewAutoreloadableApp(mockApp, []string{t.TempDir()}, func() (AppServer, error) {
		cur := atomic.AddInt64(&concurrentFactory, 1)
		// Track peak
		for {
			old := atomic.LoadInt64(&maxConcurrent)
			if cur <= old || atomic.CompareAndSwapInt64(&maxConcurrent, old, cur) {
				break
			}
		}
		// Sleep to widen the window so concurrent calls overlap
		time.Sleep(50 * time.Millisecond)
		atomic.AddInt64(&concurrentFactory, -1)
		return mockApp, nil
	}, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewAutoreloadableApp: %v", err)
	}
	defer a.Cleanup()

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)

	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			a.reload()
		}()
	}

	wg.Wait()

	peak := atomic.LoadInt64(&maxConcurrent)
	if peak != 1 {
		t.Errorf("expected max concurrent factory calls = 1 (serialized), got %d", peak)
	}
}

func TestSlowRequestDoesNotBlockReloadOrNewRequests(t *testing.T) {
	// Validates the atomic pointer swap: HandleRequest resolves the app pointer
	// under a brief RLock, then calls HandleRequest outside the lock. This means
	// a long-lived request (e.g. WebSocket) cannot block reload() or new requests.
	//
	// Pre-fix: slow HandleRequest held RLock for its entire duration → reload()
	// blocked on Lock → all new readers starved → server-wide hang.
	// Post-fix: RLock is released before the request body runs → no cascade.

	handleStarted := make(chan struct{})

	slowMock := &mockAppServer{
		onHandleRequest: func(w http.ResponseWriter, r *http.Request) error {
			close(handleStarted)
			select {
			case <-time.After(5 * time.Second):
			case <-r.Context().Done():
				return r.Context().Err()
			}
			return nil
		},
	}

	var factoryCount atomic.Int32
	fastMock := &mockAppServer{
		onHandleRequest: func(w http.ResponseWriter, r *http.Request) error {
			w.WriteHeader(200)
			return nil
		},
	}

	tempDir := t.TempDir()
	a, err := NewAutoreloadableApp(slowMock, []string{tempDir}, func() (AppServer, error) {
		factoryCount.Add(1)
		return fastMock, nil
	}, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewAutoreloadableApp: %v", err)
	}
	defer a.Cleanup()

	// Start a long-lived request (simulating WebSocket connection)
	var slowWg sync.WaitGroup
	slowWg.Add(1)
	go func() {
		defer slowWg.Done()
		w := &mockResponseWriter{headers: make(http.Header)}
		_ = a.HandleRequest(w, httptest.NewRequest("GET", "/slow", nil))
	}()

	select {
	case <-handleStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("slow request did not start within 2s")
	}

	// Trigger reload — should NOT block (no RLock held by slow request)
	reloadStart := time.Now()
	a.reload()
	reloadDuration := time.Since(reloadStart)

	if reloadDuration >= 3*time.Second {
		t.Errorf("reload blocked for %v (expected < 3s)", reloadDuration)
	}

	// Fast request should be served immediately by the new app
	w := &mockResponseWriter{headers: make(http.Header)}
	err = a.HandleRequest(w, httptest.NewRequest("GET", "/fast", nil))
	if err != nil {
		t.Errorf("fast request returned error: %v", err)
	}
	if w.statusCode != 200 {
		t.Errorf("fast request status = %d, want 200", w.statusCode)
	}

	if got := factoryCount.Load(); got != 1 {
		t.Errorf("expected factory called exactly once, got %d", got)
	}

	slowWg.Wait()
}

func TestReloadDefersOldAppCleanup(t *testing.T) {
	// Validates that reload() defers old app cleanup to avoid killing workers
	// that may still be serving in-flight requests (WebSocket, etc.).

	cleanupCh := make(chan struct{})

	initialApp := &mockAppServer{
		onHandleRequest: func(w http.ResponseWriter, r *http.Request) error {
			w.WriteHeader(200)
			return nil
		},
		onCleanup: func() {
			close(cleanupCh)
		},
	}

	newApp := &mockAppServer{
		onHandleRequest: func(w http.ResponseWriter, r *http.Request) error {
			w.WriteHeader(200)
			return nil
		},
	}

	tempDir := t.TempDir()
	a, err := NewAutoreloadableApp(initialApp, []string{tempDir}, func() (AppServer, error) {
		return newApp, nil
	}, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewAutoreloadableApp: %v", err)
	}
	defer a.Cleanup()

	// Reload — should NOT call Cleanup() on initialApp immediately
	a.reload()

	// Verify cleanup was NOT called within the first 3s
	select {
	case <-cleanupCh:
		t.Fatal("old app Cleanup called before grace period")
	case <-time.After(3 * time.Second):
		// Expected — cleanup is deferred
	}
}

func TestAutoreloadableApp_MultipleWatchDirs(t *testing.T) {
	var mu sync.Mutex
	var reloadCount int
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	mockApp := &mockAppServer{}
	a, err := NewAutoreloadableApp(mockApp, []string{dir1, dir2}, func() (AppServer, error) {
		mu.Lock()
		reloadCount++
		mu.Unlock()
		return mockApp, nil
	}, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewAutoreloadableApp: %v", err)
	}
	defer a.Cleanup()

	// Write .py file in dir1
	pyFile1 := filepath.Join(dir1, "mod1.py")
	if err := os.WriteFile(pyFile1, []byte("x = 1"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Wait for reload
	time.Sleep(800 * time.Millisecond)

	mu.Lock()
	countAfterDir1 := reloadCount
	mu.Unlock()

	if countAfterDir1 < 1 {
		t.Fatal("expected at least 1 reload after writing .py in dir1")
	}

	// Write .py file in dir2
	pyFile2 := filepath.Join(dir2, "mod2.py")
	if err := os.WriteFile(pyFile2, []byte("y = 2"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Wait for reload
	time.Sleep(800 * time.Millisecond)

	mu.Lock()
	countAfterDir2 := reloadCount
	mu.Unlock()

	if countAfterDir2 < countAfterDir1+1 {
		t.Error("expected another reload after writing .py in dir2")
	}
}
