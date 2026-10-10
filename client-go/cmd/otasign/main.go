// Command otasign is the release-signing tool for the transit-tracker OTA
// protocol (security spec v1). Subcommands:
//
//	otasign keygen      -out <path>
//	otasign pubkey      -key <path>
//	otasign sign        -key <path> -version <v> -in <binary> -out <manifest.json>
//	otasign server-cert -key <releasekey> -out-key <path> -out-cert <path>
//	otasign verify      -pub <b64> -manifest <path> -in <binary>
//
// Build tooling depends on these exact names and flags.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mike10010100/transit-tracker/client-go/internal/otasig"
)

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// Seams for tests.
var (
	randReader       io.Reader = rand.Reader
	now                        = time.Now
	errUsage                   = errors.New("usage error")
	errFlagRequired            = errors.New("missing required flag")
	errVerifyFailure           = errors.New("verification failed")
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usageText = `usage:
  otasign keygen      -out <path>
  otasign pubkey      -key <path>
  otasign sign        -key <path> -version <v> -in <binary> -out <manifest.json>
  otasign server-cert -key <releasekey> -out-key <path> -out-cert <path>
  otasign verify      -pub <b64> -manifest <path> -in <binary>
`

// run dispatches a subcommand and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	var err error
	switch args[0] {
	case "keygen":
		err = cmdKeygen(args[1:], stdout, stderr)
	case "pubkey":
		err = cmdPubkey(args[1:], stdout, stderr)
	case "sign":
		err = cmdSign(args[1:], stdout, stderr)
	case "server-cert":
		err = cmdServerCert(args[1:], stdout, stderr)
	case "verify":
		err = cmdVerify(args[1:], stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	default:
		fmt.Fprintf(stderr, "otasign: unknown subcommand %q\n%s", args[0], usageText)
		return exitUsage
	}
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errUsage), errors.Is(err, errFlagRequired), errors.Is(err, flag.ErrHelp):
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(stderr, "otasign %s: %v\n", args[0], err)
		}
		return exitUsage
	default:
		fmt.Fprintf(stderr, "otasign %s: %v\n", args[0], err)
		return exitFail
	}
}

// newFlags returns a FlagSet that reports errors instead of exiting.
func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("otasign "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse parses args and checks that every named flag is non-empty.
func parse(fs *flag.FlagSet, args []string, required map[string]*string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected arguments %v", errUsage, fs.Args())
	}
	for name, v := range required {
		if *v == "" {
			return fmt.Errorf("%w: -%s", errFlagRequired, name)
		}
	}
	return nil
}

// readPrivateKey loads a PKCS#8 PEM Ed25519 private key.
func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return otasig.ParsePrivateKeyPEM(data)
}

// writeNewSecret creates path exclusively (refusing to overwrite) with mode
// 0600, creating the parent directory with mode 0700 if needed.
func writeNewSecret(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; refusing to overwrite", path)
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// writeAtomic writes data to path via a temp file in the same directory and
// a rename, so readers never see a partial file. mode is applied explicitly
// (CreateTemp starts at 0600).
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// hashFile streams path and returns its lowercase hex SHA-256 and size.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func cmdKeygen(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("keygen", stderr)
	out := fs.String("out", "", "path for the new PKCS#8 PEM private key (must not exist)")
	if err := parse(fs, args, map[string]*string{"out": out}); err != nil {
		return err
	}
	if _, err := os.Lstat(*out); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite", *out)
	}
	pub, priv, err := ed25519.GenerateKey(randReader)
	if err != nil {
		return err
	}
	pemBytes, err := otasig.MarshalPrivateKeyPEM(priv)
	if err != nil {
		return err
	}
	if err := writeNewSecret(*out, pemBytes); err != nil {
		return err
	}
	fmt.Fprintln(stdout, otasig.EncodePublicKey(pub))
	return nil
}

