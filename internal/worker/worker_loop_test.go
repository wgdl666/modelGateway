package worker

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// 空闲等待跨过 ticker 周期后应恢复正常节奏，且停机不能等待整个空闲周期。
func TestRunWorkerIdleWaitAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := &Reconciler{ctx: ctx}
		var calls []time.Time
		w.runWorker("test", 500*time.Millisecond, func() error {
			calls = append(calls, time.Now())
			if len(calls) == 1 || len(calls) == 4 {
				select {
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
				}
			}
			return nil
		})
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if len(calls) != 4 {
			t.Fatalf("got %d calls, want 4", len(calls))
		}
		for i, want := range []time.Duration{2 * time.Second, 500 * time.Millisecond, 500 * time.Millisecond} {
			if got := calls[i+1].Sub(calls[i]); got != want {
				t.Fatalf("call gap %d = %s, want %s", i, got, want)
			}
		}
		stopped := time.Now()
		cancel()
		w.wg.Wait()
		if time.Since(stopped) != 0 {
			t.Fatal("cancellation waited for idle timer")
		}
	})
}
