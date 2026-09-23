package progress

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNewProgressSpinnerCreation verifies the spinner is created only for
// the single (verbose=false, quiet=false, terminal=true) combination; every
// other combination of the three booleans must leave the spinner nil.
func TestNewProgressSpinnerCreation(t *testing.T) {
	var out, errOut bytes.Buffer

	p := newProgress(false, false, true, &out, &errOut)
	if p.s == nil {
		t.Fatal("expected spinner to be created for verbose=false, quiet=false, terminal=true")
	}
	p.Close()

	combos := []struct {
		name           string
		verbose, quiet bool
		terminal       bool
	}{
		{"verbose", true, false, true},
		{"quiet", false, true, true},
		{"nonTerminal", false, false, false},
		{"verboseQuiet", true, true, true},
		{"verboseNonTerminal", true, false, false},
	}
	for _, c := range combos {
		t.Run(c.name, func(t *testing.T) {
			p := newProgress(c.verbose, c.quiet, c.terminal, &out, &errOut)
			if p.s != nil {
				t.Fatalf("expected nil spinner for verbose=%v quiet=%v terminal=%v", c.verbose, c.quiet, c.terminal)
			}
			p.Close()
		})
	}
}

// assertBuf fails the test unless buf holds exactly want.
func assertBuf(t *testing.T, buf *bytes.Buffer, want string) {
	t.Helper()
	if got := buf.String(); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// assertEmpty fails the test unless buf is empty.
func assertEmpty(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	if buf.Len() != 0 {
		t.Fatalf("expected empty buffer, got %q", buf.String())
	}
}

// stateA builds a Progress plus its stdout/stderr buffers for state A
// (TTY normal: spinner active).
func stateA() (*Progress, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return newProgress(false, false, true, &out, &errOut), &out, &errOut
}

// TestStateATransient covers Printf and Write for state A: both route through
// the spinner (suffix update / stop-print-restart) instead of the buffer directly.
func TestStateATransient(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, out, _ := stateA()
		defer p.Close()
		p.Printf("x")
		assertEmpty(t, out)
		if got := string(p.s.suffixText()); got != " x" {
			t.Fatalf("expected spinner suffix %q, got %q", " x", got)
		}
	})

	t.Run("WriteWithMessage", func(t *testing.T) {
		p, out, _ := stateA()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertBuf(t, out, stepMark()+"x\n")
	})

	t.Run("WriteEmptyMessage", func(t *testing.T) {
		p, out, _ := stateA()
		defer p.Close()
		n, err := p.Write([]byte("\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected n=1, got %d", n)
		}
		assertEmpty(t, out)
	})
}

// TestStateAResult covers the result tier for state A: PersistentPrintf and Okf
// emit to stdout, Errorf and Warnf emit to stderr, all despite an active spinner.
func TestStateAResult(t *testing.T) {
	t.Run("PersistentPrintf", func(t *testing.T) {
		p, out, errOut := stateA()
		defer p.Close()
		p.PersistentPrintf("x")
		assertBuf(t, out, stepMark()+"x\n")
		assertEmpty(t, errOut)
	})

	t.Run("Okf", func(t *testing.T) {
		p, out, errOut := stateA()
		defer p.Close()
		p.Okf("x")
		assertBuf(t, out, okMark()+"x\n")
		assertEmpty(t, errOut)
	})

	t.Run("Errorf", func(t *testing.T) {
		p, out, errOut := stateA()
		defer p.Close()
		p.Errorf("x")
		assertBuf(t, errOut, failMark()+"x\n")
		assertEmpty(t, out)
	})

	t.Run("Warnf", func(t *testing.T) {
		p, out, errOut := stateA()
		defer p.Close()
		p.Warnf("x")
		assertBuf(t, errOut, warnMark()+"x\n")
		assertEmpty(t, out)
	})
}

// TestStateADebug covers the debug tier for state A: debug output stays
// suppressed since verbose is false.
func TestStateADebug(t *testing.T) {
	t.Run("Debugf", func(t *testing.T) {
		p, out, _ := stateA()
		defer p.Close()
		p.Debugf("x")
		assertEmpty(t, out)
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, out, _ := stateA()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		assertEmpty(t, out)
	})
}

