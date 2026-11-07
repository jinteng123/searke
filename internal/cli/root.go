package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/jint/searke/internal/deploy"
	"github.com/jint/searke/internal/ociimg"
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// NewRootCmd builds the searke command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "searke",
		Short:         "RKE2 multi-arch air-gap bundle builder and installer",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newBundleCmd(), newImageCmd())
	root.AddCommand(newClusterCmds()...)
	root.AddCommand(newVersionCmd(), newGenCmd())
	return root
}

func newBundleCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "bundle", Short: "Manage RKE2 artifact bundles"}

	var in, out string
	fetch := &cobra.Command{
		Use:   "fetch",
		Short: "Download RKE2 release assets per bundle.yaml and verify them",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBundleFetch(in, out)
		},
	}
	fetch.Flags().StringVarP(&in, "file", "f", "bundle.yaml", "bundle descriptor")
	fetch.Flags().StringVarP(&out, "output", "o", "", "output bundle directory (required)")
	fetch.MarkFlagRequired("output")

	verify := &cobra.Command{
		Use:   "verify <bundle-dir>",
		Short: "Re-verify a fetched bundle against upstream SHA256 manifests",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBundleVerify(args[0])
		},
	}
	cmd.AddCommand(fetch, verify)
	return cmd
}

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "image", Short: "Build and distribute multi-arch OCI images"}

	var tag, outDir, comp string
	build := &cobra.Command{
		Use:   "build <bundle-dir>",
		Short: "Build an OCI Image Index (amd64+arm64 sub-images) from a verified bundle",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImageBuild(args[0], tag, outDir, comp)
		},
	}
	build.Flags().StringVarP(&tag, "tag", "t", "", "image reference recorded in annotations (optional)")
	build.Flags().StringVarP(&outDir, "output-dir", "o", "", "OCI layout output dir (default <bundle-dir>-oci)")
	build.Flags().StringVar(&comp, "compression", "zstd", "layer compression: zstd|gzip (gzip for registries that reject zstd blobs, e.g. ACR)")

	var from string
	var pushInsecure bool
	push := &cobra.Command{
		Use:   "push <ref>",
		Short: "Push an OCI layout directory to a registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImagePush(cmd.Context(), from, args[0], pushInsecure)
		},
	}
	push.Flags().StringVar(&from, "from", "", "OCI layout dir to push (required)")
	push.Flags().BoolVar(&pushInsecure, "insecure", false, "use plain HTTP for this registry")
	push.MarkFlagRequired("from")

	var out string
	var saveInsecure bool
	save := &cobra.Command{
		Use:   "save <ref-or-layout-dir>",
		Short: "Export an image index to a .oci.tar archive (air-gap transport)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImageSave(args[0], out, saveInsecure)
		},
	}
	save.Flags().StringVarP(&out, "output", "o", "", "output tar path (required)")
	save.Flags().BoolVar(&saveInsecure, "insecure", false, "use plain HTTP for this registry")
	save.MarkFlagRequired("output")

	var loadOut string
	load := &cobra.Command{
		Use:   "load <archive.oci.tar>",
		Short: "Import a saved .oci.tar into a local OCI layout directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImageLoad(args[0], loadOut)
		},
	}
	load.Flags().StringVarP(&loadOut, "output-dir", "o", "", "OCI layout output dir (default ./oci-layout)")

	var pullOut string
	var pullInsecure bool
	pull := &cobra.Command{
		Use:   "pull <ref>",
		Short: "Pull a registry index into a local OCI layout directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImagePull(args[0], pullOut, pullInsecure)
		},
	}
	pull.Flags().StringVarP(&pullOut, "output-dir", "o", "", "OCI layout output dir (required)")
	pull.Flags().BoolVar(&pullInsecure, "insecure", false, "use plain HTTP for this registry")
	pull.MarkFlagRequired("output-dir")

	var inspectInsecure bool
	inspect := &cobra.Command{
		Use:   "inspect <ref|layout-dir|archive.oci.tar>",
		Short: "Print the image index metadata (rke2 version, CNI, platforms, digests)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImageInspect(cmd, args[0], inspectInsecure)
		},
	}
	inspect.Flags().BoolVar(&inspectInsecure, "insecure", false, "use plain HTTP for this registry")

	var serveAddr string
	serve := &cobra.Command{
		Use:   "serve <layout-dir>",
		Short: "Expose a local OCI layout dir as a read-only HTTP registry (foreground)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ociimg.ServeRegistry(args[0], serveAddr, logf)
		},
	}
	serve.Flags().StringVar(&serveAddr, "address", "127.0.0.1:15000", "listen address (bind trusted interfaces only: no auth)")

	cmd.AddCommand(build, push, pull, save, load, inspect, serve)
	return cmd
}

