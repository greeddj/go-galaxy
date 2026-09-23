package collections

import (
	"sync"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
)

// rolePin is what discovery learned about one role, as the install phase
// consumes it; prebuilt is set only under --no-cache, handing the install
// phase the artifact discovery built so the repository is not fetched twice.
type rolePin struct {
	prebuilt   *downloadResult
	locator    string
	commit     string
	repository string
	ref        string
	version    string
	galaxyName string
	galaxySHA  string
	server     string
	roleName   string
	kind       string
	// url and sha256 are a url role's pin - the tarball URL and the sha256
	// of the bytes it served - standing where repository and commit stand
	// for a git or Galaxy pin; each kind leaves the other's fields empty.
	url    string
	sha256 string
	deps   []gitsource.RoleDependency
}

// roleDiscoveryMemo is the run-wide table of discovered roles by install
// name, shared by the resolve and install phases so the install phase can
// pick up a --no-cache build the resolve phase left for it.
type roleDiscoveryMemo struct {
	pins map[string]rolePin
	mu   sync.Mutex
}

func newRoleDiscoveryMemo() *roleDiscoveryMemo {
	return &roleDiscoveryMemo{pins: make(map[string]rolePin)}
}

func (m *roleDiscoveryMemo) put(name string, pin rolePin) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pins[name] = pin
}

func (m *roleDiscoveryMemo) get(name string) (rolePin, bool) {
	if m == nil {
		return rolePin{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pin, ok := m.pins[name]
	return pin, ok
}

// takePrebuilt hands out a --no-cache build exactly once: the install worker
// that takes it owns its cleanup from then on.
func (m *roleDiscoveryMemo) takePrebuilt(name string) (downloadResult, bool) {
	if m == nil {
		return downloadResult{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pin, ok := m.pins[name]
	if !ok || pin.prebuilt == nil {
		return downloadResult{}, false
	}
	result := *pin.prebuilt
	pin.prebuilt = nil
	m.pins[name] = pin
	return result, true
}

// cleanup removes every --no-cache build no install worker took. It runs
// when the run ends, from withBackend's defer, so a build left behind by a
// failed or dry run is not leaked.
func (m *roleDiscoveryMemo) cleanup() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, pin := range m.pins {
		if pin.prebuilt != nil {
			cleanupIfNeeded(pin.prebuilt.Cleanup)
			pin.prebuilt = nil
			m.pins[name] = pin
		}
	}
}
