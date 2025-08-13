package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/oci"
	"github.com/google/go-tpm-tools/cel"

	"github.com/jianlinjiang/trusted-launcher/agent"
	"github.com/jianlinjiang/trusted-launcher/internal/logging"
	"github.com/jianlinjiang/trusted-launcher/launcherfile"
	"github.com/jianlinjiang/trusted-launcher/spec"
	"github.com/jianlinjiang/trusted-launcher/teeserver"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/oauth2"
)

type ContainerRunner struct {
	container     containerd.Container
	launchSpec    spec.LaunchSpec
	attestAgent   agent.AttestationAgent
	logger        logging.Logger
	serialConsole *os.File
}

const teeServerSocket = "teeserver.sock"

// Since we only allow one container on a VM, using a deterministic id is probably fine
const (
	containerID = "tee-container"
	snapshotID  = "tee-snapshot"
)

const (
	nofile = 131072 // Max number of file descriptor
)

const defaultOOMScore = 1000

func NewRunner(ctx context.Context, cdClient *containerd.Client, token oauth2.Token, launchSpec spec.LaunchSpec, logger logging.Logger, serialConsole *os.File) (*ContainerRunner, error) {
	image, err := initImage(ctx, cdClient, launchSpec, token)
	if err != nil {
		return nil, err
	}

	var mounts []specs.Mount
	mounts = appendTokenMounts(mounts)
	envs, err := formatEnvVars(launchSpec.Envs)
	if err != nil {
		return nil, err
	}

	// Check if there is already a container
	container, err := cdClient.LoadContainer(ctx, containerID)
	if err == nil {
		// container exists, delete it first
		container.Delete(ctx, containerd.WithSnapshotCleanup)
	}

	logger.Info("Preparing Container Runner",
		"operator_input_image_ref", image.Name(),
		"image_digest", image.Target().Digest,
		"operator_override_env_vars", envs,
		"operator_override_cmd", launchSpec.Cmd,
	)

	imageConfig, err := getImageConfig(ctx, image)
	if err != nil {
		return nil, err
	}

	logger.Info(fmt.Sprintf("Exposed Ports:             : %v\n", imageConfig.ExposedPorts))

	if err := openPorts(imageConfig.ExposedPorts); err != nil {
		return nil, err
	}

	logger.Info(fmt.Sprintf("Image Labels               : %v\n", imageConfig.Labels))

	launchPolicy, err := spec.GetLaunchPolicy(imageConfig.Labels, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to parse image Launch Policy: %v: contact the image author", err)
	}
	if err := launchPolicy.Verify(launchSpec); err != nil {
		return nil, err
	}

	logger.Info(fmt.Sprintf("Launch Policy              : %+v\n", launchPolicy))

	if imageConfigDescriptor, err := image.Config(ctx); err != nil {
		logger.Error(err.Error())
	} else {
		logger.Info("Retrieved image config",
			"image_id", imageConfigDescriptor.Digest,
			"image_annotations", imageConfigDescriptor.Annotations,
		)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, &RetryableError{fmt.Errorf("cannot get hostname: [%w]", err)}
	}

	rlimits := []specs.POSIXRlimit{{
		Type: "RLIMIT_NOFILE",
		Hard: nofile,
		Soft: nofile,
	}}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfigArgs(image, launchSpec.Cmd),
		oci.WithEnv(envs),
		oci.WithMounts(mounts),
		// following 4 options are here to allow the container to have
		// the host network (same effect as --net-host in ctr command)
		oci.WithHostHostsFile,
		oci.WithHostResolvconf,
		oci.WithHostNamespace(specs.NetworkNamespace),
		oci.WithEnv([]string{fmt.Sprintf("HOSTNAME=%s", hostname)}),
		withRlimits(rlimits),
		withOOMScoreAdj(defaultOOMScore),
	}

	container, err = cdClient.NewContainer(
		ctx,
		containerID,
		containerd.WithImage(image),
		containerd.WithNewSnapshot(snapshotID, image),
		containerd.WithNewSpec(specOpts...),
	)
	if err != nil {
		if container != nil {
			container.Delete(ctx, containerd.WithSnapshotCleanup)
		}
		return nil, &RetryableError{fmt.Errorf("failed to create a container: [%w]", err)}
	}

	containerSpec, err := container.Spec(ctx)
	if err != nil {
		return nil, &RetryableError{err}
	}

	// Container process Args length should be strictly longer than the Cmd
	// override length set by the operator, as we want the Entrypoint filed
	// to be mandatory for the image.
	// Roughly speaking, Args = Entrypoint + Cmd
	if len(containerSpec.Process.Args) <= len(launchSpec.Cmd) {
		return nil,
			fmt.Errorf("length of Args [%d] is shorter or equal to the length of the given Cmd [%d], maybe the Entrypoint is set to empty in the image?",
				len(containerSpec.Process.Args), len(launchSpec.Cmd))
	}

	attestAgent, err := agent.CreateAttestationAgent(logger)

	if err != nil {
		return nil, err
	}
	return &ContainerRunner{
		container,
		launchSpec,
		attestAgent,
		logger,
		serialConsole,
	}, nil
}

