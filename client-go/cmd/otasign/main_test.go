package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mike10010100/transit-tracker/client-go/internal/otasig"
)

func TestRunUsageAndHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != exitUsage {
		t.Fatalf("run(nil) = %d, want %d", code, exitUsage)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"help"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("run(help) = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout.String(), "usage:") {
		t.Errorf("expected usage in stdout, got %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"unknown-subcmd"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("run(unknown) = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("expected unknown subcommand in stderr, got %q", stderr.String())
	}
}

func TestKeygenAndPubkey(t *testing.T) {
	td := t.TempDir()
	keyPath := filepath.Join(td, "sub", "test.key")

	var stdout, stderr bytes.Buffer
	// Missing flag
	if code := run([]string{"keygen"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("keygen without -out = %d, want %d", code, exitUsage)
	}

	// Successful keygen
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"keygen", "-out", keyPath}, &stdout, &stderr); code != exitOK {
		t.Fatalf("keygen failed: %d, stderr: %s", code, stderr.String())
	}
	pubB64Keygen := strings.TrimSpace(stdout.String())
	if pubB64Keygen == "" {
		t.Fatal("expected pubkey in stdout from keygen")
	}

	// Permissions check
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("keyfile perm = %o, want 0600", perm)
	}

	// Refuse overwrite
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"keygen", "-out", keyPath}, &stdout, &stderr); code != exitFail {
		t.Fatalf("keygen overwrite = %d, want %d", code, exitFail)
	}

	// Pubkey missing flag
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"pubkey"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("pubkey without -key = %d, want %d", code, exitUsage)
	}

	// Pubkey extraction
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"pubkey", "-key", keyPath}, &stdout, &stderr); code != exitOK {
		t.Fatalf("pubkey failed: %d, stderr: %s", code, stderr.String())
	}
	pubB64Pubkey := strings.TrimSpace(stdout.String())
	if pubB64Pubkey != pubB64Keygen {
		t.Fatalf("pubkey mismatch: %q != %q", pubB64Pubkey, pubB64Keygen)
	}

	// Pubkey missing file
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"pubkey", "-key", filepath.Join(td, "missing.key")}, &stdout, &stderr); code != exitFail {
		t.Fatalf("pubkey missing file = %d, want %d", code, exitFail)
	}
}

