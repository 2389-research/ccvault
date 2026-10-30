// ABOUTME: Global registry for source adapters, allowing adapters to self-register by name.
// ABOUTME: Thread-safe via sync.RWMutex; adapters register factories that are instantiated on Get.

package adapter

import (
	"fmt"
	"sort"
	"sync"
)

// AdapterFactory is a constructor function that creates a new SourceAdapter instance.
type AdapterFactory func() SourceAdapter

var (
	mu        sync.RWMutex
	factories = make(map[string]AdapterFactory)
)

// Register adds an adapter factory under the given type name.
// Subsequent calls to Get with the same name will use this factory.
func Register(typeName string, factory AdapterFactory) {
	mu.Lock()
	defer mu.Unlock()
	factories[typeName] = factory
}

// Get creates and returns a new SourceAdapter for the given type name.
// Returns an error if no adapter has been registered with that name.
func Get(typeName string) (SourceAdapter, error) {
	mu.RLock()
	defer mu.RUnlock()
	factory, ok := factories[typeName]
	if !ok {
		return nil, fmt.Errorf("unknown adapter type: %q", typeName)
	}
	return factory(), nil
}

// RegisteredNames returns the type names of every registered adapter, sorted.
//
// It exists so a test can say something about *every* source rather than about
// a list of sources somebody remembered to extend. The reparser table in
// test/integration is the case that needed it: being absent from that table
// used to mean being unchecked, and the thing it checks — that a source
// recording token usage can have those counters repaired — is exactly the kind
// of thing a new source gets wrong.
func RegisteredNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resetRegistry clears all registered adapters. Used only in tests.
func resetRegistry() {
	mu.Lock()
	defer mu.Unlock()
	factories = make(map[string]AdapterFactory)
}