// stateB builds a Progress plus its buffers for state B (verbose, no spinner).
func stateB() (*Progress, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return newProgress(true, false, true, &out, &errOut), &out, &errOut
}

// TestStateBTransient covers Printf and Write for state B: no spinner exists,
// so both write a full line directly.
func TestStateBTransient(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, out, _ := stateB()
		defer p.Close()
		p.Printf("x")
		assertBuf(t, out, stepMark()+"x\n")
	})

	t.Run("Write", func(t *testing.T) {
		p, out, _ := stateB()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertBuf(t, out, stepMark()+"x\n")
	})
}

// TestResultTierAcrossStates pins that verbose (state B) and quiet (state C)
// both emit every result line and keep Errorf on stderr: quiet suppresses the
// transient tier and nothing else.
func TestResultTierAcrossStates(t *testing.T) {
	states := []struct {
		build func() (*Progress, *bytes.Buffer, *bytes.Buffer)
		name  string
	}{
		{name: "B verbose", build: stateB},
		{name: "C quiet", build: stateC},
	}

	for _, st := range states {
		t.Run(st.name+"/PersistentPrintf", func(t *testing.T) {
			p, out, errOut := st.build()
			defer p.Close()
			p.PersistentPrintf("x")
			assertBuf(t, out, stepMark()+"x\n")
			assertEmpty(t, errOut)
		})

		t.Run(st.name+"/Okf", func(t *testing.T) {
			p, out, errOut := st.build()
			defer p.Close()
			p.Okf("x")
			assertBuf(t, out, okMark()+"x\n")
			assertEmpty(t, errOut)
		})

		t.Run(st.name+"/Errorf", func(t *testing.T) {
			p, out, errOut := st.build()
			defer p.Close()
			p.Errorf("x")
			assertBuf(t, errOut, failMark()+"x\n")
			assertEmpty(t, out)
		})
	}
}

// TestStateBDebug covers the debug tier for state B: verbose mode enables
// both Debugf and DebugSincef.
func TestStateBDebug(t *testing.T) {
	t.Run("Debugf", func(t *testing.T) {
		p, out, _ := stateB()
		defer p.Close()
		p.Debugf("x")
		assertBuf(t, out, stepMark()+debugPrefix+"x\n")
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, out, _ := stateB()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		got := out.String()
		if !strings.HasPrefix(got, stepMark()+debugPrefix+"timing (") {
			t.Fatalf("expected prefix %q, got %q", stepMark()+debugPrefix+"timing (", got)
		}
		if !strings.HasSuffix(got, ") x\n") {
			t.Fatalf("expected suffix %q, got %q", ") x\n", got)
		}
	})
}

// stateC builds a Progress plus its buffers for state C (quiet, no spinner).
func stateC() (*Progress, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return newProgress(false, true, true, &out, &errOut), &out, &errOut
}

// TestStateCSuppressed covers the tiers suppressed by quiet mode: Printf,
// Write and both debug methods must stay silent.
func TestStateCSuppressed(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, out, _ := stateC()
		defer p.Close()
		p.Printf("x")
		assertEmpty(t, out)
	})

	t.Run("Write", func(t *testing.T) {
		p, out, _ := stateC()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertEmpty(t, out)
	})

	t.Run("Debugf", func(t *testing.T) {
		p, out, _ := stateC()
		defer p.Close()
		p.Debugf("x")
		assertEmpty(t, out)
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, out, _ := stateC()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		assertEmpty(t, out)
	})
}

// stateD builds a Progress plus its buffers for state D (non-TTY normal:
// the CI-defect regression case, no spinner, verbose=false, quiet=false).
func stateD() (*Progress, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return newProgress(false, false, false, &out, &errOut), &out, &errOut
}

