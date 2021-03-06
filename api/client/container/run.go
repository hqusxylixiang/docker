package container

import (
	"fmt"
	"io"
	"net/http/httputil"
	"os"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/net/context"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/client"
	"github.com/docker/docker/cli"
	opttypes "github.com/docker/docker/opts"
	"github.com/docker/docker/pkg/promise"
	"github.com/docker/docker/pkg/signal"
	runconfigopts "github.com/docker/docker/runconfig/opts"
	"github.com/docker/engine-api/types"
	"github.com/docker/libnetwork/resolvconf/dns"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	errCmdNotFound          = "not found or does not exist"
	errCmdCouldNotBeInvoked = "could not be invoked"
)

type runOptions struct {
	autoRemove bool
	detach     bool
	sigProxy   bool
	name       string
	detachKeys string
}

// NewRunCommand create a new `docker run` command
func NewRunCommand(dockerCli *client.DockerCli) *cobra.Command {
	var opts runOptions
	var copts *runconfigopts.ContainerOptions

	cmd := &cobra.Command{
		Use:   "run [OPTIONS] IMAGE [COMMAND] [ARG...]",
		Short: "Run a command in a new container",
		Args:  cli.RequiresMinArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			copts.Image = args[0]
			if len(args) > 1 {
				copts.Args = args[1:]
			}
			return runRun(dockerCli, cmd.Flags(), &opts, copts)
		},
	}
	cmd.SetFlagErrorFunc(flagErrorFunc)

	flags := cmd.Flags()
	flags.SetInterspersed(false)

	// These are flags not stored in Config/HostConfig
	flags.BoolVar(&opts.autoRemove, "rm", false, "Automatically remove the container when it exits")
	flags.BoolVarP(&opts.detach, "detach", "d", false, "Run container in background and print container ID")
	flags.BoolVar(&opts.sigProxy, "sig-proxy", true, "Proxy received signals to the process")
	flags.StringVar(&opts.name, "name", "", "Assign a name to the container")
	flags.StringVar(&opts.detachKeys, "detach-keys", "", "Override the key sequence for detaching a container")

	// Add an explicit help that doesn't have a `-h` to prevent the conflict
	// with hostname
	flags.Bool("help", false, "Print usage")

	client.AddTrustedFlags(flags, true)
	copts = runconfigopts.AddFlags(flags)
	return cmd
}

func flagErrorFunc(cmd *cobra.Command, err error) error {
	return cli.StatusError{
		Status:     cli.FlagErrorFunc(cmd, err).Error(),
		StatusCode: 125,
	}
}

