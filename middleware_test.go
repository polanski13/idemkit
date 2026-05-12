package idemkit_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polanski13/idemkit"
	"github.com/polanski13/idemkit/store/mem"
)

func newStore() *mem.Store {
	return mem.New(mem.Config{})
}

type counter struct {
	atomic.Int64
}

func (c *counter) inc() int64 { return c.Add(1) }

func doRequest(t *testing.T, mw http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	var br *strings.Reader
	if body != "" {
		br = strings.NewReader(body)
	}
	var req *http.Request
	if br != nil {
		req = httptest.NewRequest(method, path, br)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_PassesThroughNonIdempotentMethods(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(200)
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	for i := 0; i < 3; i++ {
		doRequest(t, mw, "GET", "/", "", "k1")
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("handler hits: %d, want 3 (GET should always pass through)", got)
	}
}

func TestMiddleware_DefaultMethodsExcludeGETHEADOPTIONS(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(200)
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		for i := 0; i < 2; i++ {
			doRequest(t, mw, method, "/", "", "k1")
		}
	}
	if got := hits.Load(); got != 6 {
		t.Fatalf("hits: %d, want 6 (read-only methods always pass through)", got)
	}
}

func TestMiddleware_PassesThroughWhenNoKey(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(200)
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	for i := 0; i < 3; i++ {
		doRequest(t, mw, "POST", "/", "body", "")
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("hits: %d, want 3 (no key should pass through)", got)
	}
}

func TestMiddleware_FirstRequestRunsHandlerNotReplayed(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		w.Write([]byte(`{"ok":true}`))
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	rec := doRequest(t, mw, "POST", "/charges", `{"amount":100}`, "k1")
	if rec.Code != 201 {
		t.Fatalf("code: %d", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body: %q", rec.Body.String())
	}
	if got := rec.Header().Get("X-Idemkit-Replayed"); got != "" {
		t.Fatalf("first request should not have X-Idemkit-Replayed: %q", got)
	}
}

func TestMiddleware_SecondRequestReplaysCachedWithReplayHeader(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		w.Write([]byte(`{"ok":true}`))
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	first := doRequest(t, mw, "POST", "/charges", `{"amount":100}`, "k1")
	second := doRequest(t, mw, "POST", "/charges", `{"amount":100}`, "k1")

	if got := hits.Load(); got != 1 {
		t.Fatalf("hits: %d, want 1 (second must be replayed)", got)
	}
	if first.Code != second.Code {
		t.Fatalf("status mismatch: %d vs %d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("body mismatch: %q vs %q", first.Body.String(), second.Body.String())
	}
	if first.Header().Get("Content-Type") != second.Header().Get("Content-Type") {
		t.Fatal("Content-Type not replayed")
	}
	if got := second.Header().Get("X-Idemkit-Replayed"); got != "true" {
		t.Fatalf("replay header: %q, want \"true\"", got)
	}
}

func TestMiddleware_BodyMismatchReturns422(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	doRequest(t, mw, "POST", "/", "bodyA", "k1")
	rec := doRequest(t, mw, "POST", "/", "bodyB", "k1")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code: %d, want 422", rec.Code)
	}
}

func TestMiddleware_OnConflictCallbackOverridesDefault(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	var customCalled atomic.Bool
	cfg := idemkit.Config{
		OnConflict: func(w http.ResponseWriter, r *http.Request, reason idemkit.ConflictReason) {
			customCalled.Store(true)
			http.Error(w, "custom: "+reason.String(), http.StatusTeapot)
		},
	}
	mw := idemkit.Middleware(newStore(), cfg)(h)

	doRequest(t, mw, "POST", "/", "A", "k1")
	rec := doRequest(t, mw, "POST", "/", "B", "k1")

	if !customCalled.Load() {
		t.Fatal("OnConflict was not called")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("code: %d, want 418", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "body_mismatch") {
		t.Fatalf("body: %q, expected reason in message", rec.Body.String())
	}
}

func TestMiddleware_SkipFuncBypassesIdempotency(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(200)
	})
	cfg := idemkit.Config{
		SkipFunc: func(r *http.Request) bool { return r.URL.Path == "/skip" },
	}
	mw := idemkit.Middleware(newStore(), cfg)(h)

	for i := 0; i < 3; i++ {
		doRequest(t, mw, "POST", "/skip", "body", "k1")
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("hits: %d, want 3 (SkipFunc bypasses caching)", got)
	}
}

func TestMiddleware_5xxNotCachedByDefault(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(500)
		w.Write([]byte("err"))
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	for i := 0; i < 2; i++ {
		doRequest(t, mw, "POST", "/", "body", "k1")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits: %d, want 2 (5xx must not be cached by default)", got)
	}
}

func TestMiddleware_5xxCachedWhenConfigured(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(500)
		w.Write([]byte("err"))
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{CacheServerErrors: true})(h)

	for i := 0; i < 2; i++ {
		doRequest(t, mw, "POST", "/", "body", "k1")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits: %d, want 1 (CacheServerErrors caches 5xx)", got)
	}
}

