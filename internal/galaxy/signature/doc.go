// Package signature verifies detached OpenPGP signatures over MANIFEST.json.
// It only reads (the keyring, a file:// source), fetches through a client with
// no credential or relaxed TLS, and persists no verdict.
package signature