func newClusterCmds() []*cobra.Command {
	var file string
	flag := func(c *cobra.Command) {
		c.Flags().StringVarP(&file, "file", "f", "cluster.yaml", "cluster descriptor")
	}
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Check cluster.yaml, bundle and node preflight (read-only)",
		RunE:  func(cmd *cobra.Command, args []string) error { return runValidate(file) },
	}
	plan := &cobra.Command{
		Use:   "plan",
		Short: "Show the actions apply would take (read-only)",
		RunE:  func(cmd *cobra.Command, args []string) error { return runPlan(file) },
	}
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Install or converge the RKE2 cluster",
		RunE:  func(cmd *cobra.Command, args []string) error { return runApply(cmd.Context(), file) },
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show recorded apply state and live node status",
		RunE:  func(cmd *cobra.Command, args []string) error { return runStatus(file) },
	}
	var ro deploy.RunOptions
	var dryRun bool
	run := &cobra.Command{
		Use:   "run <bundle>",
		Short: "Install a single-node cluster on this machine (no cluster.yaml needed)",
		Long: "Install RKE2 on this machine as a single-node cluster, like `sealos run --single`.\n" +
			"The bundle may be an OCI layout dir, a .oci.tar archive, or a registry reference.\n" +
			"A cluster.yaml is generated under --home for status/snapshot/resume reuse.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ro.Bundle = args[0]
			return runSingle(cmd.Context(), ro, dryRun)
		},
	}
	run.Flags().StringVar(&ro.Name, "name", "", "cluster name (default searke-<hostname>)")
	run.Flags().StringVar(&ro.NodeName, "node-name", "", "Kubernetes node name (default hostname)")
	run.Flags().StringVar(&ro.Address, "address", "", "registration/API IP (default primary IPv4)")
	run.Flags().StringVar(&ro.CNI, "cni", "", "CNI the bundle was built with (default canal)")
	run.Flags().StringVar(&ro.ClusterCIDR, "cluster-cidr", "", "pod network CIDR (default 10.42.0.0/16)")
	run.Flags().StringVar(&ro.ServiceCIDR, "service-cidr", "", "service network CIDR (default 10.43.0.0/16)")
	run.Flags().StringVar(&ro.HomeDir, "home", "", "dir for generated cluster.yaml and state (default ~/.searke/<name>)")
	run.Flags().BoolVar(&dryRun, "dry-run", false, "generate cluster.yaml and run plan only; make no changes")
	run.Flags().BoolVar(&ro.Insecure, "insecure", false, "reach a registry bundle over plain HTTP (recorded in the generated cluster.yaml)")

	var skipDrain, skipSnapshot bool
	upgrade := &cobra.Command{
		Use:   "upgrade",
		Short: "Rolling upgrade to the bundle version (servers first, then agents)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpgrade(cmd.Context(), file, skipDrain, skipSnapshot)
		},
	}
	upgrade.Flags().BoolVar(&skipDrain, "skip-drain", false, "do not cordon/drain nodes during upgrade")
	upgrade.Flags().BoolVar(&skipSnapshot, "skip-snapshot", false, "do not take a pre-upgrade etcd snapshot")

	var uninstallYes, uninstallIgnore bool
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove RKE2 from every node in cluster.yaml (upstream killall + uninstall scripts)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUninstall(cmd.Context(), file, uninstallYes, uninstallIgnore)
		},
	}
	uninstall.Flags().BoolVar(&uninstallYes, "yes", false, "confirm the destructive uninstall")
	uninstall.Flags().BoolVar(&uninstallIgnore, "ignore-unreachable", false, "still clear local state when some nodes cannot be reached or cleaned")

	var add deploy.AddOptions
	var addDryRun bool
	addCmd := &cobra.Command{
		Use:   "add",
		Short: "Add servers/agents to an existing cluster (aliases: --masters/--nodes)",
		Long: "Append control-plane or worker addresses to cluster.yaml and converge " +
			"them via the normal apply pipeline. --masters and --nodes are accepted " +
			"as sealos-compatible aliases for --servers and --agents.",
		RunE: func(cmd *cobra.Command, args []string) error {
			add.DryRun = addDryRun
			return runAdd(cmd.Context(), file, &add)
		},
	}
	// Map sealos-style flag names onto the canonical ones so both may be
	// combined instead of one alias clobbering the other.
	addCmd.Flags().SetNormalizeFunc(func(f *pflag.FlagSet, name string) pflag.NormalizedName {
		switch name {
		case "masters":
			name = "servers"
		case "nodes":
			name = "agents"
		}
		return pflag.NormalizedName(name)
	})
	addCmd.Flags().StringSliceVar(&add.Servers, "servers", nil, "control-plane addresses to add (alias: --masters)")
	addCmd.Flags().StringSliceVar(&add.Agents, "agents", nil, "worker addresses to add (alias: --nodes)")
	addCmd.Flags().StringVar(&add.SSHUser, "ssh-user", "", "set spec.ssh.user in the descriptor (needed by searke run clusters)")
	addCmd.Flags().StringVar(&add.SSHKey, "ssh-key", "", "set spec.ssh.identityFile")
	addCmd.Flags().IntVar(&add.SSHPort, "ssh-port", 0, "set spec.ssh.port")
	addCmd.Flags().BoolVar(&addDryRun, "dry-run", false, "show the plan without modifying cluster.yaml or the nodes")

	var del deploy.DeleteOptions
	delCmd := &cobra.Command{
		Use:   "delete",
		Short: "Remove nodes: drain, delete from cluster, uninstall RKE2, update cluster.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(cmd.Context(), file, del)
		},
	}
	delCmd.Flags().StringSliceVar(&del.Servers, "servers", nil, "server names/addresses to remove (alias: --masters)")
	delCmd.Flags().StringSliceVar(&del.Agents, "agents", nil, "agent names/addresses to remove (alias: --nodes)")
	delCmd.Flags().SetNormalizeFunc(func(f *pflag.FlagSet, name string) pflag.NormalizedName {
		switch name {
		case "masters":
			name = "servers"
		case "nodes":
			name = "agents"
		}
		return pflag.NormalizedName(name)
	})
	delCmd.Flags().BoolVar(&del.Yes, "yes", false, "confirm removal (required)")
	delCmd.Flags().BoolVar(&del.DryRun, "dry-run", false, "show the plan without touching nodes or cluster.yaml")
	delCmd.Flags().BoolVar(&del.SkipDrain, "skip-drain", false, "do not kubectl drain nodes before removal")
	delCmd.Flags().DurationVar(&del.DrainTimeout, "drain-timeout", 120*time.Second, "kubectl drain timeout")

	var loginUser, loginPass string
	var loginInsecure, passStdin bool
	login := &cobra.Command{
		Use:   "login <registry>",
		Short: "Store registry credentials (docker-compatible ~/.docker/config.json)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pass := loginPass
			if passStdin {
				b, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				pass = strings.TrimRight(string(b), "\r\n")
			} else if pass != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: --password exposes the secret to ps and shell history; prefer --password-stdin")
			}
			if pass == "" {
				return fmt.Errorf("pass --password or --password-stdin")
			}
			return runLogin(cmd.Context(), args[0], loginUser, pass, loginInsecure)
		},
	}
	login.Flags().StringVarP(&loginUser, "username", "u", "", "registry username (required)")
	login.Flags().StringVarP(&loginPass, "password", "p", "", "registry password")
	login.Flags().BoolVar(&passStdin, "password-stdin", false, "read the password from stdin")
	login.Flags().BoolVar(&loginInsecure, "insecure", false, "mark the registry plain-HTTP (persisted for all commands)")
	login.MarkFlagRequired("username")

	logout := &cobra.Command{
		Use:   "logout <registry>",
		Short: "Remove stored registry credentials",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogout(args[0])
		},
	}

	for _, c := range []*cobra.Command{validate, plan, apply, status, upgrade, uninstall, addCmd, delCmd} {
		flag(c)
	}
	snap := newSnapshotCmd(&file)
	cmds := []*cobra.Command{validate, plan, apply, status, upgrade, uninstall, addCmd, delCmd,
		run, snap, login, logout}
	return append(cmds, newOpsCmds(&file)...)
}