// TestStateDTransient covers Printf and Write for state D: no spinner
// exists and quiet is false, so both must emit a full line - with no TTY
// there is no spinner to carry it, so dropping it leaves a CI run silent.
func TestStateDTransient(t *testing.T) {
	t.Run("Printf", func(t *testing.T) {
		p, out, _ := stateD()
		defer p.Close()
		p.Printf("x")
		assertBuf(t, out, stepGlyph+" x\n")
	})

	t.Run("Write", func(t *testing.T) {
		p, out, _ := stateD()
		defer p.Close()
		n, err := p.Write([]byte("x\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("expected n=2, got %d", n)
		}
		assertBuf(t, out, stepGlyph+" x\n")
	})
}

// TestStateDResult covers the result tier for state D: it must always emit,
// same as every other state; Errorf targets stderr.
func TestStateDResult(t *testing.T) {
	t.Run("PersistentPrintf", func(t *testing.T) {
		p, out, errOut := stateD()
		defer p.Close()
		p.PersistentPrintf("x")
		assertBuf(t, out, stepGlyph+" x\n")
		assertEmpty(t, errOut)
	})

	// State D is the non-TTY case, so its markers are plain where states A
	// through C assert the colored form on the same calls.
	t.Run("Okf", func(t *testing.T) {
		p, out, errOut := stateD()
		defer p.Close()
		p.Okf("x")
		assertBuf(t, out, okGlyph+" x\n")
		assertEmpty(t, errOut)
	})

	t.Run("Errorf", func(t *testing.T) {
		p, out, errOut := stateD()
		defer p.Close()
		p.Errorf("x")
		assertBuf(t, errOut, failGlyph+" x\n")
		assertEmpty(t, out)
	})
}

// TestStateDDebug covers the debug tier for state D: debug output stays
// suppressed since verbose is false.
func TestStateDDebug(t *testing.T) {
	t.Run("Debugf", func(t *testing.T) {
		p, out, _ := stateD()
		defer p.Close()
		p.Debugf("x")
		assertEmpty(t, out)
	})

	t.Run("DebugSincef", func(t *testing.T) {
		p, out, _ := stateD()
		defer p.Close()
		p.DebugSincef(time.Now(), "x")
		assertEmpty(t, out)
	})
}

// TestClose verifies Close does not panic whether or not a spinner exists.
func TestClose(*testing.T) {
	var out, errOut bytes.Buffer

	withSpinner := newProgress(false, false, true, &out, &errOut)
	withSpinner.Close()

	withoutSpinner := newProgress(true, false, true, &out, &errOut)
	withoutSpinner.Close()
}

// TestPrintfSuffixRace drives concurrent spinner-suffix updates through Printf
// against concurrent reads that take the spinner lock, mirroring how the render
// goroutine reads the suffix. It must stay clean under the race detector.
func TestPrintfSuffixRace(t *testing.T) {
	p := newProgress(false, false, true, io.Discard, io.Discard)
	if p.s == nil {
		t.Fatal("expected active spinner for the race scenario")
	}
	defer p.Close()

	const (
		workers    = 8
		iterations = 200
	)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range iterations {
				_ = p.s.suffixText()
			}
		})
		wg.Go(func() {
			for i := range iterations {
				p.Printf("tick %d", i)
			}
		})
	}
	wg.Wait()
}

// TestConcurrentEmissionSerialized drives every emitting method from many
// goroutines: under -race the printer mutex must keep each line intact, the
// line counts exact, and each method on its own stream.
func TestConcurrentEmissionSerialized(t *testing.T) {
	var out, errOut bytes.Buffer
	p := newProgress(false, false, true, &out, &errOut)
	if p.s == nil {
		t.Fatal("expected active spinner for state A")
	}
	defer p.Close()

	const (
		workers    = 8
		iterations = 25
	)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range iterations {
				p.PersistentPrintf("pp %d", i)
				p.Okf("ok %d", i)
				p.Errorf("err %d", i)
				_, _ = p.Write([]byte("wr\n"))
				p.Printf("pf %d", i) // suffix only, never reaches a buffer
			}
		})
	}
	wg.Wait()

	validStdout := func(line string) bool {
		return strings.HasPrefix(line, stepMark()+"pp ") || strings.HasPrefix(line, okMark()+"ok ") ||
			line == stepMark()+"wr"
	}
	assertLines(t, out.String(), workers*iterations*3, validStdout, "stdout")

	validStderr := func(line string) bool {
		return strings.HasPrefix(line, failMark()+"err ")
	}
	assertLines(t, errOut.String(), workers*iterations, validStderr, "stderr")
}

