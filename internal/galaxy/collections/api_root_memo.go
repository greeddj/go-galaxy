package collections

import "sync"

// apiRootMemo records, per server base URL, the API root that last answered
// a root-metadata request, so later collections skip the losing variants.
// It lives in one collectionDeps and is never persisted to the snapshot.
type apiRootMemo struct {
	winners map[string]string // server base -> winning apiRoot
	mu      sync.RWMutex
}

// newAPIRootMemo returns an empty memo. The map is presized to 1: a single
// Galaxy server for the whole run is the overwhelmingly common case.
func newAPIRootMemo() *apiRootMemo {
	return &apiRootMemo{winners: make(map[string]string, 1)}
}

// winner returns the recorded winning apiRoot for base, if any. A nil
// receiver reports no winner, so a collectionDeps built without
// newCollectionDeps can pass deps.apiRoots through unconditionally.
func (m *apiRootMemo) winner(base string) (string, bool) {
	if m == nil {
		return "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	apiRoot, ok := m.winners[base]
	return apiRoot, ok
}

// recordWinner records apiRoot as the winning API root for base. Call it
// only after a successful fetch, never on a 404: one absent collection must
// not blacklist that apiRoot for the rest of the server's collections.
func (m *apiRootMemo) recordWinner(base, apiRoot string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.winners[base] = apiRoot
}
