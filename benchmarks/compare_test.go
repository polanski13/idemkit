package benchmarks_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/polanski13/idemkit"
	idemkitmem "github.com/polanski13/idemkit/store/mem"

	velmie "github.com/velmie/idempo"
	velmiemem "github.com/velmie/idempo/memory"
	velmiemw "github.com/velmie/idempo/middleware"
)

func benchHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

func newIdemkit() http.Handler {
	store := idemkitmem.New(idemkitmem.Config{})
	return idemkit.Middleware(store, idemkit.Config{})(benchHandler())
}

func newVelmie() http.Handler {
	store := velmiemem.New()
	engine := velmie.NewEngine(store, velmie.WithWaitForInProgress(true))
	return velmiemw.Middleware(
		velmiemw.WithEngine(engine),
		velmiemw.WithAllowedResponseHeaders("Content-Type"),
	)(benchHandler())
}

func runReplay(b *testing.B, mw http.Handler) {
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

func runFresh(b *testing.B, mw http.Handler) {
	body := `{"amount":100}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", strconv.Itoa(i))
		mw.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func runPassThrough(b *testing.B, mw http.Handler) {
	body := `{"amount":100}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/v1/charges", strings.NewReader(body))
		mw.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func runReplayParallel(b *testing.B, mw http.Handler) {
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

func BenchmarkIdemkit_Replay(b *testing.B)         { runReplay(b, newIdemkit()) }
func BenchmarkVelmie_Replay(b *testing.B)          { runReplay(b, newVelmie()) }
func BenchmarkIdemkit_Fresh(b *testing.B)          { runFresh(b, newIdemkit()) }
func BenchmarkVelmie_Fresh(b *testing.B)           { runFresh(b, newVelmie()) }
func BenchmarkIdemkit_PassThrough(b *testing.B)    { runPassThrough(b, newIdemkit()) }
func BenchmarkVelmie_PassThrough(b *testing.B)     { runPassThrough(b, newVelmie()) }
func BenchmarkIdemkit_ReplayParallel(b *testing.B) { runReplayParallel(b, newIdemkit()) }
func BenchmarkVelmie_ReplayParallel(b *testing.B)  { runReplayParallel(b, newVelmie()) }