// assertLines splits content into non-trailing lines and asserts the exact
// count and that every line passes valid; stream names the writer for errors.
func assertLines(t *testing.T, content string, want int, valid func(string) bool, stream string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != want {
		t.Fatalf("expected %d %s lines, got %d", want, stream, len(lines))
	}
	for _, line := range lines {
		if !valid(line) {
			t.Fatalf("unexpected or interleaved %s line: %q", stream, line)
		}
	}
}

// TestPackageLevelOkf verifies the standalone package-level Okf helper writes
// to os.Stdout, since it runs before any Progress exists.
func TestPackageLevelOkf(t *testing.T) {
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	Okf("hello %s", "world")

	if closeErr := w.Close(); closeErr != nil {
		t.Fatalf("failed to close pipe writer: %v", closeErr)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read pipe: %v", err)
	}

	if want := okGlyph + " hello world\n"; string(got) != want {
		t.Fatalf("expected %q, got %q", want, string(got))
	}
}

// TestPackageLevelErrorf verifies the standalone package-level Errorf helper
// writes to os.Stderr, keeping diagnostics off stdout.
func TestPackageLevelErrorf(t *testing.T) {
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	Errorf("bye %s", "world")

	if closeErr := w.Close(); closeErr != nil {
		t.Fatalf("failed to close pipe writer: %v", closeErr)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read pipe: %v", err)
	}

	if want := failGlyph + " bye world\n"; string(got) != want {
		t.Fatalf("expected %q, got %q", want, string(got))
	}
}

// hostileCallerText carries an ANSI escape and a lone CR; its clean form is
// spelled by hand, not computed with safeout.Clean, so a broken Clean cannot
// also break the expectation. Every sanitization test here shares the pair.
const (
	hostileCallerText      = "before\x1b[31mred\rafter"
	hostileCallerTextClean = "before\ufffd[31mred\ufffdafter"
)

// capturePipe redirects *target (os.Stdout or os.Stderr) to a pipe while fn
// runs and returns what was written, for the package-level Okf and Errorf.
func capturePipe(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	orig := *target
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	*target = w
	defer func() { *target = orig }()

	fn()

	if closeErr := w.Close(); closeErr != nil {
		t.Fatalf("failed to close pipe writer: %v", closeErr)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read pipe: %v", err)
	}
	return string(got)
}

// tierCase is one row of TestTiersSanitizeCallerText: a method rendering
// caller text and its exact line on stdout (wantOut) or stderr (wantErr).
type tierCase struct {
	invoke  func(p *Progress, msg string)
	wantOut func(msg, clean string) string // non-empty when the tier lands on stdout
	wantErr func(msg, clean string) string // non-empty when the tier lands on stderr
	name    string
}

