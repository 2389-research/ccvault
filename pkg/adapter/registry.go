// ABOUTME: Global registry for source adapters, allowing adapters to self-register by name.
// ABOUTME: Thread-safe via sync.RWMutex; adapters register factories that are instantiated on Get.

package adapter

import (
	"fmt"
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

// resetRegistry clears all registered adapters. Used only in tests.
func resetRegistry() {
	mu.Lock()
	defer mu.Unlock()
	factories = make(map[string]AdapterFactory)
}
