package idemkit

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushCount int
}

func (f *flushRecorder) Flush() {
	f.flushCount++
}

type nonFlusherWriter struct {
	hdr        http.Header
	code       int
	written    []byte
	writeCalls int
}

func (n *nonFlusherWriter) Header() http.Header {
	if n.hdr == nil {
		n.hdr = http.Header{}
	}
	return n.hdr
}

func (n *nonFlusherWriter) WriteHeader(code int) {
	n.code = code
}

func (n *nonFlusherWriter) Write(b []byte) (int, error) {
	n.writeCalls++
	n.written = append(n.written, b...)
	return len(b), nil
}

func TestResponseWriter_StatusCodeCapturedAndPassedThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 1024)
	w.WriteHeader(201)

	if w.statusCode != 201 {
		t.Fatalf("captured statusCode: %d, want 201", w.statusCode)
	}
	if rec.Code != 201 {
		t.Fatalf("base statusCode: %d, want 201", rec.Code)
	}
	if !w.wroteHeader {
		t.Fatal("wroteHeader should be true")
	}
}

func TestResponseWriter_ImplicitStatusOKOnFirstWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 1024)
	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}

	if !w.wroteHeader {
		t.Fatal("wroteHeader should be true after Write")
	}
	if w.statusCode != http.StatusOK {
		t.Fatalf("statusCode: %d, want 200", w.statusCode)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("base statusCode: %d, want 200", rec.Code)
	}
}

func TestResponseWriter_DoubleWriteHeaderSecondIgnored(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 1024)
	w.WriteHeader(200)
	w.WriteHeader(500)

	if w.statusCode != 200 {
		t.Fatalf("captured statusCode: %d, want 200 (first call wins)", w.statusCode)
	}
}

func TestResponseWriter_HeaderSnapshotTakenAtWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 1024)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Trace", "abc")
	w.WriteHeader(200)
	w.Header().Set("X-After", "ignored")

	if got := w.capturedHdr.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type captured: %q", got)
	}
	if got := w.capturedHdr.Get("X-Trace"); got != "abc" {
		t.Fatalf("X-Trace captured: %q", got)
	}
	if got := w.capturedHdr.Get("X-After"); got != "" {
		t.Fatalf("X-After should not be in snapshot: %q", got)
	}
}

func TestResponseWriter_BodyPassedThroughToBase(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 1024)
	w.Write([]byte("hello"))

	if got := rec.Body.String(); got != "hello" {
		t.Fatalf("base body: %q", got)
	}
}

func TestResponseWriter_BodyCapturedUpToLimit(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 1024)
	w.Write([]byte("hello world"))

	if got := string(w.body.Bytes()); got != "hello world" {
		t.Fatalf("captured body: %q", got)
	}
	if !w.cacheable() {
		t.Fatal("should be cacheable")
	}
}

func TestResponseWriter_BodyExactlyAtLimitIsCacheable(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 5)
	w.Write([]byte("hello"))

	if w.oversize {
		t.Fatal("exactly at limit must not be oversize")
	}
	if got := string(w.body.Bytes()); got != "hello" {
		t.Fatalf("captured body: %q", got)
	}
}

func TestResponseWriter_OversizeSingleWriteDropsCapture(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 4)
	w.Write([]byte("hello"))

	if !w.oversize {
		t.Fatal("expected oversize")
	}
	if w.body != nil {
		t.Fatal("body buffer must be dropped on oversize")
	}
	if w.cacheable() {
		t.Fatal("oversize must not be cacheable")
	}
	if got := rec.Body.String(); got != "hello" {
		t.Fatalf("base must still receive full data: %q", got)
	}
}

func TestResponseWriter_OversizeAcrossWritesDropsAllCapturedBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 6)
	w.Write([]byte("hello"))
	w.Write([]byte(" world!"))

	if !w.oversize {
		t.Fatal("expected oversize after second write")
	}
	if w.body != nil {
		t.Fatal("body buffer must be fully dropped, not partial")
	}
	if got := rec.Body.String(); got != "hello world!" {
		t.Fatalf("base: %q", got)
	}
}

func TestResponseWriter_FlushMarksUncacheableAndDropsBody(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 1024)
	w.Write([]byte("partial"))
	w.Flush()

	if !w.flushed {
		t.Fatal("flushed flag should be set")
	}
	if w.body != nil {
		t.Fatal("body buffer must be dropped on Flush")
	}
	if w.cacheable() {
		t.Fatal("flushed must not be cacheable")
	}
}

func TestResponseWriter_FlushForwardedToBaseWhenSupported(t *testing.T) {
	fr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := newResponseWriter(fr, 1024)
	w.Flush()

	if fr.flushCount != 1 {
		t.Fatalf("base Flush calls: %d, want 1", fr.flushCount)
	}
}

func TestResponseWriter_FlushSafeWhenBaseNotFlusher(t *testing.T) {
	base := &nonFlusherWriter{}
	w := newResponseWriter(base, 1024)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Flush on non-Flusher base panicked: %v", r)
		}
	}()
	w.Flush()

	if !w.flushed {
		t.Fatal("flushed flag should be set regardless of base support")
	}
}

func TestResponseWriter_WritesAfterOversizeStillPassThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 3)
	w.Write([]byte("foo"))
	w.Write([]byte("bar"))
	w.Write([]byte("baz"))

	if got := rec.Body.String(); got != "foobarbaz" {
		t.Fatalf("base body: %q, want full pass-through", got)
	}
}

func TestResponseWriter_SnapshotProducesUsableResult(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 1024)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	w.Write([]byte(`{"id":1}`))

	res := w.snapshot()

	if res.StatusCode != 201 {
		t.Fatalf("snapshot status: %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("snapshot header: %q", got)
	}
	if string(res.Body) != `{"id":1}` {
		t.Fatalf("snapshot body: %q", res.Body)
	}
}

func TestResponseWriter_SnapshotEmptyHandlerProducesImplicit200(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 1024)

	res := w.snapshot()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("empty handler status: %d, want 200", res.StatusCode)
	}
	if len(res.Body) != 0 {
		t.Fatalf("empty handler body: %q", res.Body)
	}
	if res.Header == nil {
		t.Fatal("snapshot Header should never be nil")
	}
}

func TestResponseWriter_SnapshotCapturesHeadersSetBeforeAnyWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 1024)
	w.Header().Set("X-Custom", "value")

	res := w.snapshot()

	if got := res.Header.Get("X-Custom"); got != "value" {
		t.Fatalf("snapshot header: %q", got)
	}
}

func TestResponseWriter_SnapshotBodyIsIndependentCopy(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newResponseWriter(rec, 1024)
	w.Write([]byte("original"))

	res := w.snapshot()
	res.Body[0] = 'X'

	if got := string(w.body.Bytes()); got != "original" {
		t.Fatalf("snapshot body is aliased to internal buffer: internal now %q", got)
	}
}