func (r *ContainerRunner) measureCELEvents(ctx context.Context) error {
	if err := r.measureContainerClaims(ctx); err != nil {
		return fmt.Errorf("failed to measure container claims: %v", err)
	}

	separator := cel.CosTlv{
		EventType:    cel.LaunchSeparatorType,
		EventContent: nil, // Success
	}
	return r.attestAgent.MeasureEvent(separator)
}

// measureContainerClaims will measure various container claims into the COS
// eventlog in the AttestationAgent.
func (r *ContainerRunner) measureContainerClaims(ctx context.Context) error {
	image, err := r.container.Image(ctx)
	if err != nil {
		return err
	}
	if err := r.attestAgent.MeasureEvent(cel.CosTlv{EventType: cel.ImageRefType, EventContent: []byte(image.Name())}); err != nil {
		return err
	}
	if err := r.attestAgent.MeasureEvent(cel.CosTlv{EventType: cel.ImageDigestType, EventContent: []byte(image.Target().Digest)}); err != nil {
		return err
	}

	if imageConfigDescriptor, err := image.Config(ctx); err == nil { // if NO error
		if err := r.attestAgent.MeasureEvent(cel.CosTlv{EventType: cel.ImageIDType, EventContent: []byte(imageConfigDescriptor.Digest)}); err != nil {
			return err
		}
	}

	containerSpec, err := r.container.Spec(ctx)
	if err != nil {
		return err
	}
	for _, arg := range containerSpec.Process.Args {
		if err := r.attestAgent.MeasureEvent(cel.CosTlv{EventType: cel.ArgType, EventContent: []byte(arg)}); err != nil {
			return err
		}
	}
	for _, env := range containerSpec.Process.Env {
		if err := r.attestAgent.MeasureEvent(cel.CosTlv{EventType: cel.EnvVarType, EventContent: []byte(env)}); err != nil {
			return err
		}
	}

	// Measure the input overridden Env Vars and Args separately, these should be subsets of the Env Vars and Args above.
	envs, err := formatEnvVars(r.launchSpec.Envs)
	if err != nil {
		return err
	}
	for _, env := range envs {
		if err := r.attestAgent.MeasureEvent(cel.CosTlv{EventType: cel.OverrideEnvType, EventContent: []byte(env)}); err != nil {
			return err
		}
	}
	for _, arg := range r.launchSpec.Cmd {
		if err := r.attestAgent.MeasureEvent(cel.CosTlv{EventType: cel.OverrideArgType, EventContent: []byte(arg)}); err != nil {
			return err
		}
	}

	return nil
}

