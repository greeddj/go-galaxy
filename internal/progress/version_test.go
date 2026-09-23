package progress

import (
	"bytes"
	"strings"
	"testing"
)

// Version-tier fixtures: the benign pair is what a real install passes, the
// hostile pair carries hostileCallerText's control characters so a missing
// Clean on either parameter shows up as raw escape bytes.
const (
	benignVersion       = "1.0.0"
	benignCause         = "error: boom"
	hostileVersion      = "1.0.0\x1b[31m\rowned"
	hostileVersionClean = "1.0.0�[31m�owned"
	hostileCause        = "error: \x1b[31mred\rgone"
	hostileCauseClean   = "error: �[31mred�gone"
)

// colorVersionTag is benignVersion's colored tag, built through versionTag
// like okMark so it cannot describe a form production does not emit.
func colorVersionTag() string { return versionTag(benignVersion, true) }

// TestVersionTagFollowsItsDestination pins that the version tag is plain on a
// redirected stdout and colored on a terminal stderr of the same Progress; the
// plain half is what lets `grep '== '` find a version in a redirected log.
func TestVersionTagFollowsItsDestination(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	p := newStreamProgress(false, false, false,
		stream{w: &out, color: false}, stream{w: &errOut, color: true})
	defer p.Close()

	p.OkVersionf(benignVersion, "Installed: acme.app")
	p.ErrorVersionf(benignVersion, benignCause, "Failed: acme.app")

	assertBuf(t, &out, okGlyph+" Installed: acme.app == "+benignVersion+"\n")
	want := failMark() + "Failed: acme.app" + colorVersionTag() + " " + benignCause + "\n"
	if got := errOut.String(); got != want {
		t.Errorf("errOut = %q, want %q", got, want)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("a plain destination received escape bytes: %q", out.String())
	}
}

// TestVersionIsPlacedBeforeTheCause pins that a failure line's version
// precedes its cause, since a version after the cause reads as error text.
func TestVersionIsPlacedBeforeTheCause(t *testing.T) {
	t.Parallel()

	p, _, errOut := stateB()
	defer p.Close()
	p.ErrorVersionf(benignVersion, benignCause, "Failed: acme.app")

	got := errOut.String()
	versionAt := strings.Index(got, versionMark)
	causeAt := strings.Index(got, benignCause)
	if versionAt < 0 || causeAt < 0 {
		t.Fatalf("line %q is missing the version tag or the cause", got)
	}
	if versionAt > causeAt {
		t.Fatalf("line %q puts the version behind the cause", got)
	}
}

// TestEmptyVersionRendersTheUndecoratedLine pins that with an empty version
// OkVersionf and ErrorVersionf print exactly what Okf and Errorf print, never
// a bare "== ", so a call site may pass whatever version it has.
func TestEmptyVersionRendersTheUndecoratedLine(t *testing.T) {
	t.Parallel()

	t.Run("OkVersionf", func(t *testing.T) {
		t.Parallel()
		plain, out, _ := stateB()
		defer plain.Close()
		plain.Okf("Installed: acme.app")

		tagged, taggedOut, _ := stateB()
		defer tagged.Close()
		tagged.OkVersionf("", "Installed: acme.app")

		assertBuf(t, taggedOut, out.String())
	})

	t.Run("ErrorVersionf", func(t *testing.T) {
		t.Parallel()
		plain, _, errOut := stateB()
		defer plain.Close()
		plain.Errorf("Failed: acme.app %s", benignCause)

		tagged, _, taggedErr := stateB()
		defer tagged.Close()
		tagged.ErrorVersionf("", benignCause, "Failed: acme.app")

		assertBuf(t, taggedErr, errOut.String())
	})
}

// TestEmptyCauseLeavesNoTrailingSpace pins that a failure line with a version
// and no cause ends at the version rather than at an invisible trailing space.
func TestEmptyCauseLeavesNoTrailingSpace(t *testing.T) {
	t.Parallel()

	p, _, errOut := stateB()
	defer p.Close()
	p.ErrorVersionf(benignVersion, "", "Failed: acme.app")

	want := failMark() + "Failed: acme.app" + colorVersionTag() + "\n"
	if got := errOut.String(); got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

// TestVersionAndCauseAreSanitizedIndependently pins that version and cause
// are each cleaned as untrusted payload while the tag's own escapes survive
// between them; the whole-line match also catches escapes stripped from it.
func TestVersionAndCauseAreSanitizedIndependently(t *testing.T) {
	t.Parallel()

	p, _, errOut := stateB()
	defer p.Close()
	p.ErrorVersionf(hostileVersion, hostileCause, "Failed: acme.app")

	want := failMark() + "Failed: acme.app" +
		" " + ansiGray + versionMark + hostileVersionClean + ansiReset +
		" " + hostileCauseClean + "\n"
	if got := errOut.String(); got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

// TestVersionTiersEmitInEveryState pins that OkVersionf and ErrorVersionf
// emit in all four states on their undecorated siblings' streams, quiet
// included, since a --quiet CI run must still say what it installed.
func TestVersionTiersEmitInEveryState(t *testing.T) {
	states := []struct {
		build func() (*Progress, *bytes.Buffer, *bytes.Buffer)
		name  string
		tag   string
	}{
		{name: "A spinner", build: stateA, tag: colorVersionTag()},
		{name: "B verbose", build: stateB, tag: colorVersionTag()},
		{name: "C quiet", build: stateC, tag: colorVersionTag()},
		{name: "D non-TTY", build: stateD, tag: " " + versionMark + benignVersion},
	}

	for _, st := range states {
		t.Run(st.name+"/OkVersionf", func(t *testing.T) {
			p, out, errOut := st.build()
			defer p.Close()
			p.OkVersionf(benignVersion, "Installed: acme.app")
			assertBuf(t, out, okPrefixFor(st.name)+"Installed: acme.app"+st.tag+"\n")
			assertEmpty(t, errOut)
		})

		t.Run(st.name+"/ErrorVersionf", func(t *testing.T) {
			p, out, errOut := st.build()
			defer p.Close()
			p.ErrorVersionf(benignVersion, benignCause, "Failed: acme.app")
			assertBuf(t, errOut, failPrefixFor(st.name)+"Failed: acme.app"+st.tag+" "+benignCause+"\n")
			assertEmpty(t, out)
		})
	}
}

// okPrefixFor and failPrefixFor return the marker the named state renders:
// colored for states A through C, whose destinations are terminals, and
// plain for state D, which is the non-TTY case.
func okPrefixFor(state string) string {
	if strings.HasPrefix(state, "D") {
		return okGlyph + " "
	}
	return okMark()
}

func failPrefixFor(state string) string {
	if strings.HasPrefix(state, "D") {
		return failGlyph + " "
	}
	return failMark()
}
