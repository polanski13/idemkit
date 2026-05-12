package redis

import (
	"context"
	"sync"

	goredis "github.com/redis/go-redis/v9"
)

type subscriber struct {
	pubsub  *goredis.PubSub
	mu      sync.Mutex
	waiters map[string][]chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

func newSubscriber(client goredis.UniversalClient, channel string) *subscriber {
	s := &subscriber{
		pubsub:  client.Subscribe(context.Background(), channel),
		waiters: make(map[string][]chan struct{}),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *subscriber) run() {
	defer close(s.done)
	ch := s.pubsub.Channel()
	for {
		select {
		case <-s.stop:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			s.notify(msg.Payload)
		}
	}
}

func (s *subscriber) register(key string) chan struct{} {
	c := make(chan struct{}, 1)
	s.mu.Lock()
	s.waiters[key] = append(s.waiters[key], c)
	s.mu.Unlock()
	return c
}

func (s *subscriber) unregister(key string, c chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chans := s.waiters[key]
	for i, x := range chans {
		if x == c {
			s.waiters[key] = append(chans[:i], chans[i+1:]...)
			break
		}
	}
	if len(s.waiters[key]) == 0 {
		delete(s.waiters, key)
	}
}

func (s *subscriber) notify(key string) {
	s.mu.Lock()
	chans := make([]chan struct{}, len(s.waiters[key]))
	copy(chans, s.waiters[key])
	s.mu.Unlock()
	for _, c := range chans {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (s *subscriber) close() error {
	close(s.stop)
	err := s.pubsub.Close()
	<-s.done
	return err
}
