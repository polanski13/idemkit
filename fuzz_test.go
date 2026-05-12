package idemkit

import (
	"bytes"
	"testing"
)

func FuzzFingerprintCanonical(f *testing.F) {
	seeds := []struct {
		method, path string
		body         []byte
		scope        string
	}{
		{"POST", "/v1/charges", []byte(`{"x":1}`), "user_1"},
		{"GET", "/", nil, ""},
		{"PUT", "/items/123", []byte("body"), "scope"},
		{"DELETE", "", []byte("\x00\x01\xff"), "tenant-abc"},
		{"PATCH", "/π/中文", []byte("emoji 🚀"), "ünicode"},
		{"", "", nil, ""},
	}
	for _, s := range seeds {
		f.Add(s.method, s.path, s.body, s.scope)
	}

	f.Fuzz(func(t *testing.T, method, path string, body []byte, scope string) {
		fp := Fingerprint{Method: method, Path: path, Body: body, Scope: scope}
		a := fp.Canonical()
		b := fp.Canonical()
		if !bytes.Equal(a, b) {
			t.Fatalf("non-deterministic: method=%q path=%q body=%q scope=%q", method, path, body, scope)
		}
		if got := len(DefaultHasher(a)); got != 32 {
			t.Fatalf("DefaultHasher output length: got %d want 32", got)
		}

		altered := fp
		altered.Method = method + "z"
		if bytes.Equal(altered.Canonical(), a) {
			t.Fatal("appending to Method did not change Canonical")
		}
		altered = fp
		altered.Path = path + "z"
		if bytes.Equal(altered.Canonical(), a) {
			t.Fatal("appending to Path did not change Canonical")
		}
		altered = fp
		altered.Body = append(append([]byte(nil), body...), 'z')
		if bytes.Equal(altered.Canonical(), a) {
			t.Fatal("appending to Body did not change Canonical")
		}
		altered = fp
		altered.Scope = scope + "z"
		if bytes.Equal(altered.Canonical(), a) {
			t.Fatal("appending to Scope did not change Canonical")
		}
	})
}
