package idemkit

import (
	"bytes"
	"net/http"
	"slices"
)

type responseWriter struct {
	base        http.ResponseWriter
	maxBytes    int64
	statusCode  int
	capturedHdr http.Header
	body        *bytes.Buffer
	wroteHeader bool
	flushed     bool
	oversize    bool
}

func newResponseWriter(base http.ResponseWriter, maxBytes int64) *responseWriter {
	return &responseWriter{
		base:     base,
		maxBytes: maxBytes,
		body:     &bytes.Buffer{},
	}
}

func (w *responseWriter) Header() http.Header {
	return w.base.Header()
}

func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.statusCode = code
	w.capturedHdr = w.base.Header().Clone()
	w.base.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.base.Write(b)
	if !w.cacheable() {
		return n, err
	}
	if int64(w.body.Len())+int64(n) > w.maxBytes {
		w.oversize = true
		w.body = nil
		return n, err
	}
	w.body.Write(b[:n])
	return n, err
}

func (w *responseWriter) Flush() {
	w.flushed = true
	w.body = nil
	if f, ok := w.base.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *responseWriter) cacheable() bool {
	return !w.flushed && !w.oversize
}

func (w *responseWriter) snapshot() *Result {
	code := w.statusCode
	if !w.wroteHeader {
		code = http.StatusOK
	}
	hdr := w.capturedHdr
	if hdr == nil {
		hdr = w.base.Header().Clone()
	}
	var body []byte
	if w.body != nil && w.body.Len() > 0 {
		body = slices.Clone(w.body.Bytes())
	}
	return &Result{StatusCode: code, Header: hdr, Body: body}
}
