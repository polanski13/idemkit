package idemkit

import "net/http"

type Result struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}