func TestMiddleware_4xxCachedByDefault(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(400)
		w.Write([]byte("bad"))
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	for i := 0; i < 2; i++ {
		doRequest(t, mw, "POST", "/", "body", "k1")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits: %d, want 1 (4xx is cacheable)", got)
	}
}

func TestMiddleware_FlushedResponseNotCached(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(200)
		w.Write([]byte("chunk1"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		w.Write([]byte("chunk2"))
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	for i := 0; i < 2; i++ {
		doRequest(t, mw, "POST", "/stream", "body", "k1")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits: %d, want 2 (flushed responses must not be cached)", got)
	}
}

func TestMiddleware_OversizeRequestBodyReturns413(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not run on oversize body")
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{MaxRequestBytes: 5})(h)

	rec := doRequest(t, mw, "POST", "/", "this is much longer than five bytes", "k1")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code: %d, want 413", rec.Code)
	}
}

func TestMiddleware_OversizeResponseNotCached(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(200)
		w.Write([]byte("more than the response cap allows"))
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{MaxResponseBytes: 5})(h)

	for i := 0; i < 2; i++ {
		doRequest(t, mw, "POST", "/", "body", "k1")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits: %d, want 2 (oversize response must not be cached)", got)
	}
}

func TestMiddleware_HandlerPanicReleasesClaimSoNextRequestSucceeds(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.inc()
		if n == 1 {
			panic("boom")
		}
		w.WriteHeader(200)
		w.Write([]byte("recovered"))
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic to propagate to caller")
			}
		}()
		doRequest(t, mw, "POST", "/", "body", "k1")
	}()

	rec := doRequest(t, mw, "POST", "/", "body", "k1")

	if got := hits.Load(); got != 2 {
		t.Fatalf("hits: %d, want 2 (claim must have been released)", got)
	}
	if rec.Code != 200 || rec.Body.String() != "recovered" {
		t.Fatalf("recovery response: %d %q", rec.Code, rec.Body.String())
	}
}

func TestMiddleware_KeyScopeIsolatesEntries(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(200)
	})
	var currentScope atomic.Value
	currentScope.Store("")
	cfg := idemkit.Config{
		KeyScope: func(r *http.Request) string { return currentScope.Load().(string) },
	}
	mw := idemkit.Middleware(newStore(), cfg)(h)

	do := func(uid string) {
		currentScope.Store(uid)
		doRequest(t, mw, "POST", "/", "body", "k1")
	}
	do("user-A")
	do("user-A")
	do("user-B")
	do("user-B")

	if got := hits.Load(); got != 2 {
		t.Fatalf("hits: %d, want 2 (one cache miss per scope)", got)
	}
}

func TestMiddleware_CustomHeaderName(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(200)
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{Header: "X-Request-ID"})(h)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/", strings.NewReader("body"))
		req.Header.Set("X-Request-ID", "rid-1")
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits: %d, want 1 (custom header must be used)", got)
	}
}

