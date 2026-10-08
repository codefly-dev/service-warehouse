package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
	dockerrun "github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/wool"

	whv0 "github.com/codefly-dev/service-warehouse/gen/codefly/warehouse/v0"
	"github.com/codefly-dev/service-warehouse/internal/auth"
)

// gatewayContainerPort is the port the gateway listens on inside its container.
// It is handed to the gateway as SWH_LISTEN and to the Kubernetes templates as
// the container port; TestGatewayContainerPortIsTheGatewaysDefault holds it to
// the gateway's own default and to the Dockerfile's EXPOSE.
const gatewayContainerPort = 9465

// gatewayContainerUser is the unprivileged uid:gid the gateway runs as. The
// published image already declares it, but SWH_GATEWAY_IMAGE can name another
// one; setting the user explicitly makes the guarantee hold for whichever image
// is run.
const gatewayContainerUser = "65532:65532"

// readinessBudget bounds the whole wait for the gateway in Start: one overall
// deadline, so a slow attempt spends the budget instead of extending it.
const readinessBudget = 60 * time.Second

// readinessProbeTimeout bounds a single attempt, and readinessPollInterval paces
// the retries between them.
const (
	readinessProbeTimeout = 5 * time.Second
	readinessPollInterval = time.Second
)

// containerEnvironment is the part of dockerrun.DockerEnvironment the Runtime
// owns: something it can identify and tear down. Teardown is the path that has
// to keep working when Docker does not, so it is reachable through this seam.
type containerEnvironment interface {
	ContainerID() (string, error)
	Shutdown(context.Context) error
}

type Runtime struct {
	*services.DefaultRuntime
	*Service

	gatewayEnv containerEnvironment
}

func NewRuntime() *Runtime {
	service := NewService()
	return &Runtime{
		DefaultRuntime: services.NewDefaultRuntime(service.Runtime),
		Service:        service,
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	return s.Runtime.LoadService(ctx, req, services.RuntimeLoad{
		Settings:     s.Settings,
		Requirements: requirements,
		ResolveEndpoints: func(ctx context.Context, endpoints []*basev0.Endpoint) error {
			endpoint, err := resolveServingGRPCEndpoint(ctx, endpoints)
			if err != nil {
				return s.Wool.Wrapf(err, "cannot find grpc endpoint")
			}
			s.GrpcEndpoint = endpoint
			return nil
		},
	})
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)
	s.Runtime.WithContext(req.GetRuntimeContext())
	s.NetworkMappings = req.ProposedNetworkMappings

	w := s.Wool.In("runtime::init")
	configuration := req.GetConfiguration()

	net, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, s.GrpcEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GrpcEndpoint, s.Runtime.NetworkAccess())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if err = s.LoadConfiguration(ctx, configuration); err != nil {
		return s.Runtime.InitError(err)
	}
	// Refuse before any container exists: a path the gateway cannot open must
	// fail here with its own cause, not as a gateway that comes up and cannot
	// authenticate to its warehouse.
	if s.conf.credentialsFile != "" {
		return s.Runtime.InitError(s.Wool.NewError("SWH_CREDENTIALS_FILE cannot be delivered to a local run: "+
			"nothing here projects %s into the gateway container, which would name a path it cannot open. "+
			"A local run is for the embedded engine, which needs no credential; "+
			"a cloud warehouse is reached from a deployment", s.conf.credentialsFile))
	}

	s.gatewayToken, err = s.resolveGatewayToken()
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if err = s.startGateway(ctx, uint16(instance.Port)); err != nil {
		s.rollbackInit(ctx)
		return s.Runtime.InitError(err)
	}

	for _, inst := range net.Instances {
		conf, errConn := s.CreateConnectionConfiguration(ctx, configuration, inst)
		if errConn != nil {
			return s.Runtime.InitError(errConn)
		}
		s.Runtime.RuntimeConfigurations = append(s.Runtime.RuntimeConfigurations, conf)
	}

	w.Debug("init successful")
	return s.Runtime.InitResponse()
}

// resolveGatewayToken returns the secret the gateway will enforce and consumers
// will present. A configured token is honored so an operator who pins one is not
// silently overridden; otherwise a fresh one is generated per run, which is what
// stands between the all-interface published port and SQL against the bound
// database, and which revokes every token a previous run handed out.
//
// Generating a new token changes the gateway container's environment, and
// dockerrun fingerprints the environment, so each run deliberately recreates the
// container rather than reattaching to the previous one. Making the token stable
// to recover that reattach would silently give up the revocation property.
func (s *Runtime) resolveGatewayToken() (string, error) {
	if s.conf.authToken != "" {
		return s.conf.authToken, nil
	}
	return randomSecret()
}

