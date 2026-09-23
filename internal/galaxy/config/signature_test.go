package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// signatureEnvNames returns every environment variable this surface reads,
// go-galaxy's own spellings and ansible's alike.
func signatureEnvNames() []string {
	return []string{
		"GO_GALAXY_KEYRING",
		"ANSIBLE_GALAXY_GPG_KEYRING",
		"GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT",
		"ANSIBLE_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT",
		"GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE",
		"ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES",
		"GO_GALAXY_DISABLE_GPG_VERIFY",
		envDisableGPGVerifyAnsible,
	}
}

// clearSignatureEnv unsets every signature variable, so a row's verdict does
// not depend on the machine; t.Setenv runs first only for the restore it
// registers, since there is no t.Unsetenv.
func clearSignatureEnv(t *testing.T) {
	t.Helper()
	for _, name := range signatureEnvNames() {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("os.Unsetenv(%q) error = %v, want nil", name, err)
		}
	}
}

// newSignatureCmd builds a command carrying the four signature flags shaped as
// cliflags declares them, with go-galaxy's own env spellings. It is a hand-built
// copy, since this package cannot import the command layer: it pins resolution.
func newSignatureCmd(t *testing.T, args []string) *cli.Command {
	t.Helper()

	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "keyring", Sources: cli.EnvVars("GO_GALAXY_KEYRING")},
			&cli.StringFlag{
				Name:    "required-valid-signature-count",
				Value:   helpers.DefaultRequiredValidSignatureCount,
				Sources: cli.EnvVars("GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT"),
			},
			&cli.StringSliceFlag{
				Name:    "ignore-signature-status-code",
				Sources: cli.EnvVars("GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE"),
			},
			&cli.BoolFlag{Name: "disable-gpg-verify", Sources: cli.EnvVars("GO_GALAXY_DISABLE_GPG_VERIFY")},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}

	fullArgs := append([]string{"go-galaxy"}, args...)
	if err := cmd.Run(context.Background(), fullArgs); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// signatureRow is one resolution scenario: an exported variable and a command
// line; a row leaving both empty expects the flag's own default.
type signatureRow struct {
	name     string
	envName  string
	envValue string
	args     []string
}

// resolveSignature runs one row through applySignatureConfig and returns the
// resolved Config, failing the test if the resolution was refused.
func resolveSignature(t *testing.T, row signatureRow) *Config {
	t.Helper()
	clearSignatureEnv(t)
	if row.envName != "" {
		t.Setenv(row.envName, row.envValue)
	}

	cfg := &Config{}
	if err := applySignatureConfig(cfg, newSignatureCmd(t, row.args)); err != nil {
		t.Fatalf("applySignatureConfig() error = %v, want nil", err)
	}
	return cfg
}

// keyringRow is a signatureRow plus the keyring path the resolution must land
// on.
type keyringRow struct {
	want string
	row  signatureRow
}

// TestApplySignatureConfigKeyringPrecedence pins that a set flag beats the
// environment, which beats the default; each row adds the higher source on top
// of the lower one, so a pass also proves the lower source was present and lost.
func TestApplySignatureConfigKeyringPrecedence(t *testing.T) {
	rows := []keyringRow{
		{row: signatureRow{name: "nothing supplies it"}, want: ""},
		{
			row: signatureRow{
				name:    "the environment supplies it",
				envName: "GO_GALAXY_KEYRING", envValue: "/env.gpg",
			},
			want: "/env.gpg",
		},
		{
			row: signatureRow{
				name:    "the flag outranks the environment",
				envName: "GO_GALAXY_KEYRING", envValue: "/env.gpg",
				args: []string{"--keyring=/flag.gpg"},
			},
			want: "/flag.gpg",
		},
	}

	for _, row := range rows {
		t.Run(row.row.name, func(t *testing.T) {
			if got := resolveSignature(t, row.row).Signature.KeyringPath; got != row.want {
				t.Fatalf("KeyringPath = %q, want %q", got, row.want)
			}
		})
	}
}

// countRow is a signatureRow plus the count spec the resolution must land on.
type countRow struct {
	want string
	row  signatureRow
}