func TestSignVerifyAndServerCert(t *testing.T) {
	td := t.TempDir()
	relKeyPath := filepath.Join(td, "release.key")
	binPath := filepath.Join(td, "tracker-arm")
	manPath := filepath.Join(td, "tracker-arm.manifest.json")
	binData := []byte("fake-arm-binary-payload")
	if err := os.WriteFile(binPath, binData, 0755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	// Generate release key
	if code := run([]string{"keygen", "-out", relKeyPath}, &stdout, &stderr); code != exitOK {
		t.Fatalf("keygen failed: %d, stderr: %s", code, stderr.String())
	}
	relPubB64 := strings.TrimSpace(stdout.String())

	// Sign with missing flags
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"sign"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("sign missing flags = %d, want %d", code, exitUsage)
	}

	// Sign with invalid version
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"sign", "-key", relKeyPath, "-version", "1.0", "-in", binPath, "-out", manPath}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("sign bad version = %d, want %d", code, exitUsage)
	}

	// Sign valid
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"sign", "-key", relKeyPath, "-version", "1.2.3", "-in", binPath, "-out", manPath}, &stdout, &stderr); code != exitOK {
		t.Fatalf("sign valid = %d, stderr: %s", code, stderr.String())
	}

	// Verify missing flags
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("verify missing flags = %d, want %d", code, exitUsage)
	}

	// Verify valid
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", "-pub", relPubB64, "-manifest", manPath, "-in", binPath}, &stdout, &stderr); code != exitOK {
		t.Fatalf("verify valid = %d, stderr: %s", code, stderr.String())
	}

	// Verify with corrupted binary
	badBinPath := filepath.Join(td, "bad-arm")
	if err := os.WriteFile(badBinPath, []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", "-pub", relPubB64, "-manifest", manPath, "-in", badBinPath}, &stdout, &stderr); code != exitFail {
		t.Fatalf("verify tampered binary = %d, want %d", code, exitFail)
	}

	// Server-cert missing flags
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"server-cert"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("server-cert missing flags = %d, want %d", code, exitUsage)
	}

	// Server-cert valid
	srvKeyPath := filepath.Join(td, "server.key")
	srvCertPath := filepath.Join(td, "server.cert.json")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"server-cert", "-key", relKeyPath, "-out-key", srvKeyPath, "-out-cert", srvCertPath}, &stdout, &stderr); code != exitOK {
		t.Fatalf("server-cert valid = %d, stderr: %s", code, stderr.String())
	}

	// Read and verify minted certificate
	certBytes, err := os.ReadFile(srvCertPath)
	if err != nil {
		t.Fatal(err)
	}
	relPubBytes, _ := base64.StdEncoding.DecodeString(relPubB64)
	if _, _, err := otasig.VerifyCert(ed25519.PublicKey(relPubBytes), certBytes); err != nil {
		t.Fatalf("VerifyCert failed: %v", err)
	}

	// Server-cert can overwrite existing cert and key on re-mint
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"server-cert", "-key", relKeyPath, "-out-key", srvKeyPath, "-out-cert", srvCertPath}, &stdout, &stderr); code != exitOK {
		t.Fatalf("server-cert re-mint = %d, stderr: %s", code, stderr.String())
	}

	// Verify size mismatch
	shortBinPath := filepath.Join(td, "short-arm")
	if err := os.WriteFile(shortBinPath, []byte("short"), 0755); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", "-pub", relPubB64, "-manifest", manPath, "-in", shortBinPath}, &stdout, &stderr); code != exitFail {
		t.Fatalf("verify size mismatch = %d, want %d", code, exitFail)
	}

	// Verify bad pubkey
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", "-pub", "not-base64", "-manifest", manPath, "-in", binPath}, &stdout, &stderr); code != exitFail {
		t.Fatalf("verify bad pubkey = %d, want %d", code, exitFail)
	}

	// Verify missing manifest
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", "-pub", relPubB64, "-manifest", filepath.Join(td, "missing.json"), "-in", binPath}, &stdout, &stderr); code != exitFail {
		t.Fatalf("verify missing manifest = %d, want %d", code, exitFail)
	}

	// Verify SHA-256 mismatch when size matches
	sameSizeBadBinPath := filepath.Join(td, "samesize-bad-arm")
	if err := os.WriteFile(sameSizeBadBinPath, []byte("fake-arm-binary-tampr!!"), 0755); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", "-pub", relPubB64, "-manifest", manPath, "-in", sameSizeBadBinPath}, &stdout, &stderr); code != exitFail {
		t.Fatalf("verify sha256 mismatch = %d, want %d", code, exitFail)
	}

	// Verify missing binary to hash
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", "-pub", relPubB64, "-manifest", manPath, "-in", filepath.Join(td, "nonexistent-bin")}, &stdout, &stderr); code != exitFail {
		t.Fatalf("verify missing binary = %d, want %d", code, exitFail)
	}

	// Verify corrupt manifest JSON
	corruptManPath := filepath.Join(td, "corrupt.manifest.json")
	if err := os.WriteFile(corruptManPath, []byte("{corrupted"), 0644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"verify", "-pub", relPubB64, "-manifest", corruptManPath, "-in", binPath}, &stdout, &stderr); code != exitFail {
		t.Fatalf("verify corrupt manifest = %d, want %d", code, exitFail)
	}

	// Sign missing binary
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"sign", "-key", relKeyPath, "-version", "1.2.3", "-in", filepath.Join(td, "nonexistent-bin"), "-out", manPath}, &stdout, &stderr); code != exitFail {
		t.Fatalf("sign missing binary = %d, want %d", code, exitFail)
	}

	// Sign writeAtomic error
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"sign", "-key", relKeyPath, "-version", "1.2.3", "-in", binPath, "-out", "/dev/null/impossible/man.json"}, &stdout, &stderr); code != exitFail {
		t.Fatalf("sign writeAtomic failure = %d, want %d", code, exitFail)
	}

	// Server-cert writeAtomic error on outKey
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"server-cert", "-key", relKeyPath, "-out-key", "/dev/null/impossible/key", "-out-cert", srvCertPath}, &stdout, &stderr); code != exitFail {
		t.Fatalf("server-cert writeAtomic outKey = %d, want %d", code, exitFail)
	}

	// Server-cert writeAtomic error on outCert
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"server-cert", "-key", relKeyPath, "-out-key", srvKeyPath, "-out-cert", "/dev/null/impossible/cert"}, &stdout, &stderr); code != exitFail {
		t.Fatalf("server-cert writeAtomic outCert = %d, want %d", code, exitFail)
	}
}

