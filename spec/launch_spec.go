package spec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jianlinjiang/trusted-launcher/internal/logging"
	"github.com/jianlinjiang/trusted-launcher/metadata"
)

// Metadata variable names.
const (
	imageRefKey    = "tee-image-reference"
	envKeyPrefix   = "tee-env-"
	cmdKey         = "tee-cmd"
	logRedirectKey = "tee-container-log-redirect"
)

var errImageRefNotSpecified = fmt.Errorf("%s is not specified in the custom metadata", imageRefKey)

// EnvVar represent a single environment variable key/value pair.
type EnvVar struct {
	Name  string
	Value string
}

// LogRedirectLocation specifies the workload logging redirect location.
type LogRedirectLocation string

func (l LogRedirectLocation) isValid() error {
	switch l {
	case Everywhere, CloudLogging, Serial, Nowhere:
		return nil
	}
	return fmt.Errorf("invalid logging redirect location %s, expect one of %s", l,
		[]LogRedirectLocation{Everywhere, CloudLogging, Serial, Nowhere})
}

func (l LogRedirectLocation) enabled() bool {
	return l != Nowhere
}

// LogRedirectLocation acceptable values.
const (
	Everywhere   LogRedirectLocation = "true"
	CloudLogging LogRedirectLocation = "cloud_logging"
	Serial       LogRedirectLocation = "serial"
	Nowhere      LogRedirectLocation = "false"
)

type LaunchSpec struct {
	ImageRef    string
	Cmd         []string
	Envs        []EnvVar
	Hardened    bool
	LogRedirect LogRedirectLocation
}

func GetLaunchSpec(ctx context.Context, logger logging.Logger) (LaunchSpec, error) {
	s := &LaunchSpec{}
	unmarshaledMap, err := metadata.ReadUserData()
	if err != nil {
		return LaunchSpec{}, err
	}

	s.ImageRef = unmarshaledMap[imageRefKey]
	if s.ImageRef == "" {
		return LaunchSpec{}, errImageRefNotSpecified
	}

	// Populate cmd override.
	if val, ok := unmarshaledMap[cmdKey]; ok && val != "" {
		if err := json.Unmarshal([]byte(val), &s.Cmd); err != nil {
			return LaunchSpec{}, err
		}
	}

	// Populate all env vars.
	for k, v := range unmarshaledMap {
		if strings.HasPrefix(k, envKeyPrefix) {
			s.Envs = append(s.Envs, EnvVar{strings.TrimPrefix(k, envKeyPrefix), v})
		}
	}

	s.LogRedirect = LogRedirectLocation(unmarshaledMap[logRedirectKey])
	// Default log redirect location is Nowhere ("false").
	if s.LogRedirect == "" {
		s.LogRedirect = Nowhere
	}
	if err := s.LogRedirect.isValid(); err != nil {
		return LaunchSpec{}, err
	}

	kernelCmd, err := readCmdline()
	if err != nil {
		return LaunchSpec{}, err
	}
	s.Hardened = isHardened(kernelCmd)
	return *s, nil
}

func readCmdline() (string, error) {
	kernelCmd, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return "", err
	}
	return string(kernelCmd), nil
}

func isHardened(kernelCmd string) bool {
	for _, arg := range strings.Fields(kernelCmd) {
		if arg == "confidential-space.hardened=true" {
			return true
		}
	}
	return false
}