// Run the container
// Container output will always be redirected to logger writer for now
func (r *ContainerRunner) Run(ctx context.Context) error {
	// Note start time for workload setup.
	start := time.Now()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := r.measureCELEvents(ctx); err != nil {
		return fmt.Errorf("failed to measure CEL events: %v", err)
	}

	// create and start the TEE server
	r.logger.Info("EnableOnDemandAttestation is enabled: initializing TEE server.")

	teeServer, err := teeserver.New(ctx, path.Join(launcherfile.HostTmpPath, teeServerSocket), r.attestAgent, r.logger, r.launchSpec)
	if err != nil {
		return fmt.Errorf("failed to create the TEE server: %v", err)
	}
	go teeServer.Serve()
	defer teeServer.Shutdown(ctx)

	var streamOpt cio.Opt
	switch r.launchSpec.LogRedirect {
	case spec.Nowhere:
		streamOpt = cio.WithStreams(nil, nil, nil)
		r.logger.Info("Container stdout/stderr will not be redirected.")
	case spec.Everywhere:
		w := io.MultiWriter(os.Stdout, r.serialConsole)
		streamOpt = cio.WithStreams(nil, w, w)
		r.logger.Info("Container stdout/stderr will be redirected to serial and Cloud Logging. This may result in performance issues due to slow serial console writes.")
	case spec.CloudLogging:
		streamOpt = cio.WithStreams(nil, os.Stdout, os.Stdout)
		r.logger.Info("Container stdout/stderr will be redirected to Cloud Logging.")
	case spec.Serial:
		streamOpt = cio.WithStreams(nil, r.serialConsole, r.serialConsole)
		r.logger.Info("Container stdout/stderr will be redirected to serial logging. This may result in performance issues due to slow serial console writes.")
	default:
		return fmt.Errorf("unknown logging redirect location: %v", r.launchSpec.LogRedirect)
	}

	task, err := r.container.NewTask(ctx, cio.NewCreator(streamOpt))
	if err != nil {
		return &RetryableError{err}
	}
	defer task.Delete(ctx)

	setupDuration := time.Since(start)
	r.logger.Info("Workload setup completed",
		"setup_sec", setupDuration.Seconds(),
	)

	exitStatusC, err := task.Wait(ctx)
	if err != nil {
		r.logger.Error(err.Error())
	}
	// Start timer for workload execution.
	start = time.Now()
	r.logger.Info("workload task started")

	if err := task.Start(ctx); err != nil {
		return &RetryableError{err}
	}
	status := <-exitStatusC
	workloadDuration := time.Since(start)

	code, _, err := status.Result()
	if err != nil {
		return err
	}

	if code != 0 {
		r.logger.Error("workload task ended and returned non-zero",
			"workload_execution_sec", workloadDuration.Seconds(),
		)
		return &WorkloadError{code}
	}
	r.logger.Info("workload task ended and returned 0",
		"workload_execution_sec", workloadDuration.Seconds(),
	)
	return nil
}

func initImage(ctx context.Context, cdClient *containerd.Client, launchSpec spec.LaunchSpec, token oauth2.Token) (containerd.Image, error) {
	// if token.Valid() {
	// 	remoteOpt := containerd.WithResolver(registryauth.Resolver(token.AccessToken))
	// 	image, err := pullImageWithRetries(
	// 		func() (containerd.Image, error) {
	// 			return cdClient.Pull(ctx, launchSpec.ImageRef, containerd.WithPullUnpack, remoteOpt)
	// 		},
	// 		pullImageBackoffPolicy,
	// 	)
	// 	if err != nil {
	// 		return nil, fmt.Errorf("cannot pull the image: %w", err)
	// 	}
	// 	return image, nil
	// }
	image, err := pullImageWithRetries(
		func() (containerd.Image, error) {
			return cdClient.Pull(ctx, launchSpec.ImageRef, containerd.WithPullUnpack)
		},
		pullImageBackoffPolicy,
	)
	if err != nil {
		return nil, fmt.Errorf("cannot pull the image (no token, only works for a public image): %w", err)
	}
	return image, nil
}