func TestMiddleware_CustomHasherUsed(t *testing.T) {
	var hasherCalled atomic.Bool
	cfg := idemkit.Config{
		Hasher: func(b []byte) []byte {
			hasherCalled.Store(true)
			return idemkit.DefaultHasher(b)
		},
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	mw := idemkit.Middleware(newStore(), cfg)(h)

	doRequest(t, mw, "POST", "/", "body", "k1")
	if !hasherCalled.Load() {
		t.Fatal("custom Hasher was not invoked")
	}
}

func TestMiddleware_ConcurrentDuplicatesAllReceiveSameResponse(t *testing.T) {
	var hits counter
	releaseHandler := make(chan struct{})
	handlerEntered := make(chan struct{}, 1)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		select {
		case handlerEntered <- struct{}{}:
		default:
		}
		<-releaseHandler
		w.WriteHeader(201)
		w.Write([]byte("created"))
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	type out struct {
		code int
		body string
	}
	const total = 6
	results := make(chan out, total)

	go func() {
		rec := doRequest(t, mw, "POST", "/", "body", "k1")
		results <- out{rec.Code, rec.Body.String()}
	}()
	<-handlerEntered

	var wg sync.WaitGroup
	wg.Add(total - 1)
	for i := 0; i < total-1; i++ {
		go func() {
			defer wg.Done()
			rec := doRequest(t, mw, "POST", "/", "body", "k1")
			results <- out{rec.Code, rec.Body.String()}
		}()
	}

	time.Sleep(80 * time.Millisecond)
	close(releaseHandler)
	wg.Wait()

	for i := 0; i < total; i++ {
		select {
		case r := <-results:
			if r.code != 201 || r.body != "created" {
				t.Errorf("result: %+v", r)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for results")
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits: %d, want 1 (only one handler invocation for %d duplicates)", got, total)
	}
}

func TestMiddleware_HandlerSeesOriginalBody(t *testing.T) {
	var seen string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		seen = string(buf[:n])
		w.WriteHeader(200)
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	doRequest(t, mw, "POST", "/", "hello world", "k1")
	if seen != "hello world" {
		t.Fatalf("handler saw body %q, want %q", seen, "hello world")
	}
}

func TestMiddleware_QueryIsPartOfFingerprint(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(200)
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	rec1 := doRequest(t, mw, "POST", "/?a=1", "body", "k1")
	rec2 := doRequest(t, mw, "POST", "/?a=2", "body", "k1")

	if hits.Load() != 1 {
		t.Fatalf("hits: %d, want 1 (different query → body mismatch on same key)", hits.Load())
	}
	if rec1.Code != 200 {
		t.Fatalf("rec1 code: %d", rec1.Code)
	}
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rec2 code: %d, want 422 (different query is a fingerprint mismatch)", rec2.Code)
	}
}

func TestMiddleware_PathIsPartOfFingerprint(t *testing.T) {
	var hits counter
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.inc()
		w.WriteHeader(200)
	})
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(h)

	rec1 := doRequest(t, mw, "POST", "/charges", "body", "k1")
	rec2 := doRequest(t, mw, "POST", "/refunds", "body", "k1")

	if rec1.Code != 200 {
		t.Fatalf("rec1: %d", rec1.Code)
	}
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rec2: %d, want 422", rec2.Code)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits: %d, want 1", hits.Load())
	}
}

func benchHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

func BenchmarkMiddleware_Baseline(b *testing.B) {
	h := benchHandler()
	body := `{"amount":100}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(body))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkMiddleware_Replay(b *testing.B) {
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(benchHandler())
	body := `{"amount":100}`

	primer := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(body))
	primer.Header.Set("Idempotency-Key", "k_prime")
	mw.ServeHTTP(httptest.NewRecorder(), primer)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "k_prime")
		mw.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkMiddleware_FreshPerKey(b *testing.B) {
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(benchHandler())
	body := `{"amount":100}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", strconv.Itoa(i))
		mw.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkMiddleware_PassThrough_NoKey(b *testing.B) {
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(benchHandler())
	body := `{"amount":100}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(body))
		mw.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkMiddleware_PassThrough_GET(b *testing.B) {
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(benchHandler())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("GET", "/v1/charges", nil)
		req.Header.Set("Idempotency-Key", "k1")
		mw.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkMiddleware_ReplayParallel(b *testing.B) {
	mw := idemkit.Middleware(newStore(), idemkit.Config{})(benchHandler())
	body := `{"amount":100}`

	primer := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(body))
	primer.Header.Set("Idempotency-Key", "k_prime")
	mw.ServeHTTP(httptest.NewRecorder(), primer)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(body))
			req.Header.Set("Idempotency-Key", "k_prime")
			mw.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
}
