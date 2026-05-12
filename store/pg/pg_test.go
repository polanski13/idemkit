package pg_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/polanski13/idemkit"
	"github.com/polanski13/idemkit/store/pg"
)

var pkgPool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("IDEMKIT_PG_TEST_URL")
	if url == "" {
		os.Exit(m.Run())
	}
	var err error
	pkgPool, err = pgxpool.New(context.Background(), url)
	if err != nil {
		panic(err)
	}
	if err := pg.ApplySchema(context.Background(), pkgPool); err != nil {
		panic(err)
	}
	code := m.Run()
	pkgPool.Close()
	os.Exit(code)
}

func newTestStore(t *testing.T) *pg.Store {
	t.Helper()
	return newTestStoreWithConfig(t, pg.Config{
		TTL:          time.Hour,
		LockTimeout:  30 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
}

func newTestStoreWithConfig(t *testing.T, cfg pg.Config) *pg.Store {
	t.Helper()
	if pkgPool == nil {
		t.Skip("set IDEMKIT_PG_TEST_URL to run pg integration tests")
	}
	if _, err := pkgPool.Exec(context.Background(), "TRUNCATE idemkit_keys"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pg.New(pkgPool, cfg)
}

var _ idemkit.Store = (*pg.Store)(nil)

func TestApplySchema_Idempotent(t *testing.T) {
	if pkgPool == nil {
		t.Skip("set IDEMKIT_PG_TEST_URL to run pg integration tests")
	}
	for i := 0; i < 3; i++ {
		if err := pg.ApplySchema(context.Background(), pkgPool); err != nil {
			t.Fatalf("apply #%d: %v", i, err)
		}
	}
}

func TestBegin_FreshOnFirstCall(t *testing.T) {
	s := newTestStore(t)
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
	s := newTestStore(t)
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
	s := newTestStore(t)
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
	s := newTestStore(t)
	ctx := context.Background()
	_, _, a, _ := s.Begin(ctx, "a", []byte{1})
	_, _, b, _ := s.Begin(ctx, "b", []byte{2})
	_, _, c, _ := s.Begin(ctx, "c", []byte{3})
	if a == 0 || b == 0 || c == 0 {
		t.Fatalf("zero tokens: a=%d b=%d c=%d", a, b, c)
	}
	if a == b || b == c || a == c {
		t.Fatalf("non-unique tokens: a=%d b=%d c=%d", a, b, c)
	}
}

func TestBegin_DoneWithCachedResultOnMatchingHash(t *testing.T) {
	s := newTestStore(t)
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
}

func TestBegin_BodyMismatchOnDone_StillReturnsCachedResult(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})
	saved := &idemkit.Result{StatusCode: 200, Body: []byte("ok")}
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
		t.Fatalf("result: %#v, want %#v", got, saved)
	}
}

func TestWait_ReturnsImmediatelyWhenDone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})
	saved := &idemkit.Result{StatusCode: 200, Body: []byte("ok")}
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
	s := newTestStore(t)
	got, err := s.Wait(context.Background(), "absent")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Fatalf("result: %v, want nil", got)
	}
}

func TestWait_BlocksUntilSave(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})

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
	case <-time.After(80 * time.Millisecond):
	}

	saved := &idemkit.Result{StatusCode: 200, Body: []byte("ok")}
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
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after Save")
	}
}

func TestWait_BlocksUntilReleaseReturnsNil(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})

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
	case <-time.After(80 * time.Millisecond):
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
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after Release")
	}
}

func TestWait_HonorsCtxCancellation(t *testing.T) {
	s := newTestStore(t)
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
	s := newTestStore(t)
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})
	if err := s.Release(ctx, "k", tok); err != nil {
		t.Fatal(err)
	}
	state, _, _, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh", state)
	}
}

func TestRelease_OnAbsentKeyIsNoop(t *testing.T) {
	s := newTestStore(t)
	if err := s.Release(context.Background(), "absent", idemkit.Token(1)); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestRelease_WithMismatchingTokenIsNoop(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})

	if err := s.Release(ctx, "k", tok+1); err != nil {
		t.Fatalf("err: %v", err)
	}

	state, _, _, _ := s.Begin(ctx, "k", []byte{1})
	if state != idemkit.StateInFlight {
		t.Fatalf("state: %v, want StateInFlight", state)
	}
}

func TestSave_OnAbsentKeyReturnsTokenMismatch(t *testing.T) {
	s := newTestStore(t)
	err := s.Save(context.Background(), "absent", idemkit.Token(1), &idemkit.Result{StatusCode: 200})
	if !errors.Is(err, idemkit.ErrTokenMismatch) {
		t.Fatalf("err: %v, want ErrTokenMismatch", err)
	}
	state, _, _, _ := s.Begin(context.Background(), "absent", []byte{1})
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh", state)
	}
}

func TestSave_WithMismatchingTokenReturnsErrTokenMismatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})

	err := s.Save(ctx, "k", tok+1, &idemkit.Result{StatusCode: 200})
	if !errors.Is(err, idemkit.ErrTokenMismatch) {
		t.Fatalf("err: %v, want ErrTokenMismatch", err)
	}

	state, _, _, _ := s.Begin(ctx, "k", []byte{1})
	if state != idemkit.StateInFlight {
		t.Fatalf("state: %v, want StateInFlight", state)
	}
}

func TestSave_AfterReclaimByOtherCallerReturnsErrTokenMismatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, _, tokA, _ := s.Begin(ctx, "k", []byte{1})
	if err := s.Release(ctx, "k", tokA); err != nil {
		t.Fatal(err)
	}
	_, _, tokB, _ := s.Begin(ctx, "k", []byte{2})
	if tokA == tokB {
		t.Fatal("expected distinct tokens after reclaim")
	}

	err := s.Save(ctx, "k", tokA, &idemkit.Result{StatusCode: 200})
	if !errors.Is(err, idemkit.ErrTokenMismatch) {
		t.Fatalf("A's late Save: err=%v, want ErrTokenMismatch", err)
	}

	bResult := &idemkit.Result{StatusCode: 201}
	if err := s.Save(ctx, "k", tokB, bResult); err != nil {
		t.Fatal(err)
	}

	state, got, _, _ := s.Begin(ctx, "k", []byte{2})
	if state != idemkit.StateDone {
		t.Fatalf("state: %v, want StateDone", state)
	}
	if !reflect.DeepEqual(got, bResult) {
		t.Fatalf("result: %#v, want %#v", got, bResult)
	}
}

func TestLockTimeout_ExpiredInFlightIsReclaimable(t *testing.T) {
	s := newTestStoreWithConfig(t, pg.Config{
		TTL:          time.Hour,
		LockTimeout:  1 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
	ctx := context.Background()
	if _, _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1100 * time.Millisecond)

	state, _, _, err := s.Begin(ctx, "k", []byte{2})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh after lock timeout", state)
	}
}

func TestTTL_ExpiredDoneEntryIsReclaimable(t *testing.T) {
	s := newTestStoreWithConfig(t, pg.Config{
		TTL:          1 * time.Second,
		LockTimeout:  30 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})
	if err := s.Save(ctx, "k", tok, &idemkit.Result{StatusCode: 200}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1100 * time.Millisecond)

	state, _, _, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh after TTL expiry", state)
	}
}

func TestConcurrentClaim_ExactlyOneSeesFresh(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const goroutines = 20

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
	s := newTestStore(t)
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})

	const waiters = 10
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

	saved := &idemkit.Result{StatusCode: 200, Body: []byte("ok")}
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