type errReader struct{}

func (errReader) Read(p []byte) (n int, err error) {
	return 0, os.ErrPermission
}

func TestKeygenRandError(t *testing.T) {
	td := t.TempDir()
	origRand := randReader
	randReader = errReader{}
	defer func() { randReader = origRand }()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"keygen", "-out", filepath.Join(td, "err.key")}, &stdout, &stderr); code != exitFail {
		t.Fatalf("keygen with broken rand = %d, want %d", code, exitFail)
	}
}

func TestWriteAtomicAndSecret(t *testing.T) {
	td := t.TempDir()
	filePath := filepath.Join(td, "sub", "test.txt")

	if err := writeAtomic(filePath, []byte("hello"), 0644); err != nil {
		t.Fatalf("writeAtomic failed: %v", err)
	}
	data, _ := os.ReadFile(filePath)
	if string(data) != "hello" {
		t.Errorf("read %q, want 'hello'", string(data))
	}

	// writeNewSecret refuses overwrite
	if err := writeNewSecret(filePath, []byte("secret")); err == nil {
		t.Fatal("expected error on overwriting secret")
	}

	// hashFile missing file
	if _, _, err := hashFile(filepath.Join(td, "nonexistent")); err == nil {
		t.Fatal("expected error hashing nonexistent file")
	}
}

func TestMissingFlagsAndErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer

	// sign missing flags
	if code := run([]string{"sign"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("sign without flags = %d, want exitUsage", code)
	}
	if code := run([]string{"sign", "-key", "k", "-version", "1.0.0", "-in", "in"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("sign missing out = %d, want exitUsage", code)
	}
	if code := run([]string{"sign", "-key", "k", "-version", "invalid", "-in", "in", "-out", "out"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("sign invalid version = %d, want exitUsage", code)
	}

	// server-cert missing flags
	if code := run([]string{"server-cert"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("server-cert without flags = %d, want exitUsage", code)
	}
	if code := run([]string{"server-cert", "-key", "k", "-out-key", "ok"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("server-cert missing out-cert = %d, want exitUsage", code)
	}

	// verify missing flags
	if code := run([]string{"verify"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("verify without flags = %d, want exitUsage", code)
	}
	if code := run([]string{"verify", "-pub", "p", "-manifest", "m"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("verify missing in = %d, want exitUsage", code)
	}
}

func TestMoreCommandErrors(t *testing.T) {
	td := t.TempDir()
	var stdout, stderr bytes.Buffer

	// Help flag on subcommand
	if code := run([]string{"sign", "-h"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("sign -h = %d, want exitUsage", code)
	}

	// Unexpected positional arguments
	if code := run([]string{"sign", "-key", "k", "-version", "1.0.0", "-in", "i", "-out", "o", "extra"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("sign with extra arg = %d, want exitUsage", code)
	}

	// sign missing key file
	if code := run([]string{"sign", "-key", filepath.Join(td, "nonexistent.key"), "-version", "1.0.0", "-in", "i", "-out", "o"}, &stdout, &stderr); code != exitFail {
		t.Errorf("sign missing key file = %d, want exitFail", code)
	}

	// server-cert missing key file
	if code := run([]string{"server-cert", "-key", filepath.Join(td, "nonexistent.key"), "-out-key", "k", "-out-cert", "c"}, &stdout, &stderr); code != exitFail {
		t.Errorf("server-cert missing key file = %d, want exitFail", code)
	}

	// server-cert rand failure
	relKeyPath := filepath.Join(td, "rel.key")
	_ = run([]string{"keygen", "-out", relKeyPath}, &stdout, &stderr)
	origRand := randReader
	randReader = errReader{}
	if code := run([]string{"server-cert", "-key", relKeyPath, "-out-key", filepath.Join(td, "k"), "-out-cert", filepath.Join(td, "c")}, &stdout, &stderr); code != exitFail {
		t.Errorf("server-cert rand failure = %d, want exitFail", code)
	}
	randReader = origRand

	// writeAtomic failure on invalid path
	if err := writeAtomic("/dev/null/impossible/path", []byte("data"), 0644); err == nil {
		t.Error("expected error for writeAtomic to invalid path")
	}

	// writeNewSecret failure on invalid path
	if err := writeNewSecret("/dev/null/impossible/path", []byte("data")); err == nil {
		t.Error("expected error for writeNewSecret to invalid path")
	}
}
