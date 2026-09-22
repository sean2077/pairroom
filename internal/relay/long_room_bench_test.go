package relay

import (
	"fmt"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

// Terminal history is populated without disk I/O to isolate projection cost.
// Live publication, fsync and vendor inference are deliberately not measured.
func BenchmarkNativeLongRoom(b *testing.B) {
	for _, size := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			log, err := store.Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			e, err := Open(Config{RoomID: "bench", Store: log, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot2: model.RuntimeCodex}})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = e.Close() })
			binding, err := e.Bind(model.ActorSlot2, BindRequest{BindID: "bench-bind", CredentialHash: Digest("bench-secret"), SessionID: "bench-session"})
			if err != nil {
				b.Fatal(err)
			}
			auth := Auth{Slot: model.ActorSlot2, BindID: binding.BindID, Generation: binding.Generation, SessionID: binding.SessionID, Secret: "bench-secret"}
			for i := 0; i < size; i++ {
				e.putMessage(Message{ID: fmt.Sprint(i), To: model.ActorSlot2, State: "handed_off", TargetGeneration: 1, CreatedAt: time.Now()})
			}
			e.putMessage(Message{ID: "queued", To: model.ActorSlot2, State: "queued", TargetGeneration: 1, CreatedAt: time.Now()})
			b.Run("summary", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := e.AuthSummary(auth); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("wake", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					e.WakeCandidate("queued")
				}
			})
		})
	}
}