func runRun(dockerCli *client.DockerCli, flags *pflag.FlagSet, opts *runOptions, copts *runconfigopts.ContainerOptions) error {
	stdout, stderr, stdin := dockerCli.Out(), dockerCli.Err(), dockerCli.In()
	client := dockerCli.Client()
	// TODO: pass this as an argument
	cmdPath := "run"

	var (
		flAttach                              *opttypes.ListOpts
		ErrConflictAttachDetach               = fmt.Errorf("Conflicting options: -a and -d")
		ErrConflictRestartPolicyAndAutoRemove = fmt.Errorf("Conflicting options: --restart and --rm")
		ErrConflictDetachAutoRemove           = fmt.Errorf("Conflicting options: --rm and -d")
	)

	config, hostConfig, networkingConfig, err := runconfigopts.Parse(flags, copts)

	// just in case the Parse does not exit
	if err != nil {
		reportError(stderr, cmdPath, err.Error(), true)
		return cli.StatusError{StatusCode: 125}
	}

	if hostConfig.OomKillDisable != nil && *hostConfig.OomKillDisable && hostConfig.Memory == 0 {
		fmt.Fprintf(stderr, "WARNING: Disabling the OOM killer on containers without setting a '-m/--memory' limit may be dangerous.\n")
	}

	if len(hostConfig.DNS) > 0 {
		// check the DNS settings passed via --dns against
		// localhost regexp to warn if they are trying to
		// set a DNS to a localhost address
		for _, dnsIP := range hostConfig.DNS {
			if dns.IsLocalhost(dnsIP) {
				fmt.Fprintf(stderr, "WARNING: Localhost DNS setting (--dns=%s) may fail in containers.\n", dnsIP)
				break
			}
		}
	}

	config.ArgsEscaped = false

	if !opts.detach {
		if err := dockerCli.CheckTtyInput(config.AttachStdin, config.Tty); err != nil {
			return err
		}
	} else {
		if fl := flags.Lookup("attach"); fl != nil {
			flAttach = fl.Value.(*opttypes.ListOpts)
			if flAttach.Len() != 0 {
				return ErrConflictAttachDetach
			}
		}
		if opts.autoRemove {
			return ErrConflictDetachAutoRemove
		}

		config.AttachStdin = false
		config.AttachStdout = false
		config.AttachStderr = false
		config.StdinOnce = false
	}

	// Disable sigProxy when in TTY mode
	if config.Tty {
		opts.sigProxy = false
	}

	// Telling the Windows daemon the initial size of the tty during start makes
	// a far better user experience rather than relying on subsequent resizes
	// to cause things to catch up.
	if runtime.GOOS == "windows" {
		hostConfig.ConsoleSize[0], hostConfig.ConsoleSize[1] = dockerCli.GetTtySize()
	}

	ctx, cancelFun := context.WithCancel(context.Background())

	createResponse, err := createContainer(ctx, dockerCli, config, hostConfig, networkingConfig, hostConfig.ContainerIDFile, opts.name)
	if err != nil {
		reportError(stderr, cmdPath, err.Error(), true)
		return runStartContainerErr(err)
	}
	if opts.sigProxy {
		sigc := dockerCli.ForwardAllSignals(ctx, createResponse.ID)
		defer signal.StopCatch(sigc)
	}
	var (
		waitDisplayID chan struct{}
		errCh         chan error
	)
	if !config.AttachStdout && !config.AttachStderr {
		// Make this asynchronous to allow the client to write to stdin before having to read the ID
		waitDisplayID = make(chan struct{})
		go func() {
			defer close(waitDisplayID)
			fmt.Fprintf(stdout, "%s\n", createResponse.ID)
		}()
	}
	if opts.autoRemove && (hostConfig.RestartPolicy.IsAlways() || hostConfig.RestartPolicy.IsOnFailure()) {
		return ErrConflictRestartPolicyAndAutoRemove
	}
	attach := config.AttachStdin || config.AttachStdout || config.AttachStderr
	if attach {
		var (
			out, cerr io.Writer
			in        io.ReadCloser
		)
		if config.AttachStdin {
			in = stdin
		}
		if config.AttachStdout {
			out = stdout
		}
		if config.AttachStderr {
			if config.Tty {
				cerr = stdout
			} else {
				cerr = stderr
			}
		}

		if opts.detachKeys != "" {
			dockerCli.ConfigFile().DetachKeys = opts.detachKeys
		}

		options := types.ContainerAttachOptions{
			Stream:     true,
			Stdin:      config.AttachStdin,
			Stdout:     config.AttachStdout,
			Stderr:     config.AttachStderr,
			DetachKeys: dockerCli.ConfigFile().DetachKeys,
		}

		resp, errAttach := client.ContainerAttach(ctx, createResponse.ID, options)
		if errAttach != nil && errAttach != httputil.ErrPersistEOF {
			// ContainerAttach returns an ErrPersistEOF (connection closed)
			// means server met an error and put it in Hijacked connection
			// keep the error and read detailed error message from hijacked connection later
			return errAttach
		}
		defer resp.Close()

		errCh = promise.Go(func() error {
			errHijack := dockerCli.HoldHijackedConnection(ctx, config.Tty, in, out, cerr, resp)
			if errHijack == nil {
				return errAttach
			}
			return errHijack
		})
	}

	if opts.autoRemove {
		defer func() {
			// Explicitly not sharing the context as it could be "Done" (by calling cancelFun)
			// and thus the container would not be removed.
			if err := removeContainer(dockerCli, context.Background(), createResponse.ID, true, false, true); err != nil {
				fmt.Fprintf(stderr, "%v\n", err)
			}
		}()
	}

	//start the container
	if err := client.ContainerStart(ctx, createResponse.ID, types.ContainerStartOptions{}); err != nil {
		// If we have holdHijackedConnection, we should notify
		// holdHijackedConnection we are going to exit and wait
		// to avoid the terminal are not restored.
		if attach {
			cancelFun()
			<-errCh
		}

		reportError(stderr, cmdPath, err.Error(), false)
		return runStartContainerErr(err)
	}

	if (config.AttachStdin || config.AttachStdout || config.AttachStderr) && config.Tty && dockerCli.IsTerminalOut() {
		if err := dockerCli.MonitorTtySize(ctx, createResponse.ID, false); err != nil {
			fmt.Fprintf(stderr, "Error monitoring TTY size: %s\n", err)
		}
	}

	if errCh != nil {
		if err := <-errCh; err != nil {
			logrus.Debugf("Error hijack: %s", err)
			return err
		}
	}

	// Detached mode: wait for the id to be displayed and return.
	if !config.AttachStdout && !config.AttachStderr {
		// Detached mode
		<-waitDisplayID
		return nil
	}

	var status int

	// Attached mode
	if opts.autoRemove {
		// Autoremove: wait for the container to finish, retrieve
		// the exit code and remove the container
		if status, err = client.ContainerWait(ctx, createResponse.ID); err != nil {
			return runStartContainerErr(err)
		}
		if _, status, err = getExitCode(dockerCli, ctx, createResponse.ID); err != nil {
			return err
		}
	} else {
		// No Autoremove: Simply retrieve the exit code
		if !config.Tty {
			// In non-TTY mode, we can't detach, so we must wait for container exit
			if status, err = client.ContainerWait(ctx, createResponse.ID); err != nil {
				return err
			}
		} else {
			// In TTY mode, there is a race: if the process dies too slowly, the state could
			// be updated after the getExitCode call and result in the wrong exit code being reported
			if _, status, err = getExitCode(dockerCli, ctx, createResponse.ID); err != nil {
				return err
			}
		}
	}
	if status != 0 {
		return cli.StatusError{StatusCode: status}
	}
	return nil
}

// reportError is a utility method that prints a user-friendly message
// containing the error that occurred during parsing and a suggestion to get help
func reportError(stderr io.Writer, name string, str string, withHelp bool) {
	if withHelp {
		str += ".\nSee '" + os.Args[0] + " " + name + " --help'"
	}
	fmt.Fprintf(stderr, "%s: %s.\n", os.Args[0], str)
}

// if container start fails with 'not found'/'no such' error, return 127
// if container start fails with 'permission denied' error, return 126
// return 125 for generic docker daemon failures
func runStartContainerErr(err error) error {
	trimmedErr := strings.TrimPrefix(err.Error(), "Error response from daemon: ")
	statusError := cli.StatusError{StatusCode: 125}
	if strings.Contains(trimmedErr, "executable file not found") ||
		strings.Contains(trimmedErr, "no such file or directory") ||
		strings.Contains(trimmedErr, "system cannot find the file specified") {
		statusError = cli.StatusError{StatusCode: 127}
	} else if strings.Contains(trimmedErr, syscall.EACCES.Error()) {
		statusError = cli.StatusError{StatusCode: 126}
	}

	return statusError
}
