package deploy

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/jint/searke/internal/cluster"
)

// Password-based SSH auth is deliberately never persisted to the
// descriptor, so nodes installed or added via --password would become
// unreachable for later commands (apply/exec/uninstall/...). To keep
// the password a one-time bootstrap, searke authorizes a public key on
// every node it reaches with a password: the key stored as
// spec.ssh.identityFile when one exists, otherwise a per-cluster key
// generated here and recorded in the descriptor.

// managedKeyPath is the per-cluster SSH private key inside the
// cluster's state directory (<cluster home>/.searke/id_ed25519).
func managedKeyPath(dir string) string {
	return filepath.Join(dir, ".searke", "id_ed25519")
}

// ensureManagedKey loads (or generates) the cluster's ed25519 key and
// returns (private key path, authorized_keys line). Files are created
// with 0700/0600 permissions alongside the state file.
func ensureManagedKey(dir, clusterName string) (string, string, error) {
	privPath := managedKeyPath(dir)
	if pub, err := pubLineFromIdentity(privPath, keyComment(clusterName)); err == nil {
		return privPath, pub, nil
	} else if !os.IsNotExist(err) {
		return "", "", err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	blk, err := ssh.MarshalPrivateKey(priv, keyComment(clusterName))
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(privPath), 0o700); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(blk), 0o600); err != nil {
		return "", "", err
	}
	pub, err := pubLineFromIdentity(privPath, keyComment(clusterName))
	if err != nil {
		return "", "", err
	}
	return privPath, pub, nil
}

// pubLineFromIdentity renders the authorized_keys line for the public
// half of an SSH private key file.
func pubLineFromIdentity(identityFile, comment string) (string, error) {
	data, err := os.ReadFile(cluster.ExpandPath(identityFile))
	if err != nil {
		return "", err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", identityFile, err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if comment != "" {
		line += " " + comment
	}
	return line, nil
}

func keyComment(clusterName string) string { return "searke-" + clusterName }

// provisionKey installs the cluster's public key into the node's
// authorized_keys so future runs authenticate by key instead of the
// throwaway password. Best effort: a failure warns but never aborts
// the run — the current connection stays usable.
func provisionKey(ex Executor, node, pubLine string, opts *Options) {
	cmd := `d="$HOME/.ssh" && mkdir -p "$d" && chmod 700 "$d" ` +
		`&& touch "$d/authorized_keys" && chmod 600 "$d/authorized_keys" ` +
		`&& (grep -qxF ` + shellQuote(pubLine) + ` "$d/authorized_keys" ` +
		`|| echo ` + shellQuote(pubLine) + ` >> "$d/authorized_keys")`
	res, err := ex.Run(cmd)
	switch {
	case err != nil:
		logf(opts, "  warning %s: install cluster SSH key failed: %v (password-only auth for future commands)", node, err)
	case res.ExitCode != 0:
		logf(opts, "  warning %s: install cluster SSH key failed: %s (password-only auth for future commands)", node, tail(res.Stderr, 200))
	default:
		logf(opts, "  [%s] authorized cluster SSH key; future commands use key auth", node)
	}
}

// maybeProvisionKey pushes the public half of cfg.IdentityFile onto a
// node that was reached with a password. Local executors and pure
// key-auth connections are skipped.
func maybeProvisionKey(ex Executor, node string, cfg SSHConfig, opts *Options) {
	if cfg.Password == "" || cfg.IdentityFile == "" {
		return
	}
	if _, local := ex.(*LocalExecutor); local {
		return
	}
	pub, err := pubLineFromIdentity(cfg.IdentityFile, "")
	if err != nil {
		logf(opts, "  warning %s: cannot derive public key from %s: %v", node, cfg.IdentityFile, err)
		return
	}
	provisionKey(ex, node, pub, opts)
}

// isAuthErr reports whether a Connect failure is an authentication
// problem (rather than a network error) — used to decide whether the
// bootstrap server deserves a retry with the run-scoped password.
func isAuthErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "unable to authenticate") || strings.Contains(s, "no SSH credentials")
}
