// The cex-dra-kubeletplugin command is the DRA kubelet plugin for IBM
// Crypto Express (CEX) adapters on s390x nodes. It runs preflight checks,
// registers with the kubelet, and serves the driver until terminated.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"k8s.io/apimachinery/pkg/util/validation"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"k8s-cex-dra-driver/internal/driver"
	"k8s-cex-dra-driver/internal/features"
	"k8s-cex-dra-driver/internal/health"
	"k8s-cex-dra-driver/internal/logphase"
	"k8s-cex-dra-driver/internal/preflight"
	"k8s-cex-dra-driver/internal/shadowsysfs"
	"k8s-cex-dra-driver/internal/version"
)

const (
	// DriverName is the DRA driver name this plugin registers with the
	// kubelet and stamps on its ResourceSlices.
	DriverName = "cex-driver.ibm.com"
)

// Flags collects the command-line options of the plugin.
type Flags struct {
	nodeName                      string
	machineID                     string
	sysinfoPath                   string
	kubeconfig                    string
	kubeletRegistrarDirectoryPath string
	kubeletPluginsDirectoryPath   string
	scanInterval                  time.Duration
	healthcheckPort               int
	cdiRoot                       string
	shadowSysfsRoot               string
	featureGates                  string
}

func main() {
	if err := newApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		var fatal *preflight.FatalError
		if errors.As(err, &fatal) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// klogFlagValue adapts a klog-registered flag value to the cli Value
// interface, which additionally requires flag.Getter.
type klogFlagValue struct{ flag.Value }

func (v klogFlagValue) Get() any { return v.String() }

func newApp() *cli.Command {
	flags := &Flags{}

	// klog registers its flags on a standalone set. The operational ones are
	// bridged into the cli surface below so they exist on the command line.
	// Setting the bridged flag writes straight through to klog's globals.
	klogFlags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(klogFlags)

	// The auto-added version flag aliases -v, which klog convention claims
	// for verbosity, so --version stays long-form only.
	cli.VersionFlag = &cli.BoolFlag{
		Name:        "version",
		Usage:       "print the version",
		HideDefault: true,
		Local:       true,
	}

	cmd := &cli.Command{
		Name:            "cex-dra-kubeletplugin",
		Usage:           "Minimal CEX DRA driver kubelet plugin",
		Version:         version.Get(),
		ArgsUsage:       " ",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "node-name",
				Usage:       "The name of the node to be worked on (defaults to hostname)",
				Destination: &flags.nodeName,
				Sources:     cli.EnvVars("NODE_NAME"),
			},
			&cli.StringFlag{
				Name:        "machine-id",
				Usage:       "Machine identifier for device name uniqueness, a DNS-1123 label of at most 55 characters (defaults to sysinfo parsing)",
				Destination: &flags.machineID,
				Sources:     cli.EnvVars("MACHINE_ID"),
			},
			&cli.StringFlag{
				Name:        "sysinfo-path",
				Usage:       "Path to the sysinfo file parsed for machine-id auto-detection (defaults to the container's own /proc/sysinfo, a global node needing no host mount)",
				Value:       "/proc/sysinfo",
				Destination: &flags.sysinfoPath,
				Sources:     cli.EnvVars("SYSINFO_PATH"),
			},
			&cli.StringFlag{
				Name:        "kubeconfig",
				Usage:       "Absolute path to the kubeconfig file (for out-of-cluster)",
				Destination: &flags.kubeconfig,
				Sources:     cli.EnvVars("KUBECONFIG"),
			},
			&cli.StringFlag{
				Name:        "kubelet-registrar-directory-path",
				Usage:       "Absolute path to the directory where kubelet stores plugin registrations",
				Value:       kubeletplugin.KubeletRegistryDir,
				Destination: &flags.kubeletRegistrarDirectoryPath,
				Sources:     cli.EnvVars("KUBELET_REGISTRAR_DIRECTORY_PATH"),
			},
			&cli.StringFlag{
				Name:        "kubelet-plugins-directory-path",
				Usage:       "Absolute path to the directory where kubelet stores plugin data",
				Value:       kubeletplugin.KubeletPluginsDir,
				Destination: &flags.kubeletPluginsDirectoryPath,
				Sources:     cli.EnvVars("KUBELET_PLUGINS_DIRECTORY_PATH"),
			},
			&cli.IntFlag{
				Name: "healthcheck-port",
				Usage: "`Port` for the gRPC liveness service the kubelet probes. " +
					"Positive is a literal port, zero allocates a random one, negative disables the service",
				Value:       -1,
				Destination: &flags.healthcheckPort,
				Sources:     cli.EnvVars("HEALTHCHECK_PORT"),
			},
			&cli.DurationFlag{
				Name:        "scan-interval",
				Usage:       "Interval between AP queue scans",
				Value:       30 * time.Second,
				Destination: &flags.scanInterval,
				Sources:     cli.EnvVars("SCAN_INTERVAL"),
			},
			&cli.StringFlag{
				Name:        "cdi-root",
				Usage:       "Absolute path to the CDI specification directory",
				Value:       "/var/run/cdi",
				Destination: &flags.cdiRoot,
				Sources:     cli.EnvVars("CDI_ROOT"),
			},
			&cli.StringFlag{
				Name:        "shadow-sysfs-root",
				Usage:       "Absolute host path holding the per-claim shadow AP sysfs trees of container claims; must be mounted at the same path into the plugin container",
				Value:       shadowsysfs.BaseDir,
				Destination: &flags.shadowSysfsRoot,
				Sources:     cli.EnvVars("SHADOW_SYSFS_ROOT"),
			},
			&cli.StringFlag{
				Name: "feature-gates",
				Usage: "A set of Name=bool pairs enabling or disabling features. Options are:\n" +
					strings.Join(features.KnownFeatures(), "\n"),
				Destination: &flags.featureGates,
				Sources:     cli.EnvVars("FEATURE_GATES"),
			},
			&cli.GenericFlag{
				Name:    "v",
				Usage:   "Klog verbosity `level`; per-queue detail from repeat scans and from driver_override switching appears at 4 and up",
				Value:   klogFlagValue{klogFlags.Lookup("v").Value},
				Sources: cli.EnvVars("LOG_VERBOSITY"),
			},
			&cli.GenericFlag{
				Name:    "vmodule",
				Usage:   "Comma-separated `pattern=N` file-filtered klog verbosity overrides",
				Value:   klogFlagValue{klogFlags.Lookup("vmodule").Value},
				Sources: cli.EnvVars("LOG_VMODULE"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// First line of every run: even a startup that dies on the next
			// step leaves the build identity in the log for the bug report.
			logphase.Logf(logphase.Startup, "cex-dra-kubeletplugin %s", version.Get())

			// Gates decide what the rest of the process does, so they are
			// resolved and logged before any driver work: a typo aborts
			// startup here rather than after the preflight probes.
			if err := features.Set(flags.featureGates); err != nil {
				return err
			}
			logphase.Logf(logphase.Startup, "feature gates: %s", features.Summary())

			if err := validateWorkloadGates(); err != nil {
				return err
			}

			if err := validateMachineID(flags.machineID); err != nil {
				return err
			}
			if err := validateShadowSysfsRoot(flags.shadowSysfsRoot); err != nil {
				return err
			}

			if err := preflight.Run(); err != nil {
				return err
			}

			if flags.nodeName == "" {
				hostname, err := os.Hostname()
				if err != nil {
					return fmt.Errorf("--node-name not specified and failed to get hostname: %w", err)
				}
				flags.nodeName = hostname
				klog.Infof("Using hostname as node name: %s", flags.nodeName)
			}

			coreclient, err := newKubeClient(flags.kubeconfig)
			if err != nil {
				return fmt.Errorf("create client: %w", err)
			}

			return runPlugin(ctx, flags, coreclient)
		},
	}

	return cmd
}

// validateWorkloadGates rejects the one unsupported gate combination: every
// workload path disabled. The driver would register, publish slices, and
// refuse every prepare - a config error better reported at startup than as
// per-claim failures. Ordinary configuration error, exit code 1, distinct
// from preflight's 2: the node is fine, the flags are not.
func validateWorkloadGates() error {
	if !features.Gate.Enabled(features.VirtualMachineWorkload) && !features.Gate.Enabled(features.ContainerWorkload) {
		return fmt.Errorf("no workload path enabled: %s=false and %s=false leave the driver nothing to serve, so enable at least one",
			features.VirtualMachineWorkload, features.ContainerWorkload)
	}
	return nil
}

// maxMachineIDLen bounds --machine-id so every published device name stays
// a valid resource.k8s.io device name. The API caps names at 63 characters,
// and the driver builds them as <machine-id>-<apid>-<apqi>, appending the
// two-digit adapter ID and the four-digit domain index with their hyphens.
const maxMachineIDLen = 63 - len("-xx-yyyy")

// validateMachineID rejects a --machine-id the resource API would refuse
// later. Checked at startup so the error names the flag. Left to the API
// server it surfaces at ResourceSlice write time and names the object
// instead. Empty means sysinfo auto-detection, which composes a compliant
// value. Same error class as validateWorkloadGates: exit code 1, the node
// is fine, the flag is not.
func validateMachineID(machineID string) error {
	if machineID == "" {
		return nil
	}
	if len(machineID) > maxMachineIDLen {
		return fmt.Errorf("--machine-id %q is %d characters, at most %d fit a device name: the API caps names at 63 and the adapter-domain suffix takes 8",
			machineID, len(machineID), maxMachineIDLen)
	}
	if errs := validation.IsDNS1123Label(machineID); len(errs) > 0 {
		return fmt.Errorf("--machine-id %q is not a DNS-1123 label, which device names must be: %s",
			machineID, strings.Join(errs, ", "))
	}
	return nil
}

// validateShadowSysfsRoot rejects a relative --shadow-sysfs-root. The
// container runtime resolves the trees' CDI mounts on the host, so the path
// must be absolute and the same inside and outside the plugin pod. Same error
// class as validateMachineID.
func validateShadowSysfsRoot(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("--shadow-sysfs-root %q must be an absolute host path", root)
	}
	return nil
}

