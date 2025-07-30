package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/namespaces"
	launcher "github.com/jianlinjiang/trusted-launcher"
	"github.com/jianlinjiang/trusted-launcher/internal/logging"
	"github.com/jianlinjiang/trusted-launcher/launcherfile"
	"github.com/jianlinjiang/trusted-launcher/spec"
	"golang.org/x/oauth2"
)

const (
	successRC = 0 // workload successful (no reboot)
	failRC    = 1 // workload or launcher internal failed (no reboot)
	// panic() returns 2
	rebootRC = 3 // reboot
	holdRC   = 4 // hold
)

var rcMessage = map[int]string{
	successRC: "workload finished successfully, shutting down the VM",
	failRC:    "workload or launcher error, shutting down the VM",
	rebootRC:  "rebooting VM",
	holdRC:    "VM remains running",
}

// BuildCommit shows the commit when building the binary, set by -ldflags when building
var BuildCommit = "dev"

var logger logging.Logger

var welcomeMessage = "TEE container launcher initiating"
var exitMessage = "TEE container launcher exiting"

var start time.Time

func getUptime() (string, error) {
	file, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "", fmt.Errorf("error opening /proc/uptime: %v", err)
	}

	// proc/uptime contains two values separated by a space. We only need the first.
	split := bytes.Split(file, []byte(" "))
	if len(split) != 2 {
		return "", fmt.Errorf("unexpected /proc/uptime contents: %s", file)
	}

	return string(split[0]), nil
}

func main() {
	uptime, err := getUptime()
	if err != nil {
		logger.Error(fmt.Sprintf("error reading VM uptime: %v", err))
	}
	// Note the current time to later calculate launch time.
	start = time.Now()

	var exitCode int // by default exit code is 0
	ctx := context.Background()

	defer func() {
		os.Exit(exitCode)
	}()

	logger, err = logging.NewLogger(ctx)
	if err != nil {
		log.Default().Printf("failed to initialize logging: %v", err)
		exitCode = failRC
		log.Default().Printf("%s, exit code: %d (%s)\n", exitMessage, exitCode, rcMessage[exitCode])
		return
	}

	logger.Info("Boot completed", "duration_sec", uptime)
	logger.Info(welcomeMessage, "build_commit", BuildCommit)

	if err := os.MkdirAll(launcherfile.HostTmpPath, 0755); err != nil {
		logger.Error(fmt.Sprintf("failed to create %s: %v", launcherfile.HostTmpPath, err))
	}

	launchSpec, err := spec.GetLaunchSpec(ctx, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to get launchspec, make sure you're running inside a GCE VM: %v", err))
		// if cannot get launchSpec, exit directly
		exitCode = failRC
		logger.Error(exitMessage, "exit_code", exitCode, "exit_msg", rcMessage[exitCode])
		return
	}

	defer func() {
		// Catch panic to attempt to output to Cloud Logging.
		if r := recover(); r != nil {
			logger.Error(fmt.Sprintf("Panic: %v", r))
			exitCode = 2
		}
		msg, ok := rcMessage[exitCode]
		if ok {
			logger.Info(exitMessage, "exit_code", exitCode, "exit_msg", msg)
		} else {
			logger.Info(exitMessage, "exit_code", exitCode)
		}
	}()

	if err = startLauncher(launchSpec, logger.SerialConsoleFile()); err != nil {
		logger.Error(err.Error())
	}

	workloadDuration := time.Since(start)
	logger.Info("Workload completed",
		"workload", launchSpec.ImageRef,
		"workload_execution_sec", workloadDuration.Seconds(),
	)

	exitCode = getExitCode(launchSpec.Hardened, err)
}

func startLauncher(launchSpec spec.LaunchSpec, serialConsole *os.File) error {
	logger.Info(fmt.Sprintf("Launch Spec: %+v", launchSpec))
	containerdClient, err := containerd.New(defaults.DefaultAddress)
	if err != nil {
		return &launcher.RetryableError{Err: err}
	}
	defer containerdClient.Close()
	token := oauth2.Token{}
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	r, err := launcher.NewRunner(ctx, containerdClient, token, launchSpec, logger, serialConsole)
	if err != nil {
		return err
	}

	return r.Run(ctx)
}

func getExitCode(isHardened bool, err error) int {
	exitCode := 0

	// if in a debug image, will always hold
	if !isHardened {
		return holdRC
	}

	if err != nil {
		// switch err.(type) {
		// default:
		// 	// non-retryable error
		// 	exitCode = failRC
		// case *launcher.RetryableError, *launcher.WorkloadError:
		// 	if restartPolicy == spec.Always || restartPolicy == spec.OnFailure {
		// 		exitCode = rebootRC
		// 	} else {
		// 		exitCode = failRC
		// 	}
		// }
		exitCode = failRC
	} else {
		exitCode = successRC
		// if no erro
	}

	return exitCode
}
