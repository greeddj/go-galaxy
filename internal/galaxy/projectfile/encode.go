package projectfile

import (
	"fmt"
	"strings"

	"github.com/greeddj/go-galaxy/internal/safeout"
)

// Draft is a [project] table to write. Name "" writes no name key, and a
// list whose Has flag is false writes no key; one that is true but empty
// writes "= []", so a file naming only an empty list still decodes.
type Draft struct {
	Name           string
	Collections    []Entry
	Roles          []Entry
	HasCollections bool
	HasRoles       bool
}

// Entry is one list item: a basic string holding Text when Fields is empty,
// else an inline table of Fields in their order.
type Entry struct {
	Text   string
	Fields []Field
}

// Field is one inline-table key, written bare, so Key must be a TOML bare
// key; Value is a basic string, or List an array of them when IsList.
type Field struct {
	Key    string
	Value  string
	List   []string
	IsList bool
}

// Encode renders d in the documented layout: [project], then name, then each
// list one entry per line with a trailing comma. It never fails; the caller
// proves the bytes by decoding them back.
func Encode(d Draft) []byte {
	var b strings.Builder
	b.WriteString("[project]\n")
	if d.Name != "" {
		b.WriteString("name = " + quoteBasic(d.Name) + "\n")
	}
	if d.HasCollections {
		writeList(&b, "collections", d.Collections)
	}
	if d.HasRoles {
		writeList(&b, "roles", d.Roles)
	}
	return []byte(b.String())
}

// writeList writes one array key, "= []" when entries is empty.
func writeList(b *strings.Builder, key string, entries []Entry) {
	if len(entries) == 0 {
		b.WriteString(key + " = []\n")
		return
	}
	b.WriteString(key + " = [\n")
	for _, e := range entries {
		b.WriteString("  " + entryText(e) + ",\n")
	}
	b.WriteString("]\n")
}

// entryText renders one entry as a basic string or an inline table.
func entryText(e Entry) string {
	if len(e.Fields) == 0 {
		return quoteBasic(e.Text)
	}
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		value := quoteBasic(f.Value)
		if f.IsList {
			items := make([]string, 0, len(f.List))
			for _, item := range f.List {
				items = append(items, quoteBasic(item))
			}
			value = "[" + strings.Join(items, ", ") + "]"
		}
		parts = append(parts, f.Key+" = "+value)
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// quoteBasic writes s as a TOML basic string: the quote, the backslash and the
// controls with a short escape take it, every other safeout.IsUnsafeRune rune
// is \uXXXX, so the bytes print as they decode. s must be valid UTF-8.
func quoteBasic(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if escape, ok := shortEscape(r); ok {
			b.WriteString(escape)
			continue
		}
		if safeout.IsUnsafeRune(r) {
			fmt.Fprintf(&b, `\u%04X`, r)
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// shortEscape is the two-character TOML escape of r, when r has one.
func shortEscape(r rune) (string, bool) {
	switch r {
	case '"':
		return `\"`, true
	case '\\':
		return `\\`, true
	case '\b':
		return `\b`, true
	case '\t':
		return `\t`, true
	case '\n':
		return `\n`, true
	case '\f':
		return `\f`, true
	case '\r':
		return `\r`, true
	default:
		return "", false
	}
}
