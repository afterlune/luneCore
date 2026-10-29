package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Include realistic transcript sizes: usage writes must not decode or remarshal
// the canonical transcript just to increment accounting fields. Use a fixed
// iteration count (e.g. -benchtime=20x) when comparing initial record counts.
func BenchmarkRecordUsageByHistorySize(b *testing.B) {
	for _, size := range []int{1024, 64 * 1024, 1024 * 1024} {
		for _, initialRecords := range []int{0, 20, 200} {
			b.Run(fmt.Sprintf("history=%d/initial-records=%d", size, initialRecords), func(b *testing.B) {
				client, err := New(runtimeTestConfig(b.TempDir(), b.TempDir(), &scriptedHostProvider{}))
				if err != nil {
					b.Fatal(err)
				}
				defer client.Close(context.Background())
				history, _ := json.Marshal([]map[string]string{{"type": "message", "role": "user", "content": strings.Repeat("x", size)}})
				saved, err := client.CreateSession(context.Background(), history)
				if err != nil {
					b.Fatal(err)
				}
				for range initialRecords {
					if err := client.recordUsage(context.Background(), saved.ID, Usage{InputTokens: 10, OutputTokens: 1}); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if err := client.recordUsage(context.Background(), saved.ID, Usage{InputTokens: 10, OutputTokens: 1}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
