package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
	"go.yaml.in/yaml/v3"
)

// The two plaintexts the fixture carries. They are distinct strings so a
// rendering that leaks one but not the other cannot pass.
const (
	s3SecretPlaintext  = "top-secret"
	s3SessionPlaintext = "session-secret"
)

// s3RedactionFixture builds an S3CacheConfig carrying both credentials, plus
// the ordinary non-secret fields, so every assertion below runs against a
// struct shaped like the one a real run holds.
func s3RedactionFixture() S3CacheConfig {
	return S3CacheConfig{
		AccessKey:    "AKIAEXAMPLE",
		SecretKey:    NewSecret(s3SecretPlaintext),
		SessionToken: NewSecret(s3SessionPlaintext),
		Bucket:       "b",
		Region:       "r",
	}
}

// TestS3CacheConfigRedactsSecrets drives every rendering a Secret closes (%v,
// %+v, %#v, JSON, YAML) against the whole struct, the way a debug dump leaks;
// %#v needs Secret.GoString, since it reflects into the unexported field.
func TestS3CacheConfigRedactsSecrets(t *testing.T) {
	t.Parallel()
	cfg := s3RedactionFixture()

	// Encoding is the subject, so both linters are exempted: gosec flags
	// AccessKey, deliberately not a Secret, and musttag wants tags on a struct
	// production never encodes.
	//nolint:gosec,musttag // see above
	jsonBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	//nolint:gosec,musttag // same fixture, same reasons as the json.Marshal above
	yamlBytes, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	renderings := map[string]string{
		"%v":   fmt.Sprintf("%v", cfg),
		"%+v":  fmt.Sprintf("%+v", cfg),
		"%#v":  fmt.Sprintf("%#v", cfg),
		"json": string(jsonBytes),
		"yaml": string(yamlBytes),
	}
	for verb, out := range renderings {
		if strings.Contains(out, s3SecretPlaintext) {
			t.Errorf("%s output contains the plaintext secret key: %s", verb, out)
		}
		if strings.Contains(out, s3SessionPlaintext) {
			t.Errorf("%s output contains the plaintext session token: %s", verb, out)
		}
	}
}

// TestS3CacheConfigSecretsAreStillReadable is the positive control for
// TestS3CacheConfigRedactsSecrets: the same fixture's credentials are held
// and readable through Reveal, just not printable.
func TestS3CacheConfigSecretsAreStillReadable(t *testing.T) {
	t.Parallel()
	cfg := s3RedactionFixture()

	if got := cfg.SecretKey.Reveal(); got != s3SecretPlaintext {
		t.Errorf("SecretKey.Reveal() = %q, want %q", got, s3SecretPlaintext)
	}
	if got := cfg.SessionToken.Reveal(); got != s3SessionPlaintext {
		t.Errorf("SessionToken.Reveal() = %q, want %q", got, s3SessionPlaintext)
	}
}

// newS3Cmd builds a cli.Command carrying only the S3 flags loadS3CacheConfig
// reads, so a test can drive that loader without standing up the whole CLI.
func newS3Cmd(t *testing.T, args []string) *cli.Command {
	t.Helper()

	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "s3-bucket"},
			&cli.StringFlag{Name: "s3-prefix"},
			&cli.StringFlag{Name: "s3-endpoint"},
			&cli.StringFlag{Name: "s3-region"},
			&cli.StringFlag{Name: "s3-access-key"},
			&cli.StringFlag{Name: "s3-secret-key"},
			&cli.StringFlag{Name: "s3-session-token"},
			&cli.BoolFlag{Name: "s3-path-style-disabled"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}
	if err := cmd.Run(context.Background(), append([]string{"go-galaxy"}, args...)); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// TestLoadS3CacheConfigRequiresCredentials pins that a bucket without a
// secret key is helpers.ErrS3EmptyCreds, and that one with both credentials
// is accepted and carries the secret through, the refusal's positive control.
func TestLoadS3CacheConfigRequiresCredentials(t *testing.T) {
	t.Parallel()

	t.Run("missing secret key is refused", func(t *testing.T) {
		t.Parallel()
		c := newS3Cmd(t, []string{"--s3-bucket=b", "--s3-access-key=x"})

		err := loadS3CacheConfig(&Config{}, c, projectSettings{})

		if !errors.Is(err, helpers.ErrS3EmptyCreds) {
			t.Fatalf("loadS3CacheConfig = %v, want errors.Is helpers.ErrS3EmptyCreds", err)
		}
	})

	t.Run("both credentials are accepted", func(t *testing.T) {
		t.Parallel()
		c := newS3Cmd(t, []string{"--s3-bucket=b", "--s3-access-key=x", "--s3-secret-key=" + s3SecretPlaintext})

		var cfg Config
		if err := loadS3CacheConfig(&cfg, c, projectSettings{}); err != nil {
			t.Fatalf("loadS3CacheConfig = %v, want nil", err)
		}
		if !cfg.S3Cache.Enabled {
			t.Fatalf("Enabled = false, want true for a configured bucket")
		}
		if got := cfg.S3Cache.SecretKey.Reveal(); got != s3SecretPlaintext {
			t.Fatalf("SecretKey.Reveal() = %q, want %q", got, s3SecretPlaintext)
		}
	})
}