func cmdPubkey(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("pubkey", stderr)
	key := fs.String("key", "", "PKCS#8 PEM Ed25519 private key")
	if err := parse(fs, args, map[string]*string{"key": key}); err != nil {
		return err
	}
	priv, err := readPrivateKey(*key)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, otasig.EncodePublicKey(otasig.PublicKeyOf(priv)))
	return nil
}

func cmdSign(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("sign", stderr)
	key := fs.String("key", "", "release signing key (PKCS#8 PEM)")
	version := fs.String("version", "", "release version (N.N.N)")
	in := fs.String("in", "", "binary to sign")
	out := fs.String("out", "", "manifest JSON output path")
	if err := parse(fs, args, map[string]*string{"key": key, "version": version, "in": in, "out": out}); err != nil {
		return err
	}
	if !otasig.ValidVersion(*version) {
		return fmt.Errorf("%w: -version %q must match ^[0-9]+\\.[0-9]+\\.[0-9]+$", errUsage, *version)
	}
	priv, err := readPrivateKey(*key)
	if err != nil {
		return err
	}
	sum, size, err := hashFile(*in)
	if err != nil {
		return err
	}
	m, err := otasig.SignManifest(priv, *version, sum, size)
	if err != nil {
		return err
	}
	js, err := m.MarshalCompact()
	if err != nil {
		return err
	}
	if err := writeAtomic(*out, js, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed %s version=%s sha256=%s size=%d -> %s\n", *in, m.Version, m.SHA256, m.Size, *out)
	return nil
}

func cmdServerCert(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("server-cert", stderr)
	key := fs.String("key", "", "release signing key (PKCS#8 PEM)")
	outKey := fs.String("out-key", "", "output path for the new server identity key (PKCS#8 PEM, 0600)")
	outCert := fs.String("out-cert", "", "output path for the server identity certificate JSON")
	if err := parse(fs, args, map[string]*string{"key": key, "out-key": outKey, "out-cert": outCert}); err != nil {
		return err
	}
	releasePriv, err := readPrivateKey(*key)
	if err != nil {
		return err
	}
	serverPub, serverPriv, err := ed25519.GenerateKey(randReader)
	if err != nil {
		return err
	}
	cert, err := otasig.SignCert(releasePriv, serverPub, now().Unix())
	if err != nil {
		return err
	}
	certJSON, err := cert.MarshalCompact()
	if err != nil {
		return err
	}
	keyPEM, err := otasig.MarshalPrivateKeyPEM(serverPriv)
	if err != nil {
		return err
	}
	// The server key is per build, so replacing a previous one is expected.
	if err := writeAtomic(*outKey, keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeAtomic(*outCert, certJSON, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "server identity %s issued_at=%d -> %s, %s\n", cert.PublicKey, cert.IssuedAt, *outKey, *outCert)
	return nil
}

func cmdVerify(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("verify", stderr)
	pubB64 := fs.String("pub", "", "release public key (base64)")
	manifest := fs.String("manifest", "", "manifest JSON")
	in := fs.String("in", "", "binary the manifest should describe")
	if err := parse(fs, args, map[string]*string{"pub": pubB64, "manifest": manifest, "in": in}); err != nil {
		return err
	}
	pub, err := otasig.ParsePublicKey(*pubB64)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(*manifest)
	if err != nil {
		return err
	}
	m, err := otasig.VerifyManifest(pub, data)
	if err != nil {
		return fmt.Errorf("%w: %v", errVerifyFailure, err)
	}
	sum, size, err := hashFile(*in)
	if err != nil {
		return err
	}
	if size != m.Size {
		return fmt.Errorf("%w: size %d, manifest says %d", errVerifyFailure, size, m.Size)
	}
	if sum != m.SHA256 {
		return fmt.Errorf("%w: sha256 %s, manifest says %s", errVerifyFailure, sum, m.SHA256)
	}
	fmt.Fprintf(stdout, "OK version=%s sha256=%s size=%d\n", m.Version, m.SHA256, m.Size)
	return nil
}
