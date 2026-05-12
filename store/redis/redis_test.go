package redis_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/polanski13/idemkit"
	"github.com/polanski13/idemkit/store/redis"
)

var pkgClient *goredis.Client

func TestMain(m *testing.M) {
	url := os.Getenv("IDEMKIT_REDIS_TEST_URL")
	if url == "" {
		os.Exit(m.Run())
	}
	opts, err := goredis.ParseURL(url)
	if err != nil {
		panic(err)
	}
	pkgClient = goredis.NewClient(opts)
	if err := pkgClient.Ping(context.Background()).Err(); err != nil {
		panic(err)
	}
	code := m.Run()
	pkgClient.Close()
	os.Exit(code)
}

func newTestStore(t *testing.T) *redis.Store {
	t.Helper()
	return newTestStoreWithConfig(t, redis.Config{
		TTL:          time.Hour,
		LockTimeout:  30 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
}

func newTestStoreWithConfig(t *testing.T, cfg redis.Config) *redis.Store {
	t.Helper()
	if pkgClient == nil {
		t.Skip("set IDEMKIT_REDIS_TEST_URL to run redis integration tests")
	}
	if err := pkgClient.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flushdb: %v", err)
	}
	return redis.New(pkgClient, cfg)
}

var _ idemkit.Store = (*redis.Store)(nil)

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
	s := newTestStoreWithConfig(t, redis.Config{
		TTL:          time.Hour,
		LockTimeout:  1 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
	ctx := context.Background()
	if _, _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)

	state, _, _, err := s.Begin(ctx, "k", []byte{2})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh after lock timeout", state)
	}
}

func TestTTL_ExpiredDoneEntryIsReclaimable(t *testing.T) {
	s := newTestStoreWithConfig(t, redis.Config{
		TTL:          1 * time.Second,
		LockTimeout:  30 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})
	if err := s.Save(ctx, "k", tok, &idemkit.Result{StatusCode: 200}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)

	state, _, _, err := s.Begin(ctx, "k", []byte{1})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if state != idemkit.StateFresh {
		t.Fatalf("state: %v, want StateFresh after TTL expiry", state)
	}
}

func TestKeyPrefix_NamespacesStorage(t *testing.T) {
	s := newTestStoreWithConfig(t, redis.Config{
		TTL:          time.Hour,
		LockTimeout:  30 * time.Second,
		PollInterval: 10 * time.Millisecond,
		KeyPrefix:    "mytenant:",
	})
	ctx := context.Background()
	if _, _, _, err := s.Begin(ctx, "k", []byte{1}); err != nil {
		t.Fatal(err)
	}

	exists, err := pkgClient.Exists(ctx, "mytenant:k").Result()
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if exists != 1 {
		t.Fatalf("key under prefix not found: exists=%d", exists)
	}

	if v, err := pkgClient.Exists(ctx, "k").Result(); err != nil || v != 0 {
		t.Fatalf("bare key should not exist: v=%d err=%v", v, err)
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

func newPubSubStore(t *testing.T, pollInterval time.Duration) *redis.Store {
	t.Helper()
	if pkgClient == nil {
		t.Skip("set IDEMKIT_REDIS_TEST_URL to run redis integration tests")
	}
	if err := pkgClient.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flushdb: %v", err)
	}
	s := redis.New(pkgClient, redis.Config{
		TTL:          time.Hour,
		LockTimeout:  30 * time.Second,
		PollInterval: pollInterval,
		PubSub:       true,
	})
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestPubSub_WakesWaiterOnSave(t *testing.T) {
	s := newPubSubStore(t, 5*time.Second)
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})

	type out struct {
		res *idemkit.Result
		err error
		dur time.Duration
	}
	done := make(chan out, 1)
	go func() {
		started := time.Now()
		res, err := s.Wait(ctx, "k")
		done <- out{res, err, time.Since(started)}
	}()

	time.Sleep(50 * time.Millisecond)

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
		if got.dur >= 500*time.Millisecond {
			t.Fatalf("Wait took %v, expected pub/sub-driven wakeup well under PollInterval (5s)", got.dur)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after Save")
	}
}

