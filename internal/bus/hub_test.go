package bus

import (
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestSlowSubscriberClosesWithoutLosingBufferedPrefix(t *testing.T) {
	hub := New(1)
	slow, cancelSlow := hub.Subscribe()
	defer cancelSlow()
	fast, cancelFast := hub.Subscribe()
	defer cancelFast()
	hub.Publish(model.Event{Seq: 1})
	if got := <-fast; got.Seq != 1 {
		t.Fatalf("fast subscriber: %+v", got)
	}
	hub.Publish(model.Event{Seq: 2})
	if got := <-fast; got.Seq != 2 {
		t.Fatalf("slow subscriber blocked fast subscriber: %+v", got)
	}
	if got := <-slow; got.Seq != 1 {
		t.Fatalf("buffered prefix lost: %+v", got)
	}
	if _, open := <-slow; open {
		t.Fatal("overflowed subscriber must close so SSE can reconnect")
	}
	cancelSlow() // cancellation after eviction is idempotent
}

func TestConcurrentPublishAndCancel(t *testing.T) {
	hub := New(1)
	var work sync.WaitGroup
	for i := 0; i < 32; i++ {
		stream, cancel := hub.Subscribe()
		work.Add(2)
		go func() {
			defer work.Done()
			for i := 0; i < 100; i++ {
				hub.Publish(model.Event{Seq: uint64(i + 1)})
			}
		}()
		go func() {
			defer work.Done()
			cancel()
			cancel()
			for range stream {
			}
		}()
	}
	work.Wait()
	if len(hub.subscribers) != 0 {
		t.Fatal("cancelled subscribers retained")
	}
}
