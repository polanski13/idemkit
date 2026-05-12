package mem_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polanski13/idemkit"
	"github.com/polanski13/idemkit/store/mem"
)

var _ idemkit.Store = (*mem.Store)(nil)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newStore() *mem.Store {
	return mem.New(mem.Config{TTL: time.Hour, LockTimeout: 30 * time.Second})
}

func newStoreWithClock(c *fakeClock) *mem.Store {
	return mem.New(mem.Config{
		TTL:         time.Hour,
		LockTimeout: 30 * time.Second,
		Clock:       c.Now,
	})
}

func TestBegin_FreshOnFirstCall(t *testing.T) {
	s := newStore()
	state, res, tok, err := s.Begin(context.Background(), "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh", state)
	}
	if res != nil {
		t.Fatalf("result: %v, want nil", res)
	}
	if tok == 0 {
		t.Fatal("token: 0, want non-zero on Fresh")
	}
}

func TestBegin_InFlightOnSecondCallSameKey(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	if _, _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}
	state, res, tok, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateInFlight {
		t.Fatalf("state: %v, want StateInFlight", state)
	}
	if res != nil {
		t.Fatalf("result: %v, want nil", res)
	}
	if tok != 0 {
		t.Fatalf("token: %d, want 0 on non-Fresh", tok)
	}
}

func TestBegin_BodyMismatchWhileInFlight(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	if _, _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}
	state, _, _, err := s.Begin(ctx, "k", []byte{2})
	if !errors.Is(err, idemkit.ErrBodyMismatch) {
		t.Fatalf("err: %v, want ErrBodyMismatch", err)
	}
	if state != idemkit.StateInFlight {
		t.Fatalf("state: %v, want StateInFlight", state)
	}
}

func TestBegin_FreshClaimsGetUniqueTokens(t *testing.T) {
	s := newStore()
	ctx := context.Background()

	_, _, tokA, _ := s.Begin(ctx, "a", []byte{1})
	_, _, tokB, _ := s.Begin(ctx, "b", []byte{2})
	_, _, tokC, _ := s.Begin(ctx, "c", []byte{3})

	if tokA == 0 || tokB == 0 || tokC == 0 {
		t.Fatalf("zero tokens: a=%d b=%d c=%d", tokA, tokB, tokC)
	}
	if tokA == tokB || tokB == tokC || tokA == tokC {
		t.Fatalf("non-unique tokens: a=%d b=%d c=%d", tokA, tokB, tokC)
	}
}