// sanitizingTiers is TestTiersSanitizeCallerText's table, factored out to a
// package-level function (rather than inlined in the test body) purely to
// keep that test function's own length within budget.
func sanitizingTiers() []tierCase {
	return []tierCase{
		{
			name:    "Printf",
			invoke:  func(p *Progress, msg string) { p.Printf("%s", msg) },
			wantOut: func(_, clean string) string { return stepMark() + clean + "\n" },
		},
		{
			name:    "PersistentPrintf",
			invoke:  func(p *Progress, msg string) { p.PersistentPrintf("%s", msg) },
			wantOut: func(_, clean string) string { return stepMark() + clean + "\n" },
		},
		{
			name:    "Okf",
			invoke:  func(p *Progress, msg string) { p.Okf("%s", msg) },
			wantOut: func(_, clean string) string { return okMark() + clean + "\n" },
		},
		{
			name:    "OkVersionf",
			invoke:  func(p *Progress, msg string) { p.OkVersionf(benignVersion, "%s", msg) },
			wantOut: func(_, clean string) string { return okMark() + clean + colorVersionTag() + "\n" },
		},
		{
			name:    "Updatef",
			invoke:  func(p *Progress, msg string) { p.Updatef("%s", msg) },
			wantOut: func(_, clean string) string { return updateMark() + clean + "\n" },
		},
		{
			name:    "Errorf",
			invoke:  func(p *Progress, msg string) { p.Errorf("%s", msg) },
			wantErr: func(_, clean string) string { return failMark() + clean + "\n" },
		},
		{
			name:   "ErrorVersionf",
			invoke: func(p *Progress, msg string) { p.ErrorVersionf(benignVersion, benignCause, "%s", msg) },
			wantErr: func(_, clean string) string {
				return failMark() + clean + colorVersionTag() + " " + benignCause + "\n"
			},
		},
		{
			name:    "Warnf",
			invoke:  func(p *Progress, msg string) { p.Warnf("%s", msg) },
			wantErr: func(_, clean string) string { return warnMark() + clean + "\n" },
		},
		{
			name:    "Debugf",
			invoke:  func(p *Progress, msg string) { p.Debugf("%s", msg) },
			wantOut: func(_, clean string) string { return stepMark() + debugPrefix + clean + "\n" },
		},
		{
			name:    "Write",
			invoke:  func(p *Progress, msg string) { _, _ = p.Write([]byte(msg)) },
			wantOut: func(_, clean string) string { return stepMark() + clean + "\n" },
		},
	}
}

// runTierCase drives one tierCase against a fresh non-spinner Progress and
// asserts the exact resulting line on whichever stream the tier targets,
// and that the other stream stayed empty.
func runTierCase(t *testing.T, tc tierCase, msg, clean string) {
	t.Helper()
	p, out, errOut := stateB()
	defer p.Close()
	tc.invoke(p, msg)
	if tc.wantOut != nil {
		assertBuf(t, out, tc.wantOut(msg, clean))
		assertEmpty(t, errOut)
		return
	}
	assertBuf(t, errOut, tc.wantErr(msg, clean))
	assertEmpty(t, out)
}

// TestTiersSanitizeCallerText pins, for every tier rendering caller text, the
// exact line for a benign and a hostile message, in verbose state B so Debugf
// emits and Printf takes its non-spinner branch.
func TestTiersSanitizeCallerText(t *testing.T) {
	t.Parallel()

	for _, tc := range sanitizingTiers() {
		t.Run(tc.name+"/benign", func(t *testing.T) {
			t.Parallel()
			runTierCase(t, tc, "benign text", "benign text")
		})
		t.Run(tc.name+"/hostile", func(t *testing.T) {
			t.Parallel()
			runTierCase(t, tc, hostileCallerText, hostileCallerTextClean)
		})
	}
}

// TestDebugSincefSanitizesCallerText pins DebugSincef's sanitization; its
// elapsed time varies, so only the text around it is asserted.
func TestDebugSincefSanitizesCallerText(t *testing.T) {
	t.Parallel()

	wantPrefix := stepMark() + debugPrefix + "timing ("

	check := func(t *testing.T, msg, wantSuffix string) {
		t.Helper()
		p, out, errOut := stateB()
		defer p.Close()
		p.DebugSincef(time.Now(), "%s", msg)
		got := out.String()
		if !strings.HasPrefix(got, wantPrefix) {
			t.Fatalf("expected prefix %q, got %q", wantPrefix, got)
		}
		if !strings.HasSuffix(got, wantSuffix) {
			t.Fatalf("expected suffix %q, got %q", wantSuffix, got)
		}
		assertEmpty(t, errOut)
	}

	t.Run("benign", func(t *testing.T) {
		t.Parallel()
		check(t, "benign text", ") benign text\n")
	})
	t.Run("hostile", func(t *testing.T) {
		t.Parallel()
		check(t, hostileCallerText, ") "+hostileCallerTextClean+"\n")
	})
}

