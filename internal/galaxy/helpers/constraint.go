package helpers

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// ConstraintOperatorPattern and ConstraintVersionPattern are the vendored
// Masterminds v3.5.0 operator and constraint-version grammars, verbatim. The
// solver's mirror parser and CanonicalConstraint both compile them.
const (
	ConstraintOperatorPattern = `=||!=|>|<|>=|=>|<=|=<|~|~>|\^`
	ConstraintVersionPattern  = `v?([0-9|x|X|\*]+)(\.[0-9|x|X|\*]+)?(\.[0-9|x|X|\*]+)?` +
		`(-([0-9A-Za-z\-]+(\.[0-9A-Za-z\-]+)*))?` +
		`(\+([0-9A-Za-z\-]+(\.[0-9A-Za-z\-]+)*))?`
)

var (
	constraintClauseRegexp = regexp.MustCompile(`^\s*(` + ConstraintOperatorPattern + `)\s*(` + ConstraintVersionPattern + `)\s*$`)
	constraintFindRegexp   = regexp.MustCompile(`(` + ConstraintOperatorPattern + `)\s*(` + ConstraintVersionPattern + `)`)
	constraintRangeRegexp  = regexp.MustCompile(
		`\s*(` + ConstraintVersionPattern + `)\s+-\s+(` + ConstraintVersionPattern + `)\s*`)
)

// RewriteConstraintRange mirrors the vendored rewriteRange, turning every
// "A - B" into ">= A, <= B " before splitting. Groups 1 and 11 hold the two
// versions, since each version pattern contributes nine inner groups.
func RewriteConstraintRange(s string) string {
	m := constraintRangeRegexp.FindAllStringSubmatch(s, -1)
	if m == nil {
		return s
	}
	out := s
	for _, v := range m {
		out = strings.Replace(out, v[0], fmt.Sprintf(">= %s, <= %s ", v[1], v[11]), 1)
	}
	return out
}

// CanonicalConstraint respells NormalizeConstraint's result from the clauses
// semver parses: aliases collapsed, "=" bare but in a lone "=*", "," between
// clauses, no whitespace. Input semver refuses, either time, stays unrespelled.
func CanonicalConstraint(value string) string {
	normalized := NormalizeConstraint(value)
	if normalized == "" {
		return ""
	}
	if _, err := semver.NewConstraint(normalized); err != nil {
		return normalized
	}
	groups := strings.Split(RewriteConstraintRange(normalized), "||")
	for i, group := range groups {
		tokens := constraintFindRegexp.FindAllString(group, -1)
		for j, token := range tokens {
			m := constraintClauseRegexp.FindStringSubmatch(token)
			tokens[j] = canonicalOperator(m[1]) + m[2]
		}
		groups[i] = strings.Join(tokens, ",")
	}
	// A hyphen range respelled can outgrow semver.MaxConstraintLen.
	canonical := strings.Join(groups, "||")
	if _, err := semver.NewConstraint(canonical); err != nil {
		return normalized
	}
	// A lone "*" is match-all, prereleases included, where "=*" admits releases
	// alone, so that one keeps its operator.
	if canonical == "*" {
		return "=*"
	}
	return canonical
}

// canonicalOperator maps each operator the vendored table sends to one check
// function onto one spelling: "=>" to ">=", "=<" to "<=", "~>" to "~", and
// "=" to the bare form a requirements file most often carries.
func canonicalOperator(op string) string {
	switch op {
	case "=":
		return ""
	case "=>":
		return ">="
	case "=<":
		return "<="
	case "~>":
		return "~"
	default:
		return op
	}
}
