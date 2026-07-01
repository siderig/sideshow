package main

import (
	"encoding/json"
	"sync"
	"testing"
)

// Regression for the confirmed data race (batch-1 review #1): status() used to
// alias the live mode.Params map, which setDark mutates in place — so
// JSON-encoding a status snapshot concurrently with a theme toggle was a fatal
// `concurrent map read and map write`. status() now deep-copies Params. Run
// under -race; this must not report a race or crash.
func TestStatusVsSetDarkNoRace(t *testing.T) {
	r := newModeRunner(testSupervisor(t), Mode{
		Type:   ModeWeb,
		Params: map[string]any{"url": "https://example.org", "dark": true},
	})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(d bool) { defer wg.Done(); r.setDark(d) }(i%2 == 0)
		go func() { defer wg.Done(); _, _ = json.Marshal(r.status()) }()
	}
	wg.Wait()
}