// TestPackageLevelHelpersSanitizeCallerText pins sanitization in the
// package-level Okf and Errorf, which write to os.Stdout and os.Stderr.
func TestPackageLevelHelpersSanitizeCallerText(t *testing.T) {
	cases := []struct {
		name   string
		target **os.File
		invoke func(msg string)
		prefix string
	}{
		{"Okf", &os.Stdout, func(msg string) { Okf("%s", msg) }, okGlyph + " "},
		{"Errorf", &os.Stderr, func(msg string) { Errorf("%s", msg) }, failGlyph + " "},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/benign", func(t *testing.T) {
			got := capturePipe(t, tc.target, func() { tc.invoke("benign text") })
			want := tc.prefix + "benign text\n"
			if got != want {
				t.Fatalf("expected %q, got %q", want, got)
			}
		})
		t.Run(tc.name+"/hostile", func(t *testing.T) {
			got := capturePipe(t, tc.target, func() { tc.invoke(hostileCallerText) })
			want := tc.prefix + hostileCallerTextClean + "\n"
			if got != want {
				t.Fatalf("expected %q, got %q", want, got)
			}
		})
	}
}

// TestPrintfSpinnerSuffixIsSanitized pins that Printf's spinner branch
// (state A) stores a sanitized suffix and writes nothing to out; the benign
// row proves the suffix carries real content.
func TestPrintfSpinnerSuffixIsSanitized(t *testing.T) {
	t.Parallel()

	t.Run("benign", func(t *testing.T) {
		t.Parallel()
		p, out, _ := stateA()
		defer p.Close()
		p.Printf("%s", "benign text")
		if got, want := string(p.s.suffixText()), " benign text"; got != want {
			t.Fatalf("suffix = %q, want %q", got, want)
		}
		assertEmpty(t, out)
	})

	t.Run("hostile", func(t *testing.T) {
		t.Parallel()
		p, out, _ := stateA()
		defer p.Close()
		p.Printf("%s", hostileCallerText)
		want := " " + hostileCallerTextClean
		if got := string(p.s.suffixText()); got != want {
			t.Fatalf("suffix = %q, want %q", got, want)
		}
		assertEmpty(t, out)
	})
}

// TestResultMarkerEscapesSurviveAHostileMessage pins, for every result tier,
// that a hostile message leaves the marker's own escapes intact and is itself
// sanitized, with the tier's version tag or cause intact behind it.
func TestResultMarkerEscapesSurviveAHostileMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		invoke func(p *Progress, msg string)
		stream func(out, errOut *bytes.Buffer) *bytes.Buffer
		name   string
		prefix string
		suffix string
	}{
		{
			name: "Okf", prefix: okMark(),
			invoke: func(p *Progress, msg string) { p.Okf("%s", msg) },
			stream: func(out, _ *bytes.Buffer) *bytes.Buffer { return out },
		},
		{
			name: "OkVersionf", prefix: okMark(), suffix: colorVersionTag(),
			invoke: func(p *Progress, msg string) { p.OkVersionf(benignVersion, "%s", msg) },
			stream: func(out, _ *bytes.Buffer) *bytes.Buffer { return out },
		},
		{
			name: "Updatef", prefix: updateMark(),
			invoke: func(p *Progress, msg string) { p.Updatef("%s", msg) },
			stream: func(out, _ *bytes.Buffer) *bytes.Buffer { return out },
		},
		{
			name: "Errorf", prefix: failMark(),
			invoke: func(p *Progress, msg string) { p.Errorf("%s", msg) },
			stream: func(_, errOut *bytes.Buffer) *bytes.Buffer { return errOut },
		},
		{
			name: "ErrorVersionf", prefix: failMark(), suffix: colorVersionTag() + " " + benignCause,
			invoke: func(p *Progress, msg string) { p.ErrorVersionf(benignVersion, benignCause, "%s", msg) },
			stream: func(_, errOut *bytes.Buffer) *bytes.Buffer { return errOut },
		},
		{
			name: "Warnf", prefix: warnMark(),
			invoke: func(p *Progress, msg string) { p.Warnf("%s", msg) },
			stream: func(_, errOut *bytes.Buffer) *bytes.Buffer { return errOut },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, out, errOut := stateB()
			defer p.Close()
			tc.invoke(p, hostileCallerText)
			got := tc.stream(out, errOut).String()

			// (1) the marker's own bytes are intact.
			if !strings.HasPrefix(got, tc.prefix) {
				t.Fatalf("line %q does not start with raw marker %q", got, tc.prefix)
			}
			// (2) reached only when (1) held: the message half is
			// sanitized, and whatever decoration the tier adds behind it
			// (a version tag, a cause) is there in full.
			want := tc.prefix + hostileCallerTextClean + tc.suffix + "\n"
			if got != want {
				t.Fatalf("line = %q, want %q", got, want)
			}
		})
	}
}

