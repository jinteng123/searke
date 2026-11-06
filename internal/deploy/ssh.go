package deploy

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/jint/searke/internal/cluster"
)

// SSHConfig configures connections to one node.
type SSHConfig struct {
	User         string
	Port         int
	IdentityFile string
	Sudo         bool
	Timeout      time.Duration
}

// Client is an SSH connection to a node.
type Client struct {
	*ssh.Client
	cfg  SSHConfig
	addr string
}

// Connect dials a node and authenticates with the identity file and/or
// the local SSH agent.
func Connect(address string, cfg SSHConfig) (*Client, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	var auths []ssh.AuthMethod

	if cfg.IdentityFile != "" {
		key, err := os.ReadFile(cluster.ExpandPath(cfg.IdentityFile))
		if err != nil {
			return nil, fmt.Errorf("read identity file: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("parse identity file %s: %w", cfg.IdentityFile, err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("no SSH credentials: set spec.ssh.identityFile or run ssh-agent")
	}

	khPath := cluster.ExpandPath("~/.ssh/known_hosts")
	hostKeyCB, err := knownhosts.New(khPath)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", khPath, err)
	}
	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auths,
		HostKeyCallback: hostKeyCB,
		Timeout:         cfg.Timeout,
	}
	addr := net.JoinHostPort(address, fmt.Sprint(cfg.Port))
	conn, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	return &Client{Client: conn, cfg: cfg, addr: addr}, nil
}

// Result holds the output of a remote command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes a command via the default shell.
func (c *Client) Run(cmd string) (*Result, error) {
	return c.run(cmd, false, nil)
}

// RunSudo executes a command as root via sudo -n.
func (c *Client) RunSudo(cmd string) (*Result, error) {
	return c.run(cmd, true, nil)
}

// RunStdin executes a (sudo) command feeding stdin.
func (c *Client) RunStdin(cmd string, stdin []byte) (*Result, error) {
	return c.run(cmd, true, stdin)
}

func (c *Client) run(cmd string, sudo bool, stdin []byte) (*Result, error) {
	if sudo && c.cfg.Sudo {
		cmd = "sudo -n sh -c " + shellQuote(cmd)
	}
	sess, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	var outBuf, errBuf bytes.Buffer
	sess.Stdout = &outBuf
	sess.Stderr = &errBuf
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	err = sess.Run(cmd)
	res := &Result{Stdout: outBuf.String(), Stderr: errBuf.String()}
	if err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitStatus()
			return res, nil
		}
		return nil, err
	}
	return res, nil
}

// StagingDir is a scratch directory on the node used for payload
// uploads before they are installed as root.
const StagingDir = "/var/lib/rancher/searke-staging"

// PrepareStaging creates the staging dir and hands it to the SSH user
// so SFTP can write large payloads without sudo.
func (c *Client) PrepareStaging() error {
	if !c.cfg.Sudo {
		_, err := c.Run("mkdir -p " + shellQuote(StagingDir))
		return err
	}
	res, err := c.RunSudo(fmt.Sprintf(
		"mkdir -p %s && chown %s %s",
		shellQuote(StagingDir), shellQuote(c.cfg.User), shellQuote(StagingDir)))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("prepare staging: %s", res.Stderr)
	}
	return nil
}

// UploadFile streams a local file to a remote path. With sudo enabled
// the file is staged in StagingDir then moved into place as root.
func (c *Client) UploadFile(local, remote string, mode os.FileMode) error {
	sf, err := sftp.NewClient(c.Client)
	if err != nil {
		return fmt.Errorf("sftp: %w", err)
	}
	defer sf.Close()

	dest := remote
	if c.cfg.Sudo {
		dest = StagingDir + "/" + filepath.Base(remote)
	}
	if err := sf.MkdirAll(filepath.Dir(dest)); err != nil {
		return err
	}
	f, err := sf.Create(dest)
	if err != nil {
		return err
	}
	in, err := os.Open(local)
	if err != nil {
		f.Close()
		return err
	}
	defer in.Close()
	if _, err := f.ReadFrom(in); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if c.cfg.Sudo {
		res, err := c.RunSudo(fmt.Sprintf("install -m %o -o root -g root %s %s && rm -f %s",
			mode, shellQuote(dest), shellQuote(remote), shellQuote(dest)))
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("install %s: %s", remote, res.Stderr)
		}
	} else if err := sf.Chmod(dest, mode); err != nil {
		return err
	}
	return nil
}

// WriteFile writes small text content to a remote path as root.
func (c *Client) WriteFile(remote string, content []byte, mode os.FileMode) error {
	res, err := c.RunStdin(fmt.Sprintf("umask 077; tee %s > /dev/null && chmod %o %s",
		shellQuote(remote), mode, shellQuote(remote)), content)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s: %s", remote, res.Stderr)
	}
	return nil
}

// Addr returns the connected host:port.
func (c *Client) Addr() string { return c.addr }

func shellQuote(s string) string {
	return "'" + stringsReplaceAll(s) + "'"
}

func stringsReplaceAll(s string) string {
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, []byte("'\\''")...)
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}
