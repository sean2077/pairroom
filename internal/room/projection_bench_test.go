package room

import (
	"fmt"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

// Keep payload shape and the transport window fixed as durable history grows.
// Report allocations rather than gating on machine-dependent wall-clock time.
func BenchmarkWindowedSnapshot(b *testing.B) {
	for _, count := range []int{250, 10000} {
		b.Run(fmt.Sprintf("messages_%d", count), func(b *testing.B) {
			engine := projectionBenchmarkEngine(count)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = engine.WindowedSnapshot(250)
			}
		})
	}
}

func projectionBenchmarkEngine(count int) *Engine {
	engine := &Engine{}
	engine.snapshot.Messages = make([]model.Message, count)
	for i := range engine.snapshot.Messages {
		engine.snapshot.Messages[i] = model.Message{
			Seq: uint64(i + 1), ID: fmt.Sprintf("message-%d", i), Text: "A complete visible response; never truncate relay text.",
			To:         []model.ActorID{model.ActorCodex},
			Delivery:   map[model.ActorID]model.DeliveryState{model.ActorCodex: model.DeliveryQueued},
			Processing: map[model.ActorID]model.ProcessingState{model.ActorCodex: model.ProcessingCompleted},
		}
	}
	return engine
}
