package github

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *ghStore {
	t.Helper()
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, ok, err := s.GetSnapshot(ctx, "c1"); err != nil || ok {
		t.Fatalf("GetSnapshot on empty store: ok=%v err=%v, want ok=false", ok, err)
	}
	if err := s.SetSnapshot(ctx, "c1", `{"state":"open"}`); err != nil {
		t.Fatalf("SetSnapshot: %v", err)
	}
	if j, ok, err := s.GetSnapshot(ctx, "c1"); err != nil || !ok || j != `{"state":"open"}` {
		t.Fatalf("GetSnapshot = (%q, %v, %v), want the stored JSON", j, ok, err)
	}

	if err := s.SetReviewBaseline(ctx, "c1", `["a","b"]`); err != nil {
		t.Fatalf("SetReviewBaseline: %v", err)
	}
	if p, ok, err := s.GetReviewBaseline(ctx, "c1"); err != nil || !ok || p != `["a","b"]` {
		t.Fatalf("GetReviewBaseline = (%q, %v, %v)", p, ok, err)
	}

	if err := s.SetFixState(ctx, FixState{ChatID: "c1", LastSHA: "abc123", Stopped: false}); err != nil {
		t.Fatalf("SetFixState: %v", err)
	}
	fs, err := s.GetFixState(ctx, "c1")
	if err != nil || fs == nil || fs.LastSHA != "abc123" || fs.Stopped {
		t.Fatalf("GetFixState = %+v, err=%v", fs, err)
	}
	if err := s.DeleteFixState(ctx, "c1"); err != nil {
		t.Fatalf("DeleteFixState: %v", err)
	}
	if fs, err := s.GetFixState(ctx, "c1"); err != nil || fs != nil {
		t.Fatalf("GetFixState after delete = %+v, err=%v, want nil", fs, err)
	}

	if err := s.SetMergeIntent(ctx, "c1", "alice"); err != nil {
		t.Fatalf("SetMergeIntent: %v", err)
	}
	mi, err := s.GetMergeIntent(ctx, "c1")
	if err != nil || mi == nil || mi.RequestedBy != "alice" {
		t.Fatalf("GetMergeIntent = %+v, err=%v", mi, err)
	}
	if err := s.DeleteMergeIntent(ctx, "c1"); err != nil {
		t.Fatalf("DeleteMergeIntent: %v", err)
	}
	if mi, err := s.GetMergeIntent(ctx, "c1"); err != nil || mi != nil {
		t.Fatalf("GetMergeIntent after delete = %+v, err=%v, want nil", mi, err)
	}
}

// TestStoreConcurrentAccessNoErrors is Risk 2's baseline: many goroutines
// hitting all four tables, many keys, concurrently. Run with -race. The
// property under test is that MaxOpenConns(1) actually prevents SQLITE_BUSY
// under concurrent writers - a failure here means every "best effort,
// log-and-continue" caller in webhook.go/cifix.go would silently drop
// writes under load, not that anything panics.
func TestStoreConcurrentAccessNoErrors(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const goroutines = 20
	const opsPerGoroutine = 25
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*opsPerGoroutine)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				chatID := fmt.Sprintf("chat-%d", g%5) // keys collide across goroutines on purpose
				if err := s.SetSnapshot(ctx, chatID, fmt.Sprintf(`{"n":%d}`, i)); err != nil {
					errs <- fmt.Errorf("SetSnapshot: %w", err)
				}
				if _, _, err := s.GetSnapshot(ctx, chatID); err != nil {
					errs <- fmt.Errorf("GetSnapshot: %w", err)
				}
				if err := s.SetMergeIntent(ctx, chatID, "bot"); err != nil {
					errs <- fmt.Errorf("SetMergeIntent: %w", err)
				}
				if _, err := s.GetMergeIntent(ctx, chatID); err != nil {
					errs <- fmt.Errorf("GetMergeIntent: %w", err)
				}
				if err := s.DeleteMergeIntent(ctx, chatID); err != nil {
					errs <- fmt.Errorf("DeleteMergeIntent: %w", err)
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestKeyedMutexPreventsDoubleConsumeMergeIntent reproduces the exact race
// the design doc's Risk 2 names: mergeIfApproved (Set) racing
// tryMergeStandingIntent (Get-then-Delete) for the SAME chat. Without
// serializing the two, two concurrent "Get, see an intent, Delete it"
// sequences can both observe the same intent and both act on it (a double
// merge attempt). keyedMutex closes this at the call-site: every
// consume-if-present pass for one chat is serialized against every set for
// that chat.
func TestKeyedMutexPreventsDoubleConsumeMergeIntent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	var km keyedMutex
	const chatID = "github-acme-widgets-42"

	const rounds = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	consumed := map[string]int{} // requestedBy nonce -> times observed as non-nil by a consumer

	// setter: stands up a fresh intent each round.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			nonce := fmt.Sprintf("nonce-%d", i)
			unlock := km.Lock(chatID)
			err := s.SetMergeIntent(ctx, chatID, nonce)
			unlock()
			if err != nil {
				t.Errorf("SetMergeIntent: %v", err)
			}
		}
	}()

	// two concurrent consumers: mimics mergeIfApproved and
	// tryMergeStandingIntent both potentially firing for the same PR.
	for c := 0; c < 2; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				unlock := km.Lock(chatID)
				mi, err := s.GetMergeIntent(ctx, chatID)
				if err != nil {
					unlock()
					t.Errorf("GetMergeIntent: %v", err)
					continue
				}
				if mi != nil {
					if err := s.DeleteMergeIntent(ctx, chatID); err != nil {
						t.Errorf("DeleteMergeIntent: %v", err)
					}
				}
				unlock()
				if mi != nil {
					mu.Lock()
					consumed[mi.RequestedBy]++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	for nonce, n := range consumed {
		if n > 1 {
			t.Errorf("nonce %q consumed %d times, want at most once (double-consume race not closed)", nonce, n)
		}
	}
}
