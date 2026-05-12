package pg

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
)

type listener struct {
	conn    *pgx.Conn
	cancel  context.CancelFunc
	mu      sync.Mutex
	waiters map[string][]chan struct{}
	done    chan struct{}
}

func newListener(startCtx context.Context, conn *pgx.Conn, channel string) (*listener, error) {
	if _, err := conn.Exec(startCtx, "LISTEN "+channel); err != nil {
		return nil, fmt.Errorf("idemkit/pg: LISTEN %s: %w", channel, err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	l := &listener{
		conn:    conn,
		cancel:  cancel,
		waiters: make(map[string][]chan struct{}),
		done:    make(chan struct{}),
	}
	go l.run(runCtx)
	return l, nil
}

func (l *listener) run(ctx context.Context) {
	defer close(l.done)
	for {
		notif, err := l.conn.WaitForNotification(ctx)
		if err != nil {
			return
		}
		l.notify(notif.Payload)
	}
}

func (l *listener) register(key string) chan struct{} {
	c := make(chan struct{}, 1)
	l.mu.Lock()
	l.waiters[key] = append(l.waiters[key], c)
	l.mu.Unlock()
	return c
}

func (l *listener) unregister(key string, c chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	chans := l.waiters[key]
	for i, x := range chans {
		if x == c {
			l.waiters[key] = append(chans[:i], chans[i+1:]...)
			break
		}
	}
	if len(l.waiters[key]) == 0 {
		delete(l.waiters, key)
	}
}

func (l *listener) notify(key string) {
	l.mu.Lock()
	chans := make([]chan struct{}, len(l.waiters[key]))
	copy(chans, l.waiters[key])
	l.mu.Unlock()
	for _, c := range chans {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (l *listener) close() error {
	l.cancel()
	<-l.done
	return nil
}
