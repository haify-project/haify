package controller

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// A delete and a detach of the same resource run one after the other, and the
// second sees what the first left.
func TestResourceLocksSerialiseOneResource(t *testing.T) {
	var l resourceLocks
	var order []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	unlock := l.lock("vol")
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer l.lock("vol")()
		note("detach")
	}()
	time.Sleep(20 * time.Millisecond)
	note("delete")
	unlock()
	<-done

	assert.Equal(t, []string{"delete", "detach"}, order, "the detach waits for the delete")
	assert.Empty(t, l.locks, "a lock no one holds is dropped")
}

func TestResourceLocksLeaveOtherResourcesAlone(t *testing.T) {
	var l resourceLocks
	unlock := l.lock("a")
	defer unlock()
	got := make(chan struct{})
	go func() { l.lock("b")(); close(got) }()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("a lock on one resource blocked another")
	}
}
