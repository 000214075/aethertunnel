package discovery

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// The check that a publish is not restoring a superseded value and the
// assignment of the value to the live map used to sit in two critical sections.
// Two Publishes of one name could therefore each read a live entry that neither
// had installed yet, each find themselves not superseded by it, and each assign:
// the one whose store landed first could assign last, so the map named one
// address while the DHT held the other — and every later refresh, which reads the
// map, republished the loser. The window is a few instructions wide, so the two
// writes run together for many rounds and every round has to end with the map and
// the stored bytes naming one address.
func TestConcurrentPublishesOfOneNameLeaveTheLiveMapAndTheDHTAgreeing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	node := startNode(t, Config{ListenAddr: freeUDPAddr(t), AnnounceTTL: time.Minute})

	// The two racers have to read a record that is already live for the window to
	// open at all: with nothing live both take the "expected nothing" path, whose
	// restore branch keeps them agreeing.
	const rounds = 20000
	for i := 0; i < rounds; i++ {
		if err := node.Publish(ctx, sampleRecord("web", "10.0.0.9:7001")); err != nil {
			t.Fatalf("round %d: Publish the record both racers read: %v", i, err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for g := 0; g < 2; g++ {
			server := fmt.Sprintf("10.0.%d.1:7001", g+1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- node.Publish(ctx, sampleRecord("web", server))
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: Publish: %v", i, err)
			}
		}

		node.mu.Lock()
		live := node.live["web"].Server
		node.mu.Unlock()
		rec, err := node.Resolve("web")
		if err != nil {
			t.Fatalf("round %d: Resolve: %v", i, err)
		}
		if rec.Server != live {
			t.Fatalf("round %d: the live map names %q while the DHT holds %q", i, live, rec.Server)
		}
	}
}