// startGateway runs the gateway container, mapping the assigned host port onto
// the gateway's listen port and handing it the resolved configuration. A value
// nobody set is not passed, so the gateway's own default decides it.
func (s *Runtime) startGateway(ctx context.Context, hostPort uint16) error {
	image, _, err := effectiveGatewayImage()
	if err != nil {
		return err
	}

	runner, err := dockerrun.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace()+"-gateway")
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create gateway environment")
	}
	// Own the environment before starting it: a container that is created and
	// then exits still has to be removed, and only Shutdown does that.
	s.gatewayEnv = runner
	runner.WithOutput(os.Stdout)
	runner.WithUser(gatewayContainerUser)
	runner.WithPortMapping(ctx, hostPort, gatewayContainerPort)
	// Consumers running in their own container reach the gateway through
	// host.docker.internal (the Container network instance the agent advertises),
	// which resolves to the bridge gateway on Linux — unreachable if the port is
	// bound only to 127.0.0.1. Publish on all interfaces; what makes that safe is
	// SWH_AUTH_TOKEN below, not the binding.
	runner.WithPublicPorts()
	envs := []*resources.EnvironmentVariable{
		resources.Env("SWH_LISTEN", fmt.Sprintf(":%d", gatewayContainerPort)),
		resources.Env("SWH_AUTH_TOKEN", s.gatewayToken),
	}
	for _, value := range []struct{ name, value string }{
		{"SWH_BACKEND", s.conf.backend},
		{"SWH_DATABASE", s.conf.database},
		{"SWH_DATASET", s.conf.dataset},
		{"SWH_LOCATION", s.conf.location},
	} {
		if value.value != "" {
			envs = append(envs, resources.Env(value.name, value.value))
		}
	}
	runner.WithEnvironmentVariables(ctx, envs...)
	if err = runner.Init(ctx); err != nil {
		return s.Wool.Wrapf(err, "cannot start gateway")
	}
	return nil
}

// teardown releases everything the Runtime owns. An environment that fails to
// shut down keeps its reference so a later attempt can retry it.
func (s *Runtime) teardown(ctx context.Context) error {
	if s.gatewayEnv == nil {
		return nil
	}
	if err := s.gatewayEnv.Shutdown(ctx); err != nil {
		return s.Wool.Wrapf(err, "cannot shut down gateway")
	}
	s.gatewayEnv = nil
	return nil
}

// rollbackInit releases what Init already owns after a failed startup. The
// startup error is what the caller reports, so a teardown failure is logged
// rather than substituted for it.
func (s *Runtime) rollbackInit(ctx context.Context) {
	if err := s.teardown(ctx); err != nil {
		s.Wool.Warn("cannot fully roll back after failed init", wool.ErrField(err))
	}
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	if err := s.WaitForReady(ctx); err != nil {
		return s.Runtime.StartError(err)
	}
	return s.Runtime.StartResponse()
}

// WaitForReady polls the gateway until its health service reports it serving
// and it has accepted the agent's credential.
//
// This is liveness plus the credential, and says nothing about the warehouse:
// the gateway does not probe its backend and no RPC reports that, so a gateway
// whose warehouse is unreachable is still ready here and fails the first query
// that needs it (README, "Authentication"). Capabilities is the second probe
// because it is the cheapest call that is not exempt from authentication — the
// health service carries no token — so a mismatched token surfaces here as
// itself rather than at a consumer's first call.
//
// The gateway is addressed through the NATIVE network view. The agent is a host
// process whatever runtime context the service itself was given, and a
// container-only name resolves to nothing from here.
func (s *Runtime) WaitForReady(ctx context.Context) error {
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.GrpcEndpoint, resources.NewNativeNetworkAccess())
	if err != nil {
		return s.Wool.Wrapf(err, "cannot find network instance")
	}
	address := instance.Address
	s.Wool.Debug("waiting for warehouse gateway", wool.Field("address", address))

	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		auth.DialOption(s.gatewayToken))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create gateway client for %s", address)
	}
	defer func() { _ = conn.Close() }()
	health := healthv1.NewHealthClient(conn)
	warehouse := whv0.NewWarehouseClient(conn)

	start := time.Now()
	budget, cancel := context.WithTimeout(ctx, readinessBudget)
	defer cancel()
	ticker := time.NewTicker(readinessPollInterval)
	defer ticker.Stop()

	lastErr := "no attempt completed"
poll:
	for {
		// Stop rather than start a probe the budget cannot see through: a
		// truncated attempt reports this deadline and buries the cause the last
		// full attempt established.
		if deadline, ok := budget.Deadline(); ok && time.Until(deadline) < readinessProbeTimeout {
			break
		}

		attempt, attemptCancel := context.WithTimeout(budget, readinessProbeTimeout)
		lastErr, err = probeGateway(attempt, health, warehouse)
		attemptCancel()
		if err != nil {
			return s.Wool.Wrapf(err, "warehouse gateway rejected the agent's credential")
		}
		if lastErr == "" {
			return nil
		}

		select {
		case <-budget.Done():
			break poll
		case <-ticker.C:
		}
	}
	return s.Wool.NewError("warehouse gateway at %s not ready after %s: %s",
		address, time.Since(start).Round(time.Millisecond), lastErr)
}

// probeGateway makes one readiness attempt. A non-empty reason means the gateway
// is not ready yet and the attempt is worth repeating; a non-nil error is final.
// A rejected credential is not a gateway that is still coming up: retrying
// cannot change the answer, and spending the budget on it reports the failure as
// readiness instead of as the mismatched token it is.
func probeGateway(ctx context.Context, health healthv1.HealthClient, warehouse whv0.WarehouseClient) (reason string, final error) {
	res, err := health.Check(ctx, &healthv1.HealthCheckRequest{})
	if err != nil {
		return err.Error(), nil
	}
	if res.GetStatus() != healthv1.HealthCheckResponse_SERVING {
		return fmt.Sprintf("health status %s", res.GetStatus()), nil
	}
	if _, err = warehouse.Capabilities(ctx, &whv0.CapabilitiesRequest{}); err != nil {
		if status.Code(err) == codes.Unauthenticated {
			return "", err
		}
		return err.Error(), nil
	}
	return "", nil
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	if err := s.teardown(ctx); err != nil {
		return s.Runtime.DestroyError(err)
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}

// randomSecret returns an unguessable hex secret, so the published port is never
// reachable with a well-known credential.
func randomSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
