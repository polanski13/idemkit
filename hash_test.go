package idemkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"testing"
)

func TestCanonical_Deterministic(t *testing.T) {
	fp := Fingerprint{
		Method: "POST",
		Path:   "/v1/charges",
		Query:  url.Values{"foo": {"bar"}, "baz": {"qux", "quux"}},
		Body:   []byte(`{"amount":100}`),
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"X-Trace":      {"abc"},
		},
		Scope: "user_42",
	}
	a := fp.Canonical()
	for i := 0; i < 200; i++ {
		b := fp.Canonical()
		if !bytes.Equal(a, b) {
			t.Fatalf("call %d not deterministic:\n a=%x\n b=%x", i, a, b)
		}
	}
}

func TestCanonical_FieldSensitivity(t *testing.T) {
	base := Fingerprint{
		Method:  "POST",
		Path:    "/x",
		Query:   url.Values{"q": {"v"}},
		Body:    []byte("body"),
		Headers: map[string][]string{"X-H": {"v"}},
		Scope:   "scope",
	}
	baseHash := base.Canonical()

	cases := []struct {
		name   string
		mutate func(Fingerprint) Fingerprint
	}{
		{"Method", func(f Fingerprint) Fingerprint { f.Method = "PUT"; return f }},
		{"Path", func(f Fingerprint) Fingerprint { f.Path = "/y"; return f }},
		{"QueryValue", func(f Fingerprint) Fingerprint { f.Query = url.Values{"q": {"w"}}; return f }},
		{"QueryKey", func(f Fingerprint) Fingerprint { f.Query = url.Values{"r": {"v"}}; return f }},
		{"Body", func(f Fingerprint) Fingerprint { f.Body = []byte("other"); return f }},
		{"HeaderValue", func(f Fingerprint) Fingerprint {
			f.Headers = map[string][]string{"X-H": {"w"}}
			return f
		}},
		{"HeaderKey", func(f Fingerprint) Fingerprint {
			f.Headers = map[string][]string{"X-I": {"v"}}
			return f
		}},
		{"Scope", func(f Fingerprint) Fingerprint { f.Scope = "other"; return f }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.mutate(base).Canonical()
			if bytes.Equal(got, baseHash) {
				t.Fatalf("mutating %s did not affect Canonical", c.name)
			}
		})
	}
}

func TestCanonical_QueryKeyOrderIrrelevant(t *testing.T) {
	keys := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	q1 := url.Values{}
	for _, k := range keys {
		q1.Add(k, k+"_value")
	}
	q2 := url.Values{}
	for i := len(keys) - 1; i >= 0; i-- {
		q2.Add(keys[i], keys[i]+"_value")
	}

	a := Fingerprint{Query: q1}.Canonical()
	b := Fingerprint{Query: q2}.Canonical()
	if !bytes.Equal(a, b) {
		t.Fatalf("query key insertion order affected Canonical:\n a=%x\n b=%x", a, b)
	}
	for i := 0; i < 200; i++ {
		c := Fingerprint{Query: q1}.Canonical()
		if !bytes.Equal(a, c) {
			t.Fatalf("call %d unstable across map iterations", i)
		}
	}
}

func TestCanonical_QueryValueOrderIrrelevant(t *testing.T) {
	a := Fingerprint{Query: url.Values{"x": {"1", "2", "3"}}}.Canonical()
	b := Fingerprint{Query: url.Values{"x": {"3", "1", "2"}}}.Canonical()
	c := Fingerprint{Query: url.Values{"x": {"2", "3", "1"}}}.Canonical()
	if !bytes.Equal(a, b) || !bytes.Equal(b, c) {
		t.Fatalf("query value order affected Canonical:\n a=%x\n b=%x\n c=%x", a, b, c)
	}
}

func TestCanonical_HeaderKeyCaseFolded(t *testing.T) {
	a := Fingerprint{Headers: map[string][]string{"Content-Type": {"text/plain"}}}.Canonical()
	b := Fingerprint{Headers: map[string][]string{"content-type": {"text/plain"}}}.Canonical()
	c := Fingerprint{Headers: map[string][]string{"CONTENT-TYPE": {"text/plain"}}}.Canonical()
	if !bytes.Equal(a, b) {
		t.Fatalf("Content-Type vs content-type differ:\n a=%x\n b=%x", a, b)
	}
	if !bytes.Equal(b, c) {
		t.Fatalf("content-type vs CONTENT-TYPE differ:\n b=%x\n c=%x", b, c)
	}
}

