package requirements

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// noValueSpellings are the three ways YAML writes a key with no value.
func noValueSpellings() map[string]string {
	return map[string]string{"bare": "", "tilde": " ~", "null": " null"}
}

// entryBases are one Galaxy, one git and one url collection entry, as the
// keys they are read by; the key under test replaces its own line.
func entryBases() map[string]map[string]string {
	return map[string]map[string]string{
		"galaxy": {"name": "ns.name", "version": "1.0.0"},
		"git":    {"type": "git", "source": "https://git.example/acme/app.git", "version": "main"},
		"url":    {"name": "https://dl.example/acme-app-1.0.0.tar.gz", "type": "url"},
	}
}

// entryWithoutValue renders base as a collections entry, key written with
// the given no-value spelling in place of any value base holds for it.
func entryWithoutValue(base map[string]string, key, spelling string) string {
	var b strings.Builder
	b.WriteString("collections:\n  - " + key + ":" + spelling + "\n")
	for k, v := range base {
		if k != key {
			b.WriteString("    " + k + ": " + v + "\n")
		}
	}
	return b.String()
}

// TestParseRefusesAKeyWithNoValue pins that every key a collection entry is
// read by, written with no value, is refused at load naming the key and the
// entry's index, on a Galaxy, a git and a url entry alike.
func TestParseRefusesAKeyWithNoValue(t *testing.T) {
	t.Parallel()
	for kind, base := range entryBases() {
		for key := range collectionTableKeys() {
			for spelling, text := range noValueSpellings() {
				t.Run(kind+"/"+key+"/"+spelling, func(t *testing.T) {
					t.Parallel()
					_, err := Parse([]byte(entryWithoutValue(base, key, text)), "https://default")
					want := "collections[0]: invalid collection entry: " + key + " has no value"
					if !errors.Is(err, helpers.ErrInvalidCollectionEntry) || !strings.HasPrefix(err.Error(), want) {
						t.Fatalf("Parse error = %v, want ErrInvalidCollectionEntry starting %q", err, want)
					}
				})
			}
		}
	}
}

// TestParseRefusesAnEntryWithNoValue pins that a list item written with no
// value is refused in both lists, where it once printed as <nil>.
func TestParseRefusesAnEntryWithNoValue(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		want  error
		input string
		msg   string
	}{
		"collection dash":  {input: "collections:\n  - ns.a\n  -\n", want: helpers.ErrInvalidCollectionEntry, msg: "collections[1]: "},
		"collection tilde": {input: "- ns.a\n- ~\n", want: helpers.ErrInvalidCollectionEntry, msg: "collections[1]: "},
		"role tilde":       {input: "roles:\n  - ~\n", want: helpers.ErrInvalidRoleEntry, msg: "roles[0]: "},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.input), "https://default")
			want := tc.msg + tc.want.Error() + ": the entry has no value"
			if !errors.Is(err, tc.want) || err.Error() != want {
				t.Fatalf("Parse error = %v, want %q", err, want)
			}
		})
	}
}

// TestParseRefusesAListOrMappingWhereTextBelongs pins that a key taking text
// refuses a list or a mapping by its Go type, never printing what it holds.
func TestParseRefusesAListOrMappingWhereTextBelongs(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		want       error
		input, msg string
	}{
		"list under type": {
			input: "- name: ns.name\n  type: [git]\n", want: helpers.ErrInvalidCollectionEntry,
			msg: "collections[0]: invalid collection entry: type is a []interface {}, not a string",
		},
		"mapping under version": {
			input: "- name: ns.name\n  version: {a: s3cret}\n", want: helpers.ErrInvalidCollectionEntry,
			msg: "collections[0]: invalid collection entry: version is a map[string]interface {}, not a string",
		},
		"list under name": {
			input: "- name: [s3cret]\n", want: helpers.ErrInvalidCollectionEntry,
			msg: "collections[0]: invalid collection entry: name is a []interface {}, not a string",
		},
		"mapping under role name": {
			input: "roles:\n  - src: a.b\n    name: {a: s3cret}\n", want: helpers.ErrInvalidRoleEntry,
			msg: "roles[0]: invalid role entry: name is a map[string]interface {}, not a string",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.input), "https://default")
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("Parse error = %v, want %q", err, tc.msg)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Fatalf("error %q prints the value", err)
			}
		})
	}
}

// TestParseEmptyStringStaysAbsent pins that an explicit "" is not a key with
// no value: it still reads as absent in both formats.
func TestParseEmptyStringStaysAbsent(t *testing.T) {
	t.Parallel()
	yamlFile, err := Parse([]byte("collections:\n  - name: ns.name\n    version: \"\"\n"+
		"  - name: git+https://git.example/acme/app.git\n    version: \"\"\nroles:\n  - src: a.b\n    version: \"\"\n"), "https://default")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := yamlFile.Collections; got[0].Version != "*" || got[1].Ref != "HEAD" || yamlFile.Roles[0].Version != "" {
		t.Fatalf("Parse = %+v, want version *, ref HEAD and no role version", yamlFile)
	}
	tomlFile, err := ParseTOML([]byte("[project]\ncollections = [{ name = \"ns.name\", version = \"\" }]\n"+
		"roles = [{ src = \"a.b\", version = \"\" }]\n"), "https://default")
	if err != nil || tomlFile.Collections[0].Version != "*" || tomlFile.Roles[0].Version != "" {
		t.Fatalf("ParseTOML = %+v, %v; want version * and no role version", tomlFile, err)
	}
}

// TestParseRolesRefuseAKeyWithNoValue pins the same rule on a role entry:
// each key it is read by, written with no value, is refused naming the key.
func TestParseRolesRefuseAKeyWithNoValue(t *testing.T) {
	t.Parallel()
	for key := range roleMapKeys() {
		for spelling, text := range noValueSpellings() {
			t.Run(key+"/"+spelling, func(t *testing.T) {
				t.Parallel()
				other := "src: a.b"
				if key == "src" {
					other = "name: a.b"
				}
				_, err := Parse([]byte("roles:\n  - "+other+"\n    "+key+":"+text+"\n"), "https://default")
				want := "roles[0]: invalid role entry: " + key + " has no value"
				if _, ok := errors.AsType[*RolesError](err); !ok || !errors.Is(err, helpers.ErrInvalidRoleEntry) ||
					!strings.HasPrefix(err.Error(), want) {
					t.Fatalf("Parse error = %v, want a RolesError starting %q", err, want)
				}
			})
		}
	}
}

// TestParseUnknownKeyWithNoValueIsNotRead pins the rule's edge: a key no
// entry is read by stays ignored on a collection and a warning on a role.
func TestParseUnknownKeyWithNoValueIsNotRead(t *testing.T) {
	t.Parallel()
	f, err := Parse([]byte("collections:\n  - name: ns.name\n    extra:\nroles:\n  - src: a.b\n    extra:\n"), "https://default")
	if err != nil || len(f.Collections) != 1 || len(f.Roles) != 1 {
		t.Fatalf("Parse = %+v, %v; want one collection and one role", f, err)
	}
	if len(f.Warnings) != 1 || !strings.Contains(f.Warnings[0], "extra") {
		t.Fatalf("warnings = %q, want the role's unknown key", f.Warnings)
	}
}
