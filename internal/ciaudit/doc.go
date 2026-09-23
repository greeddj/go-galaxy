// Package ciaudit holds only tests: they gate that the golangci-lint release
// pinned in .github/workflows/ci.yml is exact and equals the Justfile's
// GOLANGCI_LINT_VERSION. Checking the workflows themselves is actionlint's job.
package ciaudit