func TestCanonical_HeaderKeyOrderIrrelevant(t *testing.T) {
	keys := []string{"X-Alpha", "X-Bravo", "X-Charlie", "X-Delta", "X-Echo", "X-Foxtrot"}
	h1 := map[string][]string{}
	for _, k := range keys {
		h1[k] = []string{k + "_value"}
	}
	h2 := map[string][]string{}
	for i := len(keys) - 1; i >= 0; i-- {
		h2[keys[i]] = []string{keys[i] + "_value"}
	}

	a := Fingerprint{Headers: h1}.Canonical()
	b := Fingerprint{Headers: h2}.Canonical()
	if !bytes.Equal(a, b) {
		t.Fatalf("header key order affected Canonical:\n a=%x\n b=%x", a, b)
	}
	for i := 0; i < 200; i++ {
		c := Fingerprint{Headers: h1}.Canonical()
		if !bytes.Equal(a, c) {
			t.Fatalf("call %d unstable across map iterations", i)
		}
	}
}

func TestCanonical_HeaderValueOrderIrrelevant(t *testing.T) {
	a := Fingerprint{Headers: map[string][]string{"X-T": {"1", "2"}}}.Canonical()
	b := Fingerprint{Headers: map[string][]string{"X-T": {"2", "1"}}}.Canonical()
	if !bytes.Equal(a, b) {
		t.Fatalf("header value order affected Canonical")
	}
}

func TestCanonical_BoundaryAmbiguityResolved(t *testing.T) {
	a := Fingerprint{Method: "POST", Path: "/foo"}.Canonical()
	b := Fingerprint{Method: "POS", Path: "T/foo"}.Canonical()
	if bytes.Equal(a, b) {
		t.Fatalf("concat-style collision detected: POST|/foo == POS|T/foo")
	}

	c := Fingerprint{Body: []byte("ab"), Scope: "cd"}.Canonical()
	d := Fingerprint{Body: []byte("abcd"), Scope: ""}.Canonical()
	if bytes.Equal(c, d) {
		t.Fatalf("body/scope boundary collision detected")
	}
}

func TestCanonical_NilAndEmptyEquivalent(t *testing.T) {
	zero := Fingerprint{}.Canonical()
	empty := Fingerprint{
		Body:    []byte{},
		Query:   url.Values{},
		Headers: map[string][]string{},
	}.Canonical()
	if !bytes.Equal(zero, empty) {
		t.Fatalf("nil vs empty containers differ:\n zero=%x\n empty=%x", zero, empty)
	}
}

func TestCanonical_StabilityVector(t *testing.T) {
	fp := Fingerprint{
		Method: "POST",
		Path:   "/x",
		Body:   []byte("a"),
	}
	got := fp.Canonical()
	want := []byte{
		0, 0, 0, 4, 'P', 'O', 'S', 'T',
		0, 0, 0, 2, '/', 'x',
		0, 0, 0, 0,
		0, 0, 0, 1, 'a',
		0, 0, 0, 0,
		0, 0, 0, 0,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical layout drift:\n got=%x\n want=%x", got, want)
	}
}

func TestDefaultHasher_KnownEmptyVector(t *testing.T) {
	want, _ := hex.DecodeString("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	got := DefaultHasher(nil)
	if !bytes.Equal(got, want) {
		t.Fatalf("DefaultHasher(nil) drift:\n got=%x\n want=%x", got, want)
	}
}

func TestDefaultHasher_KnownNonEmptyVector(t *testing.T) {
	want, _ := hex.DecodeString("ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb")
	got := DefaultHasher([]byte("a"))
	if !bytes.Equal(got, want) {
		t.Fatalf("DefaultHasher(\"a\") drift:\n got=%x\n want=%x", got, want)
	}
}

func TestDefaultHasher_OutputLength(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("x"), bytes.Repeat([]byte("y"), 4096)} {
		if got := len(DefaultHasher(in)); got != sha256.Size {
			t.Fatalf("DefaultHasher(%q) length: got %d want %d", in, got, sha256.Size)
		}
	}
}

func BenchmarkFingerprint_Canonical(b *testing.B) {
	fp := Fingerprint{
		Method: "POST",
		Path:   "/v1/charges",
		Query:  url.Values{"foo": {"bar"}, "baz": {"qux"}},
		Body:   []byte(`{"amount":100,"currency":"usd"}`),
		Headers: http.Header{
			"Content-Type": {"application/json"},
			"X-Request-Id": {"abc-123"},
		},
		Scope: "user_42",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fp.Canonical()
	}
}

func BenchmarkFingerprint_CanonicalMinimal(b *testing.B) {
	fp := Fingerprint{
		Method: "POST",
		Path:   "/v1/charges",
		Body:   []byte(`{"amount":100}`),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fp.Canonical()
	}
}

func BenchmarkDefaultHasher_SmallPayload(b *testing.B) {
	canon := Fingerprint{
		Method: "POST",
		Path:   "/v1/charges",
		Body:   []byte(`{"amount":100}`),
	}.Canonical()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = DefaultHasher(canon)
	}
}

func BenchmarkDefaultHasher_1KiB(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = DefaultHasher(payload)
	}
}