func pullImageWithRetries(f func() (containerd.Image, error), retry func() backoff.BackOff) (containerd.Image, error) {
	var err error
	var image containerd.Image
	err = backoff.Retry(func() error {
		image, err = f()
		return err
	}, retry())
	if err != nil {
		return nil, fmt.Errorf("failed to pull image with retries, the last error is: %w", err)
	}
	return image, nil
}

func pullImageBackoffPolicy() backoff.BackOff {
	b := backoff.NewConstantBackOff(time.Millisecond * 500)
	return backoff.WithMaxRetries(b, 3)
}

// formatEnvVars formats the environment variables to the oci format
func formatEnvVars(envVars []spec.EnvVar) ([]string, error) {
	var result []string
	for _, envVar := range envVars {
		ociFormat, err := cel.FormatEnvVar(envVar.Name, envVar.Value)
		if err != nil {
			return nil, fmt.Errorf("failed to format env var: %v", err)
		}
		result = append(result, ociFormat)
	}
	return result, nil
}

func getImageConfig(ctx context.Context, image containerd.Image) (v1.ImageConfig, error) {
	ic, err := image.Config(ctx)
	if err != nil {
		return v1.ImageConfig{}, err
	}
	switch ic.MediaType {
	case v1.MediaTypeImageConfig, images.MediaTypeDockerSchema2Config:
		p, err := content.ReadBlob(ctx, image.ContentStore(), ic)
		if err != nil {
			return v1.ImageConfig{}, err
		}
		var ociimage v1.Image
		if err := json.Unmarshal(p, &ociimage); err != nil {
			return v1.ImageConfig{}, err
		}
		return ociimage.Config, nil
	}
	return v1.ImageConfig{}, fmt.Errorf("unknown image config media type %s", ic.MediaType)
}

// openPorts writes firewall rules to accept all traffic into that port and protocol using iptables.
func openPorts(ports map[string]struct{}) error {
	for k := range ports {
		portAndProtocol := strings.Split(k, "/")
		if len(portAndProtocol) != 2 {
			return fmt.Errorf("failed to parse port and protocol: got %s, expected [port]/[protocol] 80/tcp", portAndProtocol)
		}

		port := portAndProtocol[0]
		_, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return fmt.Errorf("received invalid port number: %v, %w", port, err)
		}

		protocol := portAndProtocol[1]
		if protocol != "tcp" && protocol != "udp" {
			return fmt.Errorf("received unknown protocol: got %s, expected tcp or udp", protocol)
		}

		// These 2 commands will write firewall rules to accept all INPUT packets for the given port/protocol
		// for IPv4 and IPv6 traffic.
		cmd := exec.Command("iptables", "-A", "INPUT", "-p", protocol, "--dport", port, "-j", "ACCEPT")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to open port on IPv4 %s %s: %v %s", port, protocol, err, out)
		}
		v6cmd := exec.Command("ip6tables", "-A", "INPUT", "-p", protocol, "--dport", port, "-j", "ACCEPT")
		out, err = v6cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to open port on IPv6 %s %s: %v %s", port, protocol, err, out)
		}
	}

	return nil
}

// withRlimits sets the rlimit (like the max file descriptor) for the container process
func withRlimits(rlimits []specs.POSIXRlimit) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		s.Process.Rlimits = rlimits
		return nil
	}
}

// Set the container process's OOM score.
func withOOMScoreAdj(oomScore int) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		s.Process.OOMScoreAdj = &oomScore
		return nil
	}
}

// appendTokenMounts appends the default mount specs for the OIDC token
func appendTokenMounts(mounts []specs.Mount) []specs.Mount {
	m := specs.Mount{}
	m.Destination = launcherfile.ContainerRuntimeMountPath
	m.Type = "bind"
	m.Source = launcherfile.HostTmpPath
	m.Options = []string{"rbind", "ro"}

	return append(mounts, m)
}