// TestApplySignatureConfigRequiredCountPrecedence pins the count spec's
// precedence like the keyring rows, using distinct spellings of the grammar so
// no row can pass on a value another source supplied.
func TestApplySignatureConfigRequiredCountPrecedence(t *testing.T) {
	rows := []countRow{
		{row: signatureRow{name: "nothing supplies it"}, want: helpers.DefaultRequiredValidSignatureCount},
		{
			row: signatureRow{
				name:    "the environment supplies it",
				envName: "GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT", envValue: "+2",
			},
			want: "+2",
		},
		{
			row: signatureRow{
				name:    "the flag outranks the environment",
				envName: "GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT", envValue: "+2",
				args: []string{"--required-valid-signature-count=+all"},
			},
			want: "+all",
		},
	}

	for _, row := range rows {
		t.Run(row.row.name, func(t *testing.T) {
			if got := resolveSignature(t, row.row).Signature.RequiredCount; got != row.want {
				t.Fatalf("RequiredCount = %q, want %q", got, row.want)
			}
		})
	}
}

// codesRow is a signatureRow plus the tolerated-status list the resolution
// must land on.
type codesRow struct {
	row  signatureRow
	want []string
}

// TestApplySignatureConfigIgnoreStatusCodesPrecedence pins the ignore list's
// precedence, and that elements are handed on exactly as the CLI library split
// them: the space after the environment value's comma survives.
func TestApplySignatureConfigIgnoreStatusCodesPrecedence(t *testing.T) {
	rows := []codesRow{
		{row: signatureRow{name: "nothing supplies it"}, want: nil},
		{
			row: signatureRow{
				name:    "the environment supplies it",
				envName: "GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE", envValue: "BADSIG, NO_PUBKEY",
			},
			want: []string{"BADSIG", " NO_PUBKEY"},
		},
		{
			row: signatureRow{
				name:    "the flag outranks the environment",
				envName: "GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE", envValue: "BADSIG, NO_PUBKEY",
				args: []string{"--ignore-signature-status-code=EXPSIG"},
			},
			want: []string{"EXPSIG"},
		},
	}

	for _, row := range rows {
		t.Run(row.row.name, func(t *testing.T) {
			if got := resolveSignature(t, row.row).Signature.IgnoreStatusCodes; !slices.Equal(got, row.want) {
				t.Fatalf("IgnoreStatusCodes = %q, want %q", got, row.want)
			}
		})
	}
}

// disableRow is one ANSIBLE_GALAXY_DISABLE_GPG_VERIFY value and the verdict it
// must resolve to, or the refusal it must produce instead.
type disableRow struct {
	name     string
	value    string
	want     bool
	wantErr  bool
	unsetEnv bool
}

// TestApplySignatureConfigDisableAnsibleEnv pins that this layer reads the
// ansible variable in ansible's vocabulary (yes/no/on/off included, which a
// urfave/cli bool source would abort on), empty as absent, anything else refused.
func TestApplySignatureConfigDisableAnsibleEnv(t *testing.T) {
	rows := []disableRow{
		{name: "unset is false", unsetEnv: true},
		{name: "empty reads as absent", value: ""},
		{name: "true", value: "true", want: true},
		{name: "false", value: "false"},
		{name: "yes", value: "yes", want: true},
		{name: "no", value: "no"},
		{name: "on", value: "on", want: true},
		{name: "off", value: "off"},
		{name: "mixed case is accepted", value: "YeS", want: true},
		{name: "an unparseable value is refused", value: "maybe", wantErr: true},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearSignatureEnv(t)
			if !row.unsetEnv {
				t.Setenv(envDisableGPGVerifyAnsible, row.value)
			}

			cfg := &Config{}
			err := applySignatureConfig(cfg, newSignatureCmd(t, nil))
			if row.wantErr {
				if !errors.Is(err, helpers.ErrInvalidDisableGPGVerify) {
					t.Fatalf("applySignatureConfig() error = %v, want %v", err, helpers.ErrInvalidDisableGPGVerify)
				}
				return
			}
			if err != nil {
				t.Fatalf("applySignatureConfig() error = %v, want nil", err)
			}
			if cfg.Signature.DisableGPGVerify != row.want {
				t.Fatalf("DisableGPGVerify = %v, want %v", cfg.Signature.DisableGPGVerify, row.want)
			}
		})
	}
}

