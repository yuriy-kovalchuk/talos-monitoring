package collector

import (
	"fmt"
	"sort"
	"sync"
)

// Registry holds the known collectors and which of them are enabled.
type Registry struct {
	mu      sync.RWMutex
	byName  map[string]Collector
	enabled map[string]bool
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byName:  make(map[string]Collector),
		enabled: make(map[string]bool),
	}
}

// Register adds a collector (enabled by default). Duplicate names are an
// error.
func (r *Registry) Register(c Collector) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[c.Name()]; exists {
		return fmt.Errorf("collector %q already registered", c.Name())
	}
	r.byName[c.Name()] = c
	r.enabled[c.Name()] = true
	return nil
}

// SetEnabled selects the enabled set from per-name decisions. A missing name
// (or a nil map) stays enabled — the default; an explicit false takes the
// collector out of the scrape loop while it remains registered, so its state
// is still pruned. Names that were never registered are skipped.
func (r *Registry) SetEnabled(enabled map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.enabled = make(map[string]bool, len(r.byName))
	for name := range r.byName {
		if on, ok := enabled[name]; ok && !on {
			continue
		}
		r.enabled[name] = true
	}
}

// Enabled returns the enabled collectors, sorted by name.
func (r *Registry) Enabled() []Collector {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Collector
	for name, c := range r.byName {
		if r.enabled[name] {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Names returns all registered collector names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.namesLocked()
}

func (r *Registry) namesLocked() []string {
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// All returns every registered collector, enabled or not. Pruning must reach
// a disabled collector too: it may hold state from before it was disabled.
func (r *Registry) All() []Collector {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Collector, 0, len(r.byName))
	for _, c := range r.byName {
		out = append(out, c)
	}
	return out
}
