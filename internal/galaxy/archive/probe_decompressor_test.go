package archive

// This file parses archive.go to check how ProbeTarGz builds its decompressor,
// and bounds its sizing constants and helpers.ArchiveProbeMaxBytes.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// findFuncDecl returns the top-level function named name declared in file, or
// nil when the file declares no such function.
func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// gzipstreamSizingArgOffset is how many leading arguments every gzipstream
// constructor takes before its sizing ones: the context, then the reader.
const gzipstreamSizingArgOffset = 2

// decompressorCall is one internal/gzipstream call site: the constructor
// name, and how each sizing argument is spelled in the source.
type decompressorCall struct {
	name string
	args []string
}

// decompressorCalls returns, in source order, every gzipstream call in fn with
// its sizing arguments, since a name alone says nothing about the budget. It
// looks only for gzipstream, as the gzipstream gate already bans pgzip here.
func decompressorCalls(fn *ast.FuncDecl) []decompressorCall {
	var found []decompressorCall
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return true
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent || pkg.Name != "gzipstream" {
			return true
		}
		var args []string
		for i, arg := range call.Args {
			if i < gzipstreamSizingArgOffset {
				continue // the context and the reader, not sizing arguments
			}
			args = append(args, argSpelling(arg))
		}
		found = append(found, decompressorCall{name: sel.Sel.Name, args: args})
		return true
	})
	return found
}

// argSpelling names how an argument is written: an identifier by name, a
// literal by its text, anything else as "expression". The gate needs spelling,
// not value: a call handed literals escapes the named constants' bound.
func argSpelling(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.BasicLit:
		return e.Value
	default:
		return "expression"
	}
}

// TestProbeTarGzUsesTheProbeSizedDecompressor pins that ProbeTarGz calls only
// gzipstream.NewReaderN, with probeGzipBlockSize and probeGzipBlocks. It reads
// the source because the reservation is unobservable: pgzip exposes no pool.
func TestProbeTarGzUsesTheProbeSizedDecompressor(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "archive.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing archive.go: %v", err)
	}
	fn := findFuncDecl(file, "ProbeTarGz")
	if fn == nil {
		t.Fatalf("archive.go declares no top-level ProbeTarGz to audit")
	}

	calls := decompressorCalls(fn)
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.name)
	}
	if !slices.Equal(names, []string{"NewReaderN"}) {
		t.Fatalf("ProbeTarGz builds its decompressor with %v, want exactly [NewReaderN]", names)
	}
	if got := calls[0].args; !slices.Equal(got, []string{"probeGzipBlockSize", "probeGzipBlocks"}) {
		t.Fatalf("ProbeTarGz sizes its decompressor with %v, want [probeGzipBlockSize probeGzipBlocks]", got)
	}
}

// TestProbeGzipSizingStaysWithinItsBudget bounds the constants the gate above
// requires: a block size above 512, since pgzip turns 512 or less into 1 MiB,
// and a hand-spelled 256 KiB total that the extractor's 4 MiB sizing exceeds.
func TestProbeGzipSizingStaysWithinItsBudget(t *testing.T) {
	t.Parallel()

	blockSize, blocks := probeGzipBlockSize, probeGzipBlocks
	if blockSize <= 512 {
		t.Fatalf("probeGzipBlockSize = %d, want above 512: pgzip.NewReaderN coerces "+
			"anything smaller to 1 MiB", blockSize)
	}
	if blocks < 1 {
		t.Fatalf("probeGzipBlocks = %d, want at least 1", blocks)
	}
	if blockSize*blocks > 256<<10 {
		t.Fatalf("the probe reserves %d bytes per reader, want at most %d", blockSize*blocks, 256<<10)
	}
}

// TestArchiveProbeMaxBytesClearsTheMetaHeaderCeiling pins
// helpers.ArchiveProbeMaxBytes to at least 4,196,352, the most archive/tar can
// be made to read before its first header, and at most 16 MiB.
func TestArchiveProbeMaxBytesClearsTheMetaHeaderCeiling(t *testing.T) {
	t.Parallel()

	const (
		metaHeaderCeiling = int64(4_196_352)
		probeBudget       = int64(16 << 20)
	)
	if helpers.ArchiveProbeMaxBytes < metaHeaderCeiling {
		t.Fatalf("ArchiveProbeMaxBytes = %d, want at least %d", helpers.ArchiveProbeMaxBytes, metaHeaderCeiling)
	}
	if helpers.ArchiveProbeMaxBytes > probeBudget {
		t.Fatalf("the probe reads up to %d bytes before refusing, want at most %d",
			helpers.ArchiveProbeMaxBytes, probeBudget)
	}
}