func newKubeClient(kubeconfig string) (coreclientset.Interface, error) {
	var csconfig *rest.Config
	var err error

	if kubeconfig == "" {
		csconfig, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("create in-cluster client configuration: %v", err)
		}
	} else {
		csconfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("create out-of-cluster client configuration: %v", err)
		}
	}

	coreclient, err := coreclientset.NewForConfig(csconfig)
	if err != nil {
		return nil, fmt.Errorf("create core client: %v", err)
	}

	return coreclient, nil
}

func pluginPath(kubeletPluginsDir string) string {
	return filepath.Join(kubeletPluginsDir, DriverName)
}

func runPlugin(ctx context.Context, flags *Flags, coreclient coreclientset.Interface) error {
	logger := klog.FromContext(ctx)

	pluginDir := pluginPath(flags.kubeletPluginsDirectoryPath)
	logphase.Logf(logphase.Registration, "Creating plugin directory: %s", pluginDir)
	if err := os.MkdirAll(pluginDir, 0750); err != nil {
		return fmt.Errorf("create plugin directory: %w", err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()
	ctx, cancel := context.WithCancelCause(ctx)

	// Liveness comes up before the driver, and its heartbeat is seeded at
	// construction, so the initial scan runs inside the covered window rather
	// than in a gap ahead of it.
	heartbeat := health.NewHeartbeat()
	staleAfter := health.StaleAfter(flags.scanInterval)
	healthServer, err := health.Start(flags.healthcheckPort, heartbeat, staleAfter)
	if err != nil {
		cancel(err)
		return err
	}
	defer healthServer.Stop()
	if addr := healthServer.Addr(); addr != "" {
		logphase.Logf(logphase.Startup, "liveness service on %s, stale after %s", addr, staleAfter)
	}
	go health.Watch(ctx, heartbeat, staleAfter)

	shadowsysfs.BaseDir = flags.shadowSysfsRoot

	cfg := &driver.Config{
		DriverName:                    DriverName,
		NodeName:                      flags.nodeName,
		MachineID:                     flags.machineID,
		SysinfoPath:                   flags.sysinfoPath,
		ScanInterval:                  flags.scanInterval,
		Heartbeat:                     heartbeat,
		Client:                        coreclient,
		CancelCtx:                     cancel,
		KubeletRegistrarDirectoryPath: flags.kubeletRegistrarDirectoryPath,
		PluginDataDirectoryPath:       pluginDir,
		CDIRoot:                       flags.cdiRoot,
	}

	logphase.Logf(logphase.Registration, "Starting driver and registering with kubelet")
	d, err := driver.New(ctx, cfg)
	if err != nil {
		return err
	}

	logphase.Logf(logphase.Registration, "CEX DRA driver started successfully")
	<-ctx.Done()

	stop()
	// A plain context.Canceled cause is the ordinary signal-driven shutdown.
	// Anything else is a fatal background error and must reach the exit code
	// so the kubelet restarts the pod as a crash, not a clean exit.
	cause := context.Cause(ctx)
	if errors.Is(cause, context.Canceled) {
		cause = nil
	} else if cause != nil {
		logger.Error(cause, "error from context")
	}

	d.Shutdown()

	return cause
}