func TestPubSub_WakesWaiterOnRelease(t *testing.T) {
	s := newPubSubStore(t, 5*time.Second)
	ctx := context.Background()
	_, _, tok, _ := s.Begin(ctx, "k", []byte{1})

	type out struct {
		res *idemkit.Result
		err error
		dur time.Duration
	}
	done := make(chan out, 1)
	go func() {
		started := time.Now()
		res, err := s.Wait(ctx, "k")
		done <- out{res, err, time.Since(started)}
	}()

	time.Sleep(50 * time.Millisecond)

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
		if got.dur >= 500*time.Millisecond {
			t.Fatalf("Wait took %v, expected pub/sub-driven wakeup well under PollInterval (5s)", got.dur)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after Release")
	}
}

func TestPubSub_KeyPrefixIsolatesChannels(t *testing.T) {
	if pkgClient == nil {
		t.Skip("set IDEMKIT_REDIS_TEST_URL to run redis integration tests")
	}
	if err := pkgClient.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flushdb: %v", err)
	}
	cfgBase := redis.Config{
		TTL:          time.Hour,
		LockTimeout:  30 * time.Second,
		PollInterval: 5 * time.Second,
		PubSub:       true,
	}
	cfgA := cfgBase
	cfgA.KeyPrefix = "tenantA:"
	cfgB := cfgBase
	cfgB.KeyPrefix = "tenantB:"
	sA := redis.New(pkgClient, cfgA)
	sB := redis.New(pkgClient, cfgB)
	t.Cleanup(func() { _ = sA.Close(); _ = sB.Close() })

	ctx := context.Background()
	_, _, tokA, _ := sA.Begin(ctx, "k", []byte{1})
	_, _, _, _ = sB.Begin(ctx, "k", []byte{1})

	doneB := make(chan struct{}, 1)
	go func() {
		_, _ = sB.Wait(ctx, "k")
		doneB <- struct{}{}
	}()

	time.Sleep(80 * time.Millisecond)

	saved := &idemkit.Result{StatusCode: 200, Body: []byte("ok")}
	if err := sA.Save(ctx, "k", tokA, saved); err != nil {
		t.Fatal(err)
	}

	select {
	case <-doneB:
		t.Fatal("sB.Wait woke up on sA's publish — channels not isolated by KeyPrefix")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestPubSub_CloseIsIdempotent(t *testing.T) {
	if pkgClient == nil {
		t.Skip("set IDEMKIT_REDIS_TEST_URL to run redis integration tests")
	}
	if err := pkgClient.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flushdb: %v", err)
	}
	s := redis.New(pkgClient, redis.Config{PubSub: true})
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPubSub_CloseWithoutPubSubIsNoop(t *testing.T) {
	if pkgClient == nil {
		t.Skip("set IDEMKIT_REDIS_TEST_URL to run redis integration tests")
	}
	s := redis.New(pkgClient, redis.Config{})
	if err := s.Close(); err != nil {
		t.Fatalf("Close on non-pubsub store: %v", err)
	}
}

func TestPubSub_WaitHonorsCtxCancellation(t *testing.T) {
	s := newPubSubStore(t, 5*time.Second)
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

func TestPubSub_ConcurrentWaitersAllWake(t *testing.T) {
	s := newPubSubStore(t, 5*time.Second)
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

	time.Sleep(80 * time.Millisecond)

	saved := &idemkit.Result{StatusCode: 200, Body: []byte("ok")}
	started := time.Now()
	if err := s.Save(ctx, "k", tok, saved); err != nil {
		t.Fatal(err)
	}

	wg.Wait()
	elapsed := time.Since(started)
	close(results)

	if elapsed >= 500*time.Millisecond {
		t.Fatalf("waiters took %v to wake, expected pub/sub-driven wakeup well under PollInterval (5s)", elapsed)
	}

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

func newBenchStore(b *testing.B) *redis.Store {
	b.Helper()
	if pkgClient == nil {
		b.Skip("set IDEMKIT_REDIS_TEST_URL to run redis benchmarks")
	}
	if err := pkgClient.FlushDB(context.Background()).Err(); err != nil {
		b.Fatalf("flushdb: %v", err)
	}
	return redis.New(pkgClient, redis.Config{
		TTL:          time.Hour,
		LockTimeout:  30 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
}

func BenchmarkRedis_Begin_Fresh(b *testing.B) {
	s := newBenchStore(b)
	ctx := context.Background()
	hash := make([]byte, 32)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _, _ = s.Begin(ctx, strconv.Itoa(i), hash)
	}
}

func BenchmarkRedis_Begin_Done(b *testing.B) {
	s := newBenchStore(b)
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

func BenchmarkRedis_Begin_Save_Roundtrip(b *testing.B) {
	s := newBenchStore(b)
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
