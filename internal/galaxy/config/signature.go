package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/signature"
	"github.com/urfave/cli/v3"
)

// envDisableGPGVerifyAnsible is ansible's own name for the disable switch. It
// is read in this package rather than declared as a flag source; see
// resolveDisableGPGVerify for why.
const envDisableGPGVerifyAnsible = "ANSIBLE_GALAXY_DISABLE_GPG_VERIFY"

// SignatureConfig is one run's resolved signature verification settings. It
// holds raw values rather than a signature.Policy, because the consumer builds
// that policy beside the key material it loads; field names match the policy's.
type SignatureConfig struct {
	// KeyringPath is the keyring location with a leading "~" already expanded.
	// Empty means no keyring was configured, which is a run that verifies
	// nothing rather than one that verifies and fails.
	KeyringPath string
	// RequiredCount is the count spec as written, in signature.ParseCountSpec's
	// grammar ("all", the "+" marker); never empty after applySignatureConfig.
	RequiredCount string
	// IgnoreStatusCodes names the verification failure statuses this run
	// tolerates, one element per configured code, unvalidated.
	IgnoreStatusCodes []string
	// DisableGPGVerify switches verification off even when a keyring is
	// configured, which is a different fact from having configured no keyring.
	DisableGPGVerify bool
}

// applySignatureConfig resolves cfg.Signature from flags and their environment
// variables only: a setting that can relax verification must never come from
// an ansible.cfg whose author (operator or repository under test) is unknown.
func applySignatureConfig(cfg *Config, c *cli.Command) error {
	if err := checkSuppliedSignatureValues(c); err != nil {
		return err
	}

	cfg.Signature.KeyringPath = expandHome(c.String("keyring"))
	cfg.Signature.RequiredCount = resolveRequiredCount(c)
	cfg.Signature.IgnoreStatusCodes = resolveIgnoreStatusCodes(c)

	disabled, err := resolveDisableGPGVerify(c)
	if err != nil {
		return err
	}
	cfg.Signature.DisableGPGVerify = disabled

	if warning := disabledWithKeyringWarning(cfg.Signature); warning != "" {
		cfg.Warnings = append(cfg.Warnings, warning)
	}

	return validateSignatureConfig(cfg.Signature)
}

// AnsibleSignatureKeysWarning names the signature keys a discovered ansible.cfg
// carried (never a value; none was read), or returns "". It is not queued on
// Warnings, which every command drains: only the verifying commands print it.
func (c *Config) AnsibleSignatureKeysWarning() string {
	if c == nil || len(c.AnsibleSignatureKeys) == 0 {
		return ""
	}
	named := make([]string, 0, len(signatureKeyNames))
	for _, key := range signatureKeyNames {
		if slices.Contains(c.AnsibleSignatureKeys, key) {
			named = append(named, key)
		}
	}

	return fmt.Sprintf(
		"%s configures signature verification (%s); go-galaxy reads none of it - "+
			"configure the keyring and its policy through --keyring and its sibling flags, or their environment variables",
		c.AnsibleConfigPath, strings.Join(named, ", "))
}

// emptyRefusingSignatureFlag is one flag whose explicitly supplied empty value
// is refused, together with the remedy its refusal names.
type emptyRefusingSignatureFlag struct {
	name   string
	remedy string
}

// checkSuppliedSignatureValues refuses a keyring or count some source set to ""
// (IsSet tells that from an omission): an empty CI secret must not silently mean
// "verify nothing" or replace a configured count with the default.
func checkSuppliedSignatureValues(c *cli.Command) error {
	flags := [...]emptyRefusingSignatureFlag{
		{name: "keyring", remedy: "omit it entirely to run without signature verification"},
		{name: "required-valid-signature-count", remedy: "omit it entirely to use the default"},
	}

	for _, flag := range flags {
		if c.IsSet(flag.name) && c.String(flag.name) == "" {
			return fmt.Errorf("%w: --%s (or the environment variable feeding it) is empty; %s",
				helpers.ErrEmptySignatureValue, flag.name, flag.remedy)
		}
	}

	return nil
}

// resolveRequiredCount returns the count spec, or the default when no source
// supplied one. The fallback is load-bearing: a command that does not register
// the flag reads "", which validateSignatureConfig would refuse on every run.
func resolveRequiredCount(c *cli.Command) string {
	if count := c.String("required-valid-signature-count"); count != "" {
		return count
	}

	return helpers.DefaultRequiredValidSignatureCount
}

// resolveIgnoreStatusCodes passes the tolerated-status list on untrimmed, as
// urfave/cli split it: signature.ParseStatusCodes is the single authority over
// what an element may be.
func resolveIgnoreStatusCodes(c *cli.Command) []string {
	return c.StringSlice("ignore-signature-status-code")
}

// resolveDisableGPGVerify lets the flag win when any source set it, else reads
// envDisableGPGVerifyAnsible in ansible's boolean vocabulary, which a urfave/cli
// bool source would abort on ("yes", "off"); empty is false, unparseable refused.
func resolveDisableGPGVerify(c *cli.Command) (bool, error) {
	// A command without the flag verifies nothing, so it ignores the variable
	// just as it ignores the flag's own sources.
	if !registersFlag(c, "disable-gpg-verify") {
		return false, nil
	}
	if c.IsSet("disable-gpg-verify") {
		return c.Bool("disable-gpg-verify"), nil
	}

	raw := os.Getenv(envDisableGPGVerifyAnsible)
	if raw == "" {
		return false, nil
	}
	disabled, ok := parseAnsibleBool(raw)
	if !ok {
		return false, fmt.Errorf("%w: $%s = %q", helpers.ErrInvalidDisableGPGVerify, envDisableGPGVerifyAnsible, raw)
	}

	return disabled, nil
}

// registersFlag reports whether c or an ancestor, the lineage urfave/cli's own
// lookup walks, declares the named flag. IsSet cannot tell: it reports an
// undeclared flag and a declared one no source set alike, as unset.
func registersFlag(c *cli.Command, name string) bool {
	for _, cmd := range c.Lineage() {
		if slices.ContainsFunc(cmd.Flags, func(flag cli.Flag) bool { return slices.Contains(flag.Names(), name) }) {
			return true
		}
	}

	return false
}

// disabledWithKeyringWarning warns when verification is off while a keyring is
// configured, a run that would otherwise look fully verified. With no keyring
// nothing was going to be verified, so warning then would be noise on every run.
func disabledWithKeyringWarning(sc SignatureConfig) string {
	if !sc.DisableGPGVerify || sc.KeyringPath == "" {
		return ""
	}

	return fmt.Sprintf(
		"signature verification is disabled while a keyring is configured (%q); no collection will be verified on this run",
		sc.KeyringPath)
}

// expandHome expands a bare "~" or a "~/" prefix and returns anything else
// unchanged, "~user" and "$VAR" included, as it does when the home directory
// is unknown: the keyring open then fails naming the literal path.
func expandHome(path string) string {
	if path == "" {
		return path
	}
	rest, isHomeRelative := strings.CutPrefix(path, "~/")
	if path != "~" && !isHomeRelative {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}

	return filepath.Join(home, rest)
}

// validateSignatureConfig builds the policy the consumer will build and keeps
// only the error, so a bad count or status code fails at config time (exit 2),
// not in an install worker; the consumer must still check its own NewPolicy.
func validateSignatureConfig(sc SignatureConfig) error {
	_, err := signature.NewPolicy(sc.KeyringPath, sc.RequiredCount, sc.IgnoreStatusCodes, sc.DisableGPGVerify)

	return err
}
