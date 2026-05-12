package idemkit

import (
	"net/http"
	"slices"
)

type Result struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (r *Result) Clone() *Result {
	if r == nil {
		return nil
	}
	out := &Result{StatusCode: r.StatusCode}
	if r.Header != nil {
		out.Header = r.Header.Clone()
	}
	if r.Body != nil {
		out.Body = slices.Clone(r.Body)
	}
	return out
}
