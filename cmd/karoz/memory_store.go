package main

// memoryStore owns durable Agent memory entries. It is separate from the
// conversation event stream because memories are curated semantic records,
// not a projection of every session event.
type memoryStore struct {
	entries map[string][]AgentMemoryEntry
}

func newMemoryStore() *memoryStore {
	return &memoryStore{entries: map[string][]AgentMemoryEntry{}}
}

func (a *app) memoryStoreLocked() *memoryStore {
	if a.memoryStore == nil {
		a.memoryStore = newMemoryStore()
	}
	return a.memoryStore
}
