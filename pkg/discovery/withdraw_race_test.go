package discovery

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// A publish stores its record and then decides what the live map should name in
// a second critical section, and a Withdraw is a third. A publish whose store
// has landed can therefore be judged stale and Forget the bytes a second
// publish has already stored, while the second publish only assigns the live
// map and writes nothing back — Announced() is then true while Resolve misses.
//
// The race shape has to be driven, not waited for: the withdrawal has to land
// inside the first publish's store, and the second publish's store has to land
// between the first publish's store and its Forget, before that call assigns the
// live entry. Every round starts all three writers at once, and a monitor
// goroutine reads the live map and resolves the name with the writers paused:
// the moment the map names an address the DHT does not hold, the round failed.
// The check runs between rounds because it is two reads — the map, then the
// DHT — and a check that overlapped a withdrawal would see the record vanish
// between them and report a disagreement that no reader ever saw.
func TestAWithdrawRacingAPublishLeavesTheLiveMapAndTheDHTAgreeing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	node := startNode(t, Config{ListenAddr: freeUDPAddr(t), AnnounceTTL: time.Minute})
	const name = "web"

	// The monitor owns the invariant check. A round hands it the paused state
	// and takes back the verdict, so the failure is reported by the goroutine
	// that observed it.
	type verdict struct {
		consistent bool
		msg        string
	}
	requests := make(chan struct{})
	answers := make(chan verdict)
	var monitor sync.WaitGroup
	monitor.Add(1)
	go func() {
		defer monitor.Done()
		for range requests {
			node.mu.Lock()
			rec, live := node.live[name]
			node.mu.Unlock()
			if !live {
				answers <- verdict{consistent: true}
				continue
			}
			// The live record is what every later refresh republishes, so a
			// reader that resolves the name must get those same bytes.
			resolved, err := node.Resolve(name)
			switch {
			case err != nil:
				answers <- verdict{msg: fmt.Sprintf("the live record is %q but Resolve failed: %v", rec.Server, err)}
			case resolved.Server != rec.Server:
				answers <- verdict{msg: fmt.Sprintf("the live record is %q but the DHT holds %q", rec.Server, resolved.Server)}
			default:
				answers <- verdict{consistent: true}
			}
		}
	}()

	const rounds = 20000
	failures := make(chan string, 1)
	done := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		defer close(requests)
		defer close(done)
		servers := [2]string{"10.0.0.1:7001", "10.0.0.2:7001"}
		for i := 0; i < rounds; i++ {
			if ctx.Err() != nil {
				return
			}
			start := make(chan struct{})
			var round sync.WaitGroup
			run := func(fn func() error) {
				round.Add(1)
				go func() {
					defer round.Done()
					<-start
					if err := fn(); err != nil && ctx.Err() == nil {
						t.Errorf("round %d: %v", i, err)
					}
				}()
			}
			run(func() error { return node.Withdraw(name) })
			for _, server := range servers {
				run(func() error { return node.Publish(ctx, sampleRecord(name, server)) })
			}
			close(start)
			round.Wait()

			requests <- struct{}{}
			if got := <-answers; !got.consistent {
				failures <- fmt.Sprintf("round %d: %s", i, got.msg)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
	writers.Wait()
	cancel()
	monitor.Wait()

	select {
	case msg := <-failures:
		t.Fatal(msg)
	default:
	}
}