// newOpsCmds are the day-2 node operations sharing the cluster
// descriptor and SSH plumbing: exec, scp, hosts, hostname, kubeconfig.
func newOpsCmds(file *string) []*cobra.Command {
	var nodesCSV string
	nodeFlag := func(c *cobra.Command) {
		c.Flags().StringVar(&nodesCSV, "nodes", "", "comma-separated node names/addresses (default: all)")
	}

	execCmd := &cobra.Command{
		Use:   "exec -- <command>",
		Short: "Run a shell command as root on the selected nodes",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExec(*file, nodesCSV, strings.Join(args, " "))
		},
	}
	nodeFlag(execCmd)

	var scpDownload bool
	scp := &cobra.Command{
		Use:   "scp <local> <remote>",
		Short: "Copy a file to every selected node (or --download to fetch into <local-dir>)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScp(*file, nodesCSV, args[0], args[1], scpDownload)
		},
	}
	nodeFlag(scp)
	scp.Flags().BoolVar(&scpDownload, "download", false,
		"fetch <remote> from each node into <local-dir>/<node>-<basename>")

	var hostForce bool
	hostname := &cobra.Command{
		Use:   "hostname [name]",
		Short: "Set node hostnames (default: the descriptor node name)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runHostname(*file, nodesCSV, name, hostForce)
		},
	}
	nodeFlag(hostname)
	hostname.Flags().BoolVar(&hostForce, "force", false,
		"rename even when RKE2 is already installed (dangerous)")

	hosts := &cobra.Command{Use: "hosts", Short: "Manage node /etc/hosts"}
	hostsSync := &cobra.Command{
		Use:   "sync",
		Short: "Write a managed <address> <name> block to every node's /etc/hosts",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runHostsSync(*file, nodesCSV) },
	}
	nodeFlag(hostsSync)
	hosts.AddCommand(hostsSync)

	var kubeDir string
	kubeconfig := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Fetch the admin kubeconfig to <dir>/<cluster>-kubeconfig (mode 0600)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runKubeconfig(*file, kubeDir)
		},
	}
	kubeconfig.Flags().StringVarP(&kubeDir, "output-dir", "o", ".", "destination directory")

	cmds := []*cobra.Command{execCmd, scp, hosts, hostname, kubeconfig, newCertCmds(file, &nodesCSV)}
	for _, c := range cmds {
		// Persistent so flag applies on leaf subcommands (hosts sync,
		// cert check...) as well as on the command itself.
		c.PersistentFlags().StringVarP(file, "file", "f", "cluster.yaml", "cluster descriptor")
	}
	return cmds
}