// TestApplySignatureConfigDisableFlagOutranksAnsibleEnv pins that the flag wins
// over the ansible variable whenever some source set it; the two disagree, so
// only reading the flag passes.
func TestApplySignatureConfigDisableFlagOutranksAnsibleEnv(t *testing.T) {
	clearSignatureEnv(t)
	t.Setenv(envDisableGPGVerifyAnsible, "no")

	cfg := &Config{}
	if err := applySignatureConfig(cfg, newSignatureCmd(t, []string{"--disable-gpg-verify"})); err != nil {
		t.Fatalf("applySignatureConfig() error = %v, want nil", err)
	}
	if !cfg.Signature.DisableGPGVerify {
		t.Fatal("DisableGPGVerify = false, want true (the flag outranks the ansible variable)")
	}
}

// TestExpandHome pins the whole rule: "~" and a "~/" prefix expand, every other
// shape ("~user", a tilde past the first element) is returned unchanged. $HOME
// is redirected so the expansion has a value this test knows.
func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rows := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty stays empty", in: "", want: ""},
		{name: "bare tilde becomes the home directory", in: "~", want: home},
		{name: "tilde slash is joined onto it", in: "~/keys.gpg", want: filepath.Join(home, "keys.gpg")},
		{name: "another user's tilde form is untouched", in: "~other/keys.gpg", want: "~other/keys.gpg"},
		{name: "a tilde that is not the first element is untouched", in: "a/~", want: "a/~"},
		{name: "an absolute path is untouched", in: "/etc/keys.gpg", want: "/etc/keys.gpg"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := expandHome(row.in); got != row.want {
				t.Fatalf("expandHome(%q) = %q, want %q", row.in, got, row.want)
			}
		})
	}
}

// validationRow is one malformed signature setting and the sentinel the config
// build must refuse it with, or - for the positive control - a row carrying no
// defect at all.
type validationRow struct {
	wantErr error
	name    string
	args    []string
}

// TestApplySignatureConfigValidation pins that a signature setting no consumer
// could act on is refused at config time under a named sentinel; the last row,
// the same fixture with valid values, is the control that the check is reached.
func TestApplySignatureConfigValidation(t *testing.T) {
	rows := []validationRow{
		{
			name:    "a malformed count spec is refused",
			args:    []string{"--required-valid-signature-count=some"},
			wantErr: helpers.ErrInvalidSignatureCount,
		},
		{
			name:    "an unknown status code is refused",
			args:    []string{"--ignore-signature-status-code=NOT_A_CODE"},
			wantErr: helpers.ErrUnknownSignatureStatusCode,
		},
		{
			name: "the same fixture with valid values builds cleanly",
			args: []string{"--required-valid-signature-count=+1", "--ignore-signature-status-code=NO_PUBKEY"},
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearSignatureEnv(t)
			cfg := &Config{}
			err := applySignatureConfig(cfg, newSignatureCmd(t, row.args))
			if row.wantErr == nil {
				if err != nil {
					t.Fatalf("applySignatureConfig() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, row.wantErr) {
				t.Fatalf("applySignatureConfig() error = %v, want %v", err, row.wantErr)
			}
		})
	}
}

// emptyValueRow is one way a setting can arrive empty, and whether the config
// build must refuse it.
type emptyValueRow struct {
	name     string
	envName  string
	args     []string
	wantErr  bool
	setEmpty bool
}

// TestApplySignatureConfigRefusesEmptyValues pins that a keyring or count set
// empty by flag or environment (a withheld CI secret) is refused, unlike an
// omission, while an empty switch or ignore list, safe in meaning, resolves.
func TestApplySignatureConfigRefusesEmptyValues(t *testing.T) {
	rows := []emptyValueRow{
		{name: "an empty keyring flag is refused", args: []string{"--keyring="}, wantErr: true},
		{name: "an empty keyring variable is refused", envName: "GO_GALAXY_KEYRING", setEmpty: true, wantErr: true},
		{name: "an empty count flag is refused", args: []string{"--required-valid-signature-count="}, wantErr: true},
		{
			name:    "an empty count variable is refused",
			envName: "GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT", setEmpty: true, wantErr: true,
		},
		{name: "an omitted keyring is accepted"},
		{name: "an omitted count is accepted"},
		{name: "an empty disable variable is accepted", envName: "GO_GALAXY_DISABLE_GPG_VERIFY", setEmpty: true},
		{
			name:    "an empty ignore list variable is accepted",
			envName: "GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE", setEmpty: true,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearSignatureEnv(t)
			if row.setEmpty {
				t.Setenv(row.envName, "")
			}

			err := applySignatureConfig(&Config{}, newSignatureCmd(t, row.args))
			if row.wantErr {
				if !errors.Is(err, helpers.ErrEmptySignatureValue) {
					t.Fatalf("applySignatureConfig() error = %v, want %v", err, helpers.ErrEmptySignatureValue)
				}
				return
			}
			if err != nil {
				t.Fatalf("applySignatureConfig() error = %v, want nil", err)
			}
		})
	}
}