func TestBegin_DoneWithCachedResultOnMatchingHash(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_, _, tok, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	saved := &idemkit.Result{
		StatusCode: 201,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"id":42}`),
	}
	if err := s.Save(ctx, "k", tok, saved); err != nil {
		t.Fatal(err)
	}
	state, got, _, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateDone {
		t.Fatalf("state: %v, want StateDone", state)
	}
	if !reflect.DeepEqual(got, saved) {
		t.Fatalf("result: %#v, want %#v", got, saved)
	}
	if got == saved {
		t.Fatal("result returned by reference; expected a clone")
	}
}

func TestBegin_BodyMismatchOnDone_StillReturnsCachedResult(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_, _, tok, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	saved := &idemkit.Result{StatusCode: 200}
	if err := s.Save(ctx, "k", tok, saved); err != nil {
		t.Fatal(err)
	}
	state, got, _, err := s.Begin(ctx, "k", []byte{2})
	if !errors.Is(err, idemkit.ErrBodyMismatch) {
		t.Fatalf("err: %v, want ErrBodyMismatch", err)
	}
	if state != idemkit.StateDone {
		t.Fatalf("state: %v, want StateDone", state)
	}
	if !reflect.DeepEqual(got, saved) {
		t.Fatalf("result not returned alongside mismatch error: got %#v want %#v", got, saved)
	}
}

func TestWait_ReturnsImmediatelyWhenDone(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_, _, tok, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	saved := &idemkit.Result{StatusCode: 200}
	if err := s.Save(ctx, "k", tok, saved); err != nil {
		t.Fatal(err)
	}
	got, err := s.Wait(ctx, "k")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !reflect.DeepEqual(got, saved) {
		t.Fatalf("result: %#v, want %#v", got, saved)
	}
}

func TestWait_ReturnsNilWhenAbsent(t *testing.T) {
	s := newStore()
	got, err := s.Wait(context.Background(), "absent")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Fatalf("result: %v, want nil", got)
	}
}

func TestWait_BlocksUntilSave(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_, _, tok, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}

	type out struct {
		res *idemkit.Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := s.Wait(ctx, "k")
		done <- out{res, err}
	}()

	select {
	case got := <-done:
		t.Fatalf("Wait returned prematurely: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}

	saved := &idemkit.Result{StatusCode: 200}
	if err := s.Save(ctx, "k", tok, saved); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("err: %v", got.err)
		}
		if !reflect.DeepEqual(got.res, saved) {
			t.Fatalf("result: %#v, want %#v", got.res, saved)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after Save")
	}
}

func TestWait_BlocksUntilReleaseReturnsNil(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_, _, tok, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}

	type out struct {
		res *idemkit.Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := s.Wait(ctx, "k")
		done <- out{res, err}
	}()

	select {
	case got := <-done:
		t.Fatalf("Wait returned prematurely: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}

	if err := s.Release(ctx, "k", tok); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("err: %v", got.err)
		}
		if got.res != nil {
			t.Fatalf("result: %v, want nil after Release", got.res)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after Release")
	}
}

func TestWait_HonorsCtxCancellation(t *testing.T) {
	s := newStore()
	if _, _, _, err := s.Begin(context.Background(), "k", []byte{1}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Wait(ctx, "k")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err: %v, want context.Canceled", err)
	}
}

func TestRelease_AllowsReclaim(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_, _, tok, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "k", tok); err != nil {
		t.Fatal(err)
	}
	state, _, _, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state after Release: %v, want StateFresh", state)
	}
}

func TestRelease_OnAbsentKeyIsNoop(t *testing.T) {
	s := newStore()
	if err := s.Release(context.Background(), "absent", idemkit.Token(1)); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestRelease_WithMismatchingTokenIsNoop(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})

	if err := s.Release(ctx, "k", tok+1); err != nil {
		t.Fatalf("err: %v", err)
	}

	state, _, _, _ := s.Begin(ctx, "k", []byte{1})
	if state != idemkit.StateInFlight {
		t.Fatalf("state: %v, want StateInFlight (Release with wrong token must not delete)", state)
	}
}

func TestSave_OnAbsentKeyReturnsTokenMismatch(t *testing.T) {
	s := newStore()
	err := s.Save(context.Background(), "absent", idemkit.Token(1), &idemkit.Result{StatusCode: 200})
	if !errors.Is(err, idemkit.ErrTokenMismatch) {
		t.Fatalf("err: %v, want ErrTokenMismatch", err)
	}
	state, _, _, _ := s.Begin(context.Background(), "absent", []byte{1})
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh (failed Save must not create an entry)", state)
	}
}

func TestSave_WithMismatchingTokenReturnsErrTokenMismatch(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})

	err := s.Save(ctx, "k", tok+1, &idemkit.Result{StatusCode: 200})
	if !errors.Is(err, idemkit.ErrTokenMismatch) {
		t.Fatalf("err: %v, want ErrTokenMismatch", err)
	}

	state, _, _, _ := s.Begin(ctx, "k", []byte{1})
	if state != idemkit.StateInFlight {
		t.Fatalf("state: %v, want StateInFlight (failed Save must not mutate entry)", state)
	}
}

func TestSave_AfterReclaimByOtherCallerReturnsErrTokenMismatch(t *testing.T) {
	s := newStore()
	ctx := context.Background()

	_, _, tokA, _ := s.Begin(ctx, "k", []byte{1})
	if err := s.Release(ctx, "k", tokA); err != nil {
		t.Fatal(err)
	}
	_, _, tokB, _ := s.Begin(ctx, "k", []byte{2})
	if tokA == tokB {
		t.Fatal("expected distinct tokens for reclaim")
	}

	err := s.Save(ctx, "k", tokA, &idemkit.Result{StatusCode: 200})
	if !errors.Is(err, idemkit.ErrTokenMismatch) {
		t.Fatalf("A's late Save: err=%v, want ErrTokenMismatch", err)
	}

	bResult := &idemkit.Result{StatusCode: 201}
	if err := s.Save(ctx, "k", tokB, bResult); err != nil {
		t.Fatalf("B's Save: %v", err)
	}

	state, got, _, _ := s.Begin(ctx, "k", []byte{2})
	if state != idemkit.StateDone {
		t.Fatalf("state: %v, want StateDone", state)
	}
	if !reflect.DeepEqual(got, bResult) {
		t.Fatalf("result: %#v, want B's result %#v", got, bResult)
	}
}

func TestSave_InputClonedSoCallerMutationDoesNotCorruptCache(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})

	input := &idemkit.Result{
		StatusCode: 200,
		Header:     http.Header{"X-Tag": []string{"original"}},
		Body:       []byte("body"),
	}
	if err := s.Save(ctx, "k", tok, input); err != nil {
		t.Fatal(err)
	}

	input.StatusCode = 500
	input.Header.Set("X-Tag", "mutated")
	input.Body[0] = 'X'

	got, err := s.Wait(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != 200 {
		t.Fatalf("StatusCode: %d, want 200 (caller mutation leaked)", got.StatusCode)
	}
	if got.Header.Get("X-Tag") != "original" {
		t.Fatalf("X-Tag: %q, want \"original\"", got.Header.Get("X-Tag"))
	}
	if string(got.Body) != "body" {
		t.Fatalf("Body: %q, want \"body\"", got.Body)
	}
}

func TestBegin_OutputClonedSoCallerMutationDoesNotCorruptCache(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})

	saved := &idemkit.Result{
		StatusCode: 200,
		Header:     http.Header{"X-Tag": []string{"original"}},
		Body:       []byte("body"),
	}
	if err := s.Save(ctx, "k", tok, saved); err != nil {
		t.Fatal(err)
	}

	_, got1, _, _ := s.Begin(ctx, "k", []byte{1})
	got1.Body[0] = 'X'
	got1.Header.Set("X-Tag", "tampered")

	_, got2, _, _ := s.Begin(ctx, "k", []byte{1})
	if string(got2.Body) != "body" {
		t.Fatalf("second Begin saw tampered body: %q", got2.Body)
	}
	if got2.Header.Get("X-Tag") != "original" {
		t.Fatalf("second Begin saw tampered header: %q", got2.Header.Get("X-Tag"))
	}
}

func TestLockTimeout_ExpiredInFlightIsReclaimable(t *testing.T) {
	clock := newFakeClock()
	s := newStoreWithClock(clock)
	ctx := context.Background()
	if _, _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}

	clock.Advance(31 * time.Second)

	state, _, _, err := s.Begin(ctx, "k", []byte{2})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh after lock timeout", state)
	}
}

func TestLockTimeout_NotExpiredYetStillInFlight(t *testing.T) {
	clock := newFakeClock()
	s := newStoreWithClock(clock)
	ctx := context.Background()
	if _, _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}

	clock.Advance(29 * time.Second)

	state, _, _, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateInFlight {
		t.Fatalf("state: %v, want StateInFlight (lock still valid)", state)
	}
}

func TestTTL_ExpiredDoneEntryIsReclaimable(t *testing.T) {
	clock := newFakeClock()
	s := newStoreWithClock(clock)
	ctx := context.Background()
	_, _, tok, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "k", tok, &idemkit.Result{StatusCode: 200}); err != nil {
		t.Fatal(err)
	}

	clock.Advance(2 * time.Hour)

	state, _, _, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh after TTL expiry", state)
	}
}

func TestTTL_NotExpiredYetStillDone(t *testing.T) {
	clock := newFakeClock()
	s := newStoreWithClock(clock)
	ctx := context.Background()
	_, _, tok, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	saved := &idemkit.Result{StatusCode: 200}
	if err := s.Save(ctx, "k", tok, saved); err != nil {
		t.Fatal(err)
	}

	clock.Advance(59 * time.Minute)

	state, got, _, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if state != idemkit.StateDone {
		t.Fatalf("state: %v, want StateDone (TTL still valid)", state)
	}
	if !reflect.DeepEqual(got, saved) {
		t.Fatalf("result: %#v, want %#v", got, saved)
	}
}

func TestConcurrentClaim_ExactlyOneSeesFresh(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	const goroutines = 200

	var freshCount, inflightCount atomic.Int32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			state, _, _, err := s.Begin(ctx, "k", []byte{1})
			if err != nil {
				t.Errorf("Begin: %v", err)
				return
			}
			switch state {
			case idemkit.StateFresh:
				freshCount.Add(1)
			case idemkit.StateInFlight:
				inflightCount.Add(1)
			default:
				t.Errorf("unexpected state: %v", state)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := freshCount.Load(); got != 1 {
		t.Fatalf("fresh count: %d, want exactly 1", got)
	}
	if got := inflightCount.Load(); got != goroutines-1 {
		t.Fatalf("in-flight count: %d, want %d", got, goroutines-1)
	}
}

func TestConcurrentWaiters_AllReceiveSavedResult(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	_, _, tok, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}

	const waiters = 50
	results := make(chan *idemkit.Result, waiters)
	var wg sync.WaitGroup
	wg.Add(waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			defer wg.Done()
			res, err := s.Wait(ctx, "k")
			if err != nil {
				t.Errorf("Wait: %v", err)
				return
			}
			results <- res
		}()
	}

	time.Sleep(50 * time.Millisecond)

	saved := &idemkit.Result{StatusCode: 200}
	if err := s.Save(ctx, "k", tok, saved); err != nil {
		t.Fatal(err)
	}

	wg.Wait()
	close(results)

	count := 0
	for got := range results {
		count++
		if !reflect.DeepEqual(got, saved) {
			t.Errorf("waiter received %#v, want %#v", got, saved)
		}
	}
	if count != waiters {
		t.Fatalf("waiter count: %d, want %d", count, waiters)
	}
}

func TestNew_ZeroConfigAppliesDefaults(t *testing.T) {
	s := mem.New(mem.Config{})
	ctx := context.Background()
	state, _, tok, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh", state)
	}
	if err := s.Save(ctx, "k", tok, &idemkit.Result{StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkBegin_Fresh(b *testing.B) {
	s := mem.New(mem.Config{})
	ctx := context.Background()
	hash := make([]byte, 32)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _, _ = s.Begin(ctx, strconv.Itoa(i), hash)
	}
}

func BenchmarkBegin_Done(b *testing.B) {
	s := mem.New(mem.Config{})
	ctx := context.Background()
	hash := make([]byte, 32)
	_, _, tok, _ := s.Begin(ctx, "k", hash)
	_ = s.Save(ctx, "k", tok, &idemkit.Result{StatusCode: 200, Body: []byte("ok")})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _, _ = s.Begin(ctx, "k", hash)
	}
}

func BenchmarkBegin_DoneParallel(b *testing.B) {
	s := mem.New(mem.Config{})
	ctx := context.Background()
	hash := make([]byte, 32)
	_, _, tok, _ := s.Begin(ctx, "k", hash)
	_ = s.Save(ctx, "k", tok, &idemkit.Result{StatusCode: 200, Body: []byte("ok")})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _, _, _ = s.Begin(ctx, "k", hash)
		}
	})
}

func BenchmarkBegin_Save_Roundtrip(b *testing.B) {
	s := mem.New(mem.Config{})
	ctx := context.Background()
	hash := make([]byte, 32)
	res := &idemkit.Result{StatusCode: 200, Body: []byte("ok")}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := strconv.Itoa(i)
		_, _, tok, _ := s.Begin(ctx, key, hash)
		_ = s.Save(ctx, key, tok, res)
	}
}
