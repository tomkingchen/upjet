/*
Copyright 2022 Upbound Inc.
*/

package terraform

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/pkg/errors"
	"k8s.io/utils/clock"
	"k8s.io/utils/exec"
)

const (
	// error messages
	errFmtTimeout = "timed out after %v while waiting for the reattach configuration string"

	// an example value would be: '{"registry.terraform.io/hashicorp/aws": {"Protocol": "grpc", "ProtocolVersion":5, "Pid":... "Addr":{"Network": "unix","String": "..."}}}'
	fmtReattachEnv    = `{"%s":{"Protocol":"grpc","ProtocolVersion":%d,"Pid":%d,"Test": true,"Addr":{"Network": "unix","String": "%s"}}}`
	fmtSetEnv         = "%s=%s"
	envReattachConfig = "TF_REATTACH_PROVIDERS"
	envMagicCookie    = "TF_PLUGIN_MAGIC_COOKIE"
	// Terraform provider plugin expects this magic cookie in its environment
	// (as the value of key TF_PLUGIN_MAGIC_COOKIE):
	// https://github.com/hashicorp/terraform/blob/d35bc0531255b496beb5d932f185cbcdb2d61a99/internal/plugin/serve.go#L33
	valMagicCookie         = "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2"
	defaultProtocolVersion = 5
	reattachTimeout        = 1 * time.Minute
)

var (
	regexReattachLine = regexp.MustCompile(`.*unix\|(.*)\|grpc.*`)
)

// ProviderRunner is the interface for running
// Terraform native provider processes in the shared
// gRPC server mode
type ProviderRunner interface {
	Start() (string, error)
}

// NoOpProviderRunner is a no-op ProviderRunner
type NoOpProviderRunner struct{}

// NewNoOpProviderRunner constructs a new NoOpProviderRunner
func NewNoOpProviderRunner() NoOpProviderRunner {
	return NoOpProviderRunner{}
}

// Start takes no action
func (NoOpProviderRunner) Start() (string, error) {
	return "", nil
}

// SharedProvider runs the configured native provider plugin
// using the supplied command-line args
type SharedProvider struct {
	nativeProviderPath string
	nativeProviderArgs []string
	reattachConfig     string
	nativeProviderName string
	protocolVersion    int
	logger             logging.Logger
	executor           exec.Interface
	clock              clock.Clock
	mu                 *sync.Mutex
}

// SharedGRPCRunnerOption lets you configure the shared gRPC runner.
type SharedGRPCRunnerOption func(runner *SharedProvider)

// WithNativeProviderArgs are the arguments to be passed to the native provider
func WithNativeProviderArgs(args ...string) SharedGRPCRunnerOption {
	return func(sr *SharedProvider) {
		sr.nativeProviderArgs = args
	}
}

// WithNativeProviderExecutor sets the process executor to be used
func WithNativeProviderExecutor(e exec.Interface) SharedGRPCRunnerOption {
	return func(sr *SharedProvider) {
		sr.executor = e
	}
}

// WithProtocolVersion sets the gRPC protocol version in use between
// the Terraform CLI and the native provider.
func WithProtocolVersion(protocolVersion int) SharedGRPCRunnerOption {
	return func(sr *SharedProvider) {
		sr.protocolVersion = protocolVersion
	}
}

// NewSharedProvider instantiates a SharedProvider with an
// OS executor using the supplied logger
func NewSharedProvider(l logging.Logger, nativeProviderPath, nativeProviderName string, opts ...SharedGRPCRunnerOption) *SharedProvider {
	sr := &SharedProvider{
		logger:             l,
		nativeProviderPath: nativeProviderPath,
		nativeProviderName: nativeProviderName,
		protocolVersion:    defaultProtocolVersion,
		executor:           exec.New(),
		clock:              clock.RealClock{},
		mu:                 &sync.Mutex{},
	}
	for _, o := range opts {
		o(sr)
	}
	return sr
}

// Start starts a shared gRPC server if not already running
// A logger, native provider's path and command-line arguments to be
// passed to it must have been properly configured.
// Returns any errors encountered and the reattachment configuration for
// the native provider.
func (sr *SharedProvider) Start() (string, error) { //nolint:gocyclo
	sr.mu.Lock()
	defer sr.mu.Unlock()
	log := sr.logger.WithValues("nativeProviderPath", sr.nativeProviderPath, "nativeProviderArgs", sr.nativeProviderArgs)
	if sr.reattachConfig != "" {
		log.Debug("Shared gRPC server is running...", "reattachConfig", sr.reattachConfig)
		return sr.reattachConfig, nil
	}
	errCh := make(chan error, 1)
	reattachCh := make(chan string, 1)

	go func() {
		defer close(errCh)
		defer close(reattachCh)
		defer func() {
			sr.mu.Lock()
			sr.reattachConfig = ""
			sr.mu.Unlock()
		}()
		//#nosec G204 no user input
		cmd := sr.executor.Command(sr.nativeProviderPath, sr.nativeProviderArgs...)
		cmd.SetEnv(append(os.Environ(), fmt.Sprintf(fmtSetEnv, envMagicCookie, valMagicCookie)))
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			errCh <- err
			return
		}
		if err := cmd.Start(); err != nil {
			errCh <- err
			return
		}
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			t := scanner.Text()
			matches := regexReattachLine.FindStringSubmatch(t)
			if matches == nil {
				continue
			}
			reattachCh <- fmt.Sprintf(fmtReattachEnv, sr.nativeProviderName, sr.protocolVersion, os.Getpid(), matches[1])
			break
		}
		if err := cmd.Wait(); err != nil {
			log.Info("Native Terraform provider process error", "error", err)
			errCh <- err
		}
	}()

	select {
	case reattachConfig := <-reattachCh:
		sr.reattachConfig = reattachConfig
		return sr.reattachConfig, nil
	case err := <-errCh:
		return "", err
	case <-sr.clock.After(reattachTimeout):
		return "", errors.Errorf(errFmtTimeout, reattachTimeout)
	}
}
