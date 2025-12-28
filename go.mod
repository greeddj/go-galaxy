module github.com/greeddj/go-galaxy

go 1.25.5

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/Masterminds/semver v1.5.0
	github.com/briandowns/spinner v1.23.2
	github.com/klauspost/pgzip v1.2.6
	github.com/psvmcc/hub v0.0.7
	github.com/urfave/cli/v2 v2.27.7
	go.etcd.io/bbolt v1.4.3
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/cpuguy83/go-md2man/v2 v2.0.7 // indirect
	github.com/fatih/color v1.7.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/klauspost/compress v1.18.2 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/xrash/smetrics v0.0.0-20250705151800-55b8f293f342 // indirect
	golang.org/x/exp/typeparams v0.0.0-20251219203646-944ab1f22d93 // indirect
	golang.org/x/mod v0.31.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/telemetry v0.0.0-20251222180846-3f2a21fb04ff // indirect
	golang.org/x/term v0.29.0 // indirect
	golang.org/x/tools v0.40.0 // indirect
	golang.org/x/tools/go/expect v0.1.1-deprecated // indirect
	golang.org/x/tools/go/packages/packagestest v0.1.1-deprecated // indirect
	golang.org/x/vuln v1.1.4 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	honnef.co/go/tools v0.6.1 // indirect
)

tool (
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
