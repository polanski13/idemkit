package idemkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
)

type Fingerprint struct {
	Method  string
	Path    string
	Query   url.Values
	Body    []byte
	Headers http.Header
	Scope   string
}

func (f Fingerprint) Canonical() []byte {
	var buf bytes.Buffer
	writeBlock(&buf, []byte(f.Method))
	writeBlock(&buf, []byte(f.Path))
	writeBlock(&buf, canonicalPairs(f.Query))
	writeBlock(&buf, f.Body)
	writeBlock(&buf, canonicalHeaders(f.Headers))
	writeBlock(&buf, []byte(f.Scope))
	return buf.Bytes()
}

func DefaultHasher(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func writeBlock(buf *bytes.Buffer, b []byte) {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(b)))
	buf.Write(prefix[:])
	buf.Write(b)
}

func canonicalPairs(m map[string][]string) []byte {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var buf bytes.Buffer
	for _, k := range keys {
		values := slices.Clone(m[k])
		slices.Sort(values)
		writeBlock(&buf, []byte(k))
		var cnt [4]byte
		binary.BigEndian.PutUint32(cnt[:], uint32(len(values)))
		buf.Write(cnt[:])
		for _, v := range values {
			writeBlock(&buf, []byte(v))
		}
	}
	return buf.Bytes()
}

func canonicalHeaders(m http.Header) []byte {
	if len(m) == 0 {
		return nil
	}
	folded := make(map[string][]string, len(m))
	for k, v := range m {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		folded[ck] = append(folded[ck], v...)
	}
	return canonicalPairs(folded)
}
