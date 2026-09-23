package cleanup

import (
	"slices"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

const (
	urlKeysTarball  = "https://dl.example/acme-app-1.2.3.tar.gz"
	urlKeysOtherURL = "https://dl.example/acme-other-1.0.0.tar.gz"
	urlKeysSHA      = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	urlKeysOldSHA   = "0000000000000000000000000000000000000000000000000000000000000000"
)

// urlKeysStore builds the store the urlRootKeys rows share: acme.app@1.2.3 at
// the pinned sha, acme.app@1.0.0 from the same URL at an older sha that only
// the locator scan sees, and acme.other@1.0.0 from another URL.
func urlKeysStore(withPin bool) (*store.Store, map[string][]installedCollection) {
	st := store.New()
	record := func(key, url, sha string) {
		st.SetInstalled(key, store.InstalledEntry{Source: urlsource.Locator{URL: url, SHA256: sha}.String()})
	}
	record("acme.app@1.2.3", urlKeysTarball, urlKeysSHA)
	record("acme.app@1.0.0", urlKeysTarball, urlKeysOldSHA)
	record("acme.other@1.0.0", urlKeysOtherURL, urlKeysSHA)
	installedByKey := make(map[string][]installedCollection)
	for _, key := range []string{"acme.app@1.2.3", "acme.app@1.0.0", "acme.other@1.0.0"} {
		installedByKey[key] = []installedCollection{{Key: key}}
	}
	if withPin {
		st.SetURLPin(urlsource.PinKey(urlKeysTarball), store.URLPinEntry{
			SHA256: urlKeysSHA, Namespace: "acme", Name: "app", Version: "1.2.3",
		})
	}
	return st, installedByKey
}

type urlRootKeysCase struct {
	name     string
	want     []string
	withPin  bool
	pinGhost bool
}

// urlRootKeysCases pins which branch answers and what each keeps: the pin
// when one is recorded (its one identity, a ghost dropped), the locator
// scan otherwise (every sha of the URL, sorted, the other URL untouched).
func urlRootKeysCases() []urlRootKeysCase {
	return []urlRootKeysCase{
		{name: "pin present", withPin: true, want: []string{"acme.app@1.2.3"}},
		{name: "pin present but ghost", withPin: true, pinGhost: true, want: []string{}},
		{name: "pin absent", want: []string{"acme.app@1.0.0", "acme.app@1.2.3"}},
	}
}

func TestURLRootKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range urlRootKeysCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, installedByKey := urlKeysStore(tc.withPin)
			if tc.pinGhost {
				st.SetURLPin(urlsource.PinKey(urlKeysTarball), store.URLPinEntry{
					SHA256: urlKeysSHA, Namespace: "acme", Name: "ghost", Version: "9.9.9",
				})
			}
			root := requirements.CollectionRequirement{Source: urlKeysTarball, Type: requirements.TypeURL}
			got := urlRootKeys(st, installedByKey, root)
			if got == nil {
				got = []string{}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("urlRootKeys() = %v, want %v", got, tc.want)
			}
		})
	}
}