// newCertCmds groups the apiserver certificate operations.
func newCertCmds(file, nodesCSV *string) *cobra.Command {
	cert := &cobra.Command{Use: "cert", Short: "RKE2 apiserver certificate operations"}

	check := &cobra.Command{
		Use:   "check",
		Short: "Show serving-cert expiry and SANs on each server (read-only)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runCertCheck(*file, *nodesCSV) },
	}
	check.Flags().StringVar(nodesCSV, "nodes", "", "comma-separated server names/addresses")

	var sanYes bool
	san := &cobra.Command{
		Use:   "san <ip-or-domain>...",
		Short: "Add SANs to the apiserver cert on every selected server (rolling restart)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCertSan(*file, *nodesCSV, args, sanYes)
		},
	}
	san.Flags().StringVar(nodesCSV, "nodes", "", "comma-separated server names/addresses")
	san.Flags().BoolVar(&sanYes, "yes", false, "confirm the rolling rke2-server restarts")

	var rotYes bool
	rotate := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate RKE2 certificates on every selected server (rolling restart)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runCertRotate(*file, *nodesCSV, rotYes) },
	}
	rotate.Flags().StringVar(nodesCSV, "nodes", "", "comma-separated server names/addresses")
	rotate.Flags().BoolVar(&rotYes, "yes", false, "confirm the rolling rke2-server restarts")

	cert.AddCommand(check, san, rotate)
	return cert
}

func newSnapshotCmd(file *string) *cobra.Command {
	cmd := &cobra.Command{Use: "snapshot", Short: "etcd snapshot workflows"}

	var name string
	save := &cobra.Command{
		Use:   "save",
		Short: "Create an etcd snapshot on the bootstrap server",
		RunE:  func(cmd *cobra.Command, args []string) error { return runSnapshotSave(*file, name) },
	}
	save.Flags().StringVar(&name, "name", "", "snapshot name (default searke-<timestamp>)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List snapshots on the bootstrap server",
		RunE:  func(cmd *cobra.Command, args []string) error { return runSnapshotList(*file) },
	}

	var outDir string
	pull := &cobra.Command{
		Use:   "pull <name>",
		Short: "Download a snapshot file locally",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runSnapshotPull(*file, args[0], outDir) },
	}
	pull.Flags().StringVarP(&outDir, "output-dir", "o", "./snapshots", "local output dir")

	var yes bool
	restore := &cobra.Command{
		Use:   "restore <name>",
		Short: "cluster-reset restore on the bootstrap server (other servers must rejoin)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runSnapshotRestore(*file, args[0], yes) },
	}
	restore.Flags().BoolVar(&yes, "yes", false, "confirm the destructive restore")

	for _, c := range []*cobra.Command{save, list, pull, restore} {
		c.Flags().StringVarP(file, "file", "f", "cluster.yaml", "cluster descriptor")
	}
	cmd.AddCommand(save, list, pull, restore)
	return cmd
}
