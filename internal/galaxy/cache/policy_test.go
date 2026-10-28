package cache

import (
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// policyTestOptions is an Options value whose three flags a case sets directly.
type policyTestOptions struct {
	noCache bool
	refresh bool
	offline bool
}

func (o policyTestOptions) IsNoCache() bool { return o.noCache }
func (o policyTestOptions) IsRefresh() bool { return o.refresh }
func (o policyTestOptions) IsOffline() bool { return o.offline }

// policyCase is one row of TestPolicyForConstraint.
type policyCase struct {
	opts  Options
	name  string
	want  Policy
	exact bool
}

// policyCases covers each flag alone, offline outranking the others, and the
// exact and floating constraint split.
func policyCases() []policyCase {
	ttl := helpers.CacheLatestMetadataTTL
	return []policyCase{
		{name: "nil options, exact", opts: nil, exact: true, want: Policy{Read: true, Write: true}},
		{name: "nil options, floating", opts: nil, exact: false, want: Policy{Read: true, Write: true}},
		{name: "defaults, exact", opts: policyTestOptions{}, exact: true, want: Policy{Read: true, Write: true}},
		{name: "defaults, floating", opts: policyTestOptions{}, exact: false, want: Policy{Read: true, Write: true, TTL: ttl}},
		{name: "offline, floating", opts: policyTestOptions{offline: true}, want: Policy{Read: true}},
		{
			name:  "offline outranks no-cache and refresh",
			opts:  policyTestOptions{offline: true, noCache: true, refresh: true},
			exact: true,
			want:  Policy{Read: true},
		},
		{name: "no-cache, floating", opts: policyTestOptions{noCache: true}, want: Policy{}},
		{name: "no-cache outranks refresh", opts: policyTestOptions{noCache: true, refresh: true}, want: Policy{}},
		{name: "refresh, floating", opts: policyTestOptions{refresh: true}, want: Policy{Write: true, TTL: ttl}},
		{name: "refresh, exact", opts: policyTestOptions{refresh: true}, exact: true, want: Policy{Read: true, Write: true}},
	}
}

// TestPolicyForConstraint pins each flag combination's policy: offline never
// writes, no-cache neither reads nor writes, and refresh skips reads only for
// a floating constraint, which alone carries a TTL.
func TestPolicyForConstraint(t *testing.T) {
	t.Parallel()
	for _, tc := range policyCases() {
		if got := PolicyForConstraint(tc.opts, tc.exact); got != tc.want {
			t.Errorf("%s: PolicyForConstraint = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
