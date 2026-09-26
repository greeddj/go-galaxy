package collections

import "sync"

// absentMemo records, per server base and collection, the 404 that ended the
// server's whole root walk, so a later walk in the same phase passes that server
// over. Like apiRootMemo it lives in one collectionDeps and is never persisted.
type absentMemo struct {
	notFound map[string]error // base + "\n" + ns.name -> the walk's last 404
	mu       sync.RWMutex
}

// newAbsentMemo returns an empty memo.
func newAbsentMemo() *absentMemo {
	return &absentMemo{notFound: make(map[string]error)}
}

// lookup returns the 404 recorded for fqdn at base, or nil. A nil receiver
// reports nothing, as apiRootMemo's does.
func (m *absentMemo) lookup(base, fqdn string) error {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.notFound[base+"\n"+fqdn]
}

// record stores notFound, the 404 that ended base's walk for fqdn, web pages
// beside it passed over. Never record any other failure: it aborts the walk.
func (m *absentMemo) record(base, fqdn string, notFound error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notFound[base+"\n"+fqdn] = notFound
}
