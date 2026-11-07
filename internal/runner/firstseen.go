package runner

import (
	"sync"
	"time"
)

// FirstSeen tracks, in memory, since when a condition has been continuously
// observed, for "persistent for" thresholds. After a restart nothing is known,
// so a threshold never trips before it has been observed for its full length;
// Observed reports how long the process has been watching.
type FirstSeen struct {
	mu      sync.Mutex
	started time.Time
	since   map[string]time.Time
	touched map[string]bool
}

func NewFirstSeen() *FirstSeen {
	return &FirstSeen{since: map[string]time.Time{}, touched: map[string]bool{}}
}

// Begin starts one sample. Keys not observed active before End are forgotten.
func (f *FirstSeen) Begin(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started.IsZero() {
		f.started = now
	}
	f.touched = map[string]bool{}
}

// Active records whether the condition key holds in this sample and returns
// how long it has held continuously (zero when it does not hold).
func (f *FirstSeen) Active(key string, active bool, now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !active {
		delete(f.since, key)
		return 0
	}
	f.touched[key] = true
	since, ok := f.since[key]
	if !ok {
		f.since[key] = now
		return 0
	}
	return now.Sub(since)
}

// End forgets conditions that were not observed in this sample.
func (f *FirstSeen) End() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.since {
		if !f.touched[key] {
			delete(f.since, key)
		}
	}
}

// Observed is how long this process has been sampling.
func (f *FirstSeen) Observed(now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started.IsZero() {
		return 0
	}
	return now.Sub(f.started)
}