// TestSpinnerWritesOutsideThisPackagesWriters pins that the spinner draws on
// os.Stdout, never through a Progress stream, so frames bypass writeLine and
// the only caller text a frame carries is the already sanitized suffix.
func TestSpinnerWritesOutsideThisPackagesWriters(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	p := newProgress(false, false, true, &out, &errOut)
	defer p.Close()
	if p.s == nil {
		t.Fatal("expected spinner to be created for verbose=false, quiet=false, terminal=true")
	}
	if p.s.w == io.Writer(&out) {
		t.Fatal("the spinner must not draw on this package's out buffer")
	}
	if p.s.w == io.Writer(&errOut) {
		t.Fatal("the spinner must not draw on this package's errOut buffer")
	}
}

// okMark and its siblings are the colored marker prefixes, built through
// marker rather than re-spelled so they cannot describe a form nothing emits.
func okMark() string     { return marker(okGlyph, ansiGreen, true) }
func updateMark() string { return marker(updateGlyph, ansiYellow, true) }
func stepMark() string   { return marker(stepGlyph, ansiGray, true) }
func failMark() string   { return marker(failGlyph, ansiRed, true) }
func warnMark() string   { return marker(warnGlyph, ansiYellow, true) }

// The tests below pin both halves of the color rule, the destination check
// and the environment overrides, including on the package-level helpers:
// escapes in a redirected log would make `grep '^✔'` match nothing.

// charDeviceFile opens a character device to stand in for a terminal, since
// isTerminal is exactly an os.ModeCharDevice test and a pty is not portable.
func charDeviceFile(t *testing.T) *os.File {
	t.Helper()

	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no character device available to stand in for a terminal: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if !isTerminal(f) {
		t.Skipf("%s is not a character device on this platform", os.DevNull)
	}
	return f
}