// TestApplySignatureConfigEmptyCountKeepsNoDefault pins that an empty count is
// refused rather than silently replaced by the default, which could weaken a
// stricter policy the operator configured.
func TestApplySignatureConfigEmptyCountKeepsNoDefault(t *testing.T) {
	clearSignatureEnv(t)
	t.Setenv("GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT", "")

	cfg := &Config{}
	err := applySignatureConfig(cfg, newSignatureCmd(t, nil))
	if !errors.Is(err, helpers.ErrEmptySignatureValue) {
		t.Fatalf("applySignatureConfig() error = %v, want %v", err, helpers.ErrEmptySignatureValue)
	}
	if cfg.Signature.RequiredCount == helpers.DefaultRequiredValidSignatureCount {
		t.Fatalf("RequiredCount = %q, want the run refused rather than defaulted", cfg.Signature.RequiredCount)
	}
}

// warningRow is one combination of the switch and the keyring, and whether the
// run must say something about it.
type warningRow struct {
	name        string
	args        []string
	wantWarning bool
}

// TestApplySignatureConfigDisabledWithKeyringWarning pins the warning for
// verification switched off while a keyring is configured; the two silent rows
// keep "it warned" distinguishable from "it warns on every run".
func TestApplySignatureConfigDisabledWithKeyringWarning(t *testing.T) {
	rows := []warningRow{
		{
			name:        "disabled with a keyring warns",
			args:        []string{"--disable-gpg-verify", "--keyring=/keys.gpg"},
			wantWarning: true,
		},
		{name: "disabled with no keyring is silent", args: []string{"--disable-gpg-verify"}},
		{name: "a keyring with verification on is silent", args: []string{"--keyring=/keys.gpg"}},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearSignatureEnv(t)
			cfg := &Config{}
			if err := applySignatureConfig(cfg, newSignatureCmd(t, row.args)); err != nil {
				t.Fatalf("applySignatureConfig() error = %v, want nil", err)
			}
			if got := len(cfg.Warnings) > 0; got != row.wantWarning {
				t.Fatalf("warnings = %v, want a warning: %v", cfg.Warnings, row.wantWarning)
			}
		})
	}
}

// newFlaglessCmd builds a command registering no flags, which is how a command
// that does not verify (cleanup, lock, outdated) looks to this resolution.
func newFlaglessCmd(t *testing.T) *cli.Command {
	t.Helper()

	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"go-galaxy"}); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// TestApplySignatureConfigCountFallback pins that a command registering none of
// these flags still resolves the count to the default spec, not to "". The
// literal "1" makes changing the default a decision this test reports.
func TestApplySignatureConfigCountFallback(t *testing.T) {
	clearSignatureEnv(t)

	cfg := &Config{}
	err := applySignatureConfig(cfg, newFlaglessCmd(t))
	if got := cfg.Signature.RequiredCount; got != "1" {
		t.Fatalf("RequiredCount = %q, want %q", got, "1")
	}
	if err != nil {
		t.Fatalf("applySignatureConfig() error = %v, want nil", err)
	}
}
