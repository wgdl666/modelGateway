package store

import (
	"sync"
	"testing"
)

// 高频提交只需一次唤醒，通知不能阻塞已提交请求；消费后仍能收到后续通知。
func TestPublicationWakeCoalescesWithoutBlocking(t *testing.T) {
	s := New(nil)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(s.notifyPublication)
	}
	wg.Wait()
	select {
	case <-s.PublicationWake():
	default:
		t.Fatal("missing publication wake")
	}
	select {
	case <-s.PublicationWake():
		t.Fatal("notifications were not coalesced")
	default:
	}
	s.notifyPublication()
	select {
	case <-s.PublicationWake():
	default:
		t.Fatal("missing subsequent publication wake")
	}
}