// regularFile creates a plain file, the shape a redirected stream has.
func regularFile(t *testing.T) *os.File {
	t.Helper()

	f, err := os.Create(filepath.Join(t.TempDir(), "redirected.log"))
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestColorEnabledPrecedence pins every rule colorEnabled applies and the
// order it applies them in. Both destinations appear in the table so no row's
// verdict can be read as "this is just what that destination always gives".
func TestColorEnabledPrecedence(t *testing.T) {
	for _, tc := range colorEnabledCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envNoColor, tc.noColor)
			t.Setenv(envClicolorForce, tc.clicolorForce)
			t.Setenv(envForceColor, tc.forceColor)

			target := regularFile(t)
			if tc.charDevice {
				target = charDeviceFile(t)
			}
			if got := colorEnabled(target); got != tc.want {
				t.Errorf("colorEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// colorEnabledCase is one row of TestColorEnabledPrecedence.
type colorEnabledCase struct {
	name          string
	noColor       string
	clicolorForce string
	forceColor    string
	charDevice    bool
	want          bool
}

// colorEnabledCases covers the destination check, each force variable, the
// non-forcing "0", NO_COLOR alone, and NO_COLOR against a force.
func colorEnabledCases() []colorEnabledCase {
	return []colorEnabledCase{
		{name: "redirected, no variables", want: false},
		{name: "terminal, no variables", charDevice: true, want: true},
		{name: "redirected, CLICOLOR_FORCE", clicolorForce: "1", want: true},
		{name: "redirected, FORCE_COLOR", forceColor: "1", want: true},
		{name: "redirected, FORCE_COLOR=0 is not a force", forceColor: "0", want: false},
		{name: "terminal, FORCE_COLOR=0 leaves the check alone", forceColor: "0", charDevice: true, want: true},
		{name: "terminal, NO_COLOR", noColor: "1", charDevice: true, want: false},
		{name: "NO_COLOR beats CLICOLOR_FORCE", noColor: "1", clicolorForce: "1", want: false},
		{name: "NO_COLOR beats FORCE_COLOR", noColor: "1", forceColor: "1", want: false},
		{name: "NO_COLOR empty is unset", noColor: "", charDevice: true, want: true},
	}
}

// TestMarkersFollowTheirOwnDestination pins that stdout and stderr decide
// color independently: a redirected stdout gets plain markers while a
// terminal stderr on the same Progress gets colored ones.
func TestMarkersFollowTheirOwnDestination(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	p := newStreamProgress(false, false, false,
		stream{w: &out, color: false}, stream{w: &errOut, color: true})
	defer p.Close()

	p.Okf("x")
	p.Updatef("u")
	p.Errorf("y")
	p.Warnf("z")

	assertBuf(t, &out, okGlyph+" x\n"+updateGlyph+" u\n")
	if got, want := errOut.String(), failMark()+"y\n"+warnMark()+"z\n"; got != want {
		t.Errorf("errOut = %q, want %q", got, want)
	}
}

// TestPackageLevelHelpersFollowTheEnvironment pins that the package-level
// Okf and Errorf resolve color per call: plain under NO_COLOR, colored under
// FORCE_COLOR even on a pipe.
func TestPackageLevelHelpersFollowTheEnvironment(t *testing.T) {
	t.Setenv(envNoColor, "1")
	plain := capturePipe(t, &os.Stdout, func() { Okf("x") })
	if want := okGlyph + " x\n"; plain != want {
		t.Errorf("Okf under NO_COLOR = %q, want %q", plain, want)
	}

	t.Setenv(envNoColor, "")
	t.Setenv(envForceColor, "1")
	colored := capturePipe(t, &os.Stderr, func() { Errorf("y") })
	if want := failMark() + "y\n"; colored != want {
		t.Errorf("Errorf under FORCE_COLOR = %q, want %q", colored, want)
	}
}

// TestUpdateTierEmitsInEveryState pins that Updatef is a result tier: it
// emits on stdout in all four states, quiet included, since a --quiet CI run
// must still say which collections have a newer version.
func TestUpdateTierEmitsInEveryState(t *testing.T) {
	states := []struct {
		build  func() (*Progress, *bytes.Buffer, *bytes.Buffer)
		name   string
		prefix string
	}{
		{name: "A spinner", build: stateA, prefix: updateMark()},
		{name: "B verbose", build: stateB, prefix: updateMark()},
		{name: "C quiet", build: stateC, prefix: updateMark()},
		{name: "D non-TTY", build: stateD, prefix: updateGlyph + " "},
	}

	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			p, out, errOut := st.build()
			defer p.Close()
			p.Updatef("Outdated: ns.name 1.0.0 -> 2.0.0")
			assertBuf(t, out, st.prefix+"Outdated: ns.name 1.0.0 -> 2.0.0\n")
			assertEmpty(t, errOut)
		})
	}
}

// TestUpdateMarkerIsItsOwnGlyph pins that the update marker differs from the
// success, failure and warning glyphs, so a report says three distinct things.
func TestUpdateMarkerIsItsOwnGlyph(t *testing.T) {
	t.Parallel()
	for _, other := range []string{okGlyph, failGlyph, warnGlyph} {
		if updateGlyph == other {
			t.Fatalf("updateGlyph = %q, which is already some other tier's marker", updateGlyph)
		}
	}
}
