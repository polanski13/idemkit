package mem_test

import (
	"context"
	"errors"
	"net/http"
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
	state, res, err := s.Begin(context.Background(), "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh", state)
	}
	if res != nil {
		t.Fatalf("result: %v, want nil", res)
	}
}

func TestBegin_InFlightOnSecondCallSameKey(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}
	state, res, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateInFlight {
		t.Fatalf("state: %v, want StateInFlight", state)
	}
	if res != nil {
		t.Fatalf("result: %v, want nil", res)
	}
}

func TestBegin_BodyMismatchWhileInFlight(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}
	state, _, err := s.Begin(ctx, "k", []byte{2})
	if !errors.Is(err, idemkit.ErrBodyMismatch) {
		t.Fatalf("err: %v, want ErrBodyMismatch", err)
	}
	if state != idemkit.StateInFlight {
		t.Fatalf("state: %v, want StateInFlight", state)
	}
}

func TestBegin_DoneWithCachedResultOnMatchingHash(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}
	saved := &idemkit.Result{
		StatusCode: 201,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"id":42}`),
	}
	if err := s.Save(ctx, "k", saved); err != nil {
		t.Fatal(err)
	}
	state, got, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateDone {
		t.Fatalf("state: %v, want StateDone", state)
	}
	if got != saved {
		t.Fatalf("result: %v, want saved", got)
	}
}

func TestBegin_BodyMismatchOnDone_StillReturnsCachedResult(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}
	saved := &idemkit.Result{StatusCode: 200}
	if err := s.Save(ctx, "k", saved); err != nil {
		t.Fatal(err)
	}
	state, got, err := s.Begin(ctx, "k", []byte{2})
	if !errors.Is(err, idemkit.ErrBodyMismatch) {
		t.Fatalf("err: %v, want ErrBodyMismatch", err)
	}
	if state != idemkit.StateDone {
		t.Fatalf("state: %v, want StateDone", state)
	}
	if got != saved {
		t.Fatalf("result not returned alongside mismatch error: %v", got)
	}
}

func TestWait_ReturnsImmediatelyWhenDone(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}
	saved := &idemkit.Result{StatusCode: 200}
	if err := s.Save(ctx, "k", saved); err != nil {
		t.Fatal(err)
	}
	got, err := s.Wait(ctx, "k")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != saved {
		t.Fatalf("result: %v, want saved", got)
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
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
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
	if err := s.Save(ctx, "k", saved); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("err: %v", got.err)
		}
		if got.res != saved {
			t.Fatalf("result: %v, want saved", got.res)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after Save")
	}
}

func TestWait_BlocksUntilReleaseReturnsNil(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
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

	if err := s.Release(ctx, "k"); err != nil {
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
	if _, _, err := s.Begin(context.Background(), "k", []byte{1}); err != nil {
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
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	state, _, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state after Release: %v, want StateFresh", state)
	}
}

func TestRelease_OnAbsentKeyIsNoop(t *testing.T) {
	s := newStore()
	if err := s.Release(context.Background(), "absent"); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestSave_OnAbsentKeyIsNoop(t *testing.T) {
	s := newStore()
	if err := s.Save(context.Background(), "absent", &idemkit.Result{StatusCode: 200}); err != nil {
		t.Fatalf("err: %v", err)
	}
	state, _, _ := s.Begin(context.Background(), "absent", []byte{1})
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh (Save on absent must not create an entry)", state)
	}
}

func TestLockTimeout_ExpiredInFlightIsReclaimable(t *testing.T) {
	clock := newFakeClock()
	s := newStoreWithClock(clock)
	ctx := context.Background()
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}

	clock.Advance(31 * time.Second)

	state, _, err := s.Begin(ctx, "k", []byte{2})
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
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}

	clock.Advance(29 * time.Second)

	state, _, err := s.Begin(ctx, "k", []byte{1})
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
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "k", &idemkit.Result{StatusCode: 200}); err != nil {
		t.Fatal(err)
	}

	clock.Advance(2 * time.Hour)

	state, _, err := s.Begin(ctx, "k", []byte{1})
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
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}
	saved := &idemkit.Result{StatusCode: 200}
	if err := s.Save(ctx, "k", saved); err != nil {
		t.Fatal(err)
	}

	clock.Advance(59 * time.Minute)

	state, got, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if state != idemkit.StateDone {
		t.Fatalf("state: %v, want StateDone (TTL still valid)", state)
	}
	if got != saved {
		t.Fatalf("result: %v, want saved", got)
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
			state, _, err := s.Begin(ctx, "k", []byte{1})
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
	if _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
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
	if err := s.Save(ctx, "k", saved); err != nil {
		t.Fatal(err)
	}

	wg.Wait()
	close(results)

	count := 0
	for got := range results {
		count++
		if got != saved {
			t.Errorf("waiter received %v, want saved", got)
		}
	}
	if count != waiters {
		t.Fatalf("waiter count: %d, want %d", count, waiters)
	}
}

func TestNew_ZeroConfigAppliesDefaults(t *testing.T) {
	s := mem.New(mem.Config{})
	ctx := context.Background()
	state, _, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh", state)
	}
	if err := s.Save(ctx, "k", &idemkit.Result{StatusCode: 200}); err != nil {
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
		_, _, _ = s.Begin(ctx, strconv.Itoa(i), hash)
	}
}

func BenchmarkBegin_Done(b *testing.B) {
	s := mem.New(mem.Config{})
	ctx := context.Background()
	hash := make([]byte, 32)
	_, _, _ = s.Begin(ctx, "k", hash)
	_ = s.Save(ctx, "k", &idemkit.Result{StatusCode: 200, Body: []byte("ok")})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = s.Begin(ctx, "k", hash)
	}
}

func BenchmarkBegin_DoneParallel(b *testing.B) {
	s := mem.New(mem.Config{})
	ctx := context.Background()
	hash := make([]byte, 32)
	_, _, _ = s.Begin(ctx, "k", hash)
	_ = s.Save(ctx, "k", &idemkit.Result{StatusCode: 200, Body: []byte("ok")})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _, _ = s.Begin(ctx, "k", hash)
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
		_, _, _ = s.Begin(ctx, key, hash)
		_ = s.Save(ctx, key, res)
	}
}
