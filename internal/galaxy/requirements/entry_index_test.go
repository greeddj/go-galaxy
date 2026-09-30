package requirements

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestParseCollectionErrorsNameTheirIndex pins that a refused collections
// entry is named by its index once, in both formats and on both TOML paths,
// with the sentinel intact, as a refused roles entry already is.
func TestParseCollectionErrorsNameTheirIndex(t *testing.T) {
	t.Parallel()
	const second = "collections[1]: "
	cases := map[string]struct {
		parse  func([]byte, string) (File, error)
		want   error
		input  string
		prefix string
	}{
		"yaml mapping":   {parse: Parse, input: "collections:\n  - ns.a\n  - Bad.Name\n", want: helpers.ErrInvalidCollectionName, prefix: second},
		"yaml bare list": {parse: Parse, input: "- ns.a\n- Bad.Name\n", want: helpers.ErrInvalidCollectionName, prefix: second},
		"toml shim": {
			parse: ParseTOML, input: tomlCollections(`"ns.a", "ns.name@1.0"`), want: helpers.ErrInvalidCollectionName, prefix: second,
		},
		"toml parse": {
			parse: ParseTOML, input: tomlCollections(`"ns.a", "Bad.Name"`), want: helpers.ErrInvalidCollectionName, prefix: second,
		},
		"yaml roles list": {parse: Parse, input: "roles:\n  - a.b\n  - docker\n", want: helpers.ErrInvalidRoleName, prefix: "roles[1]: "},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.parse([]byte(tc.input), "https://default")
			if !errors.Is(err, tc.want) || !strings.HasPrefix(err.Error(), tc.prefix) {
				t.Fatalf("error = %v, want %v starting %q", err, tc.want, tc.prefix)
			}
			if strings.Count(err.Error(), "[1]: ") != 1 {
				t.Fatalf("error %q names the index more than once", err)
			}
		})
	}
}
