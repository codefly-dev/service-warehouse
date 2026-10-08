package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	agenttesting "github.com/codefly-dev/core/agents/testing"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// withPublishedLock makes the agent see a gateway image that was published, for
// the tests of what it does with one. They have to mean the same thing before
// the first image exists and after, and the file on disk decides neither.
func withPublishedLock(t *testing.T) {
	t.Helper()
	previous := gatewayImageLockJSON
	gatewayImageLockJSON = []byte(`{"name": "` + registry + `/service-warehouse", "digest": "` + digest + `"}`)
	t.Cleanup(func() { gatewayImageLockJSON = previous })
}

func TestNewService_EmbedsBase(t *testing.T) {
	svc := NewService()
	require.NotNil(t, svc.Base, "services.Base embedding broken")
	require.NotNil(t, svc.Settings)
}

func TestLoadConfigurationDefaults(t *testing.T) {
	svc := NewService()
	require.NoError(t, svc.LoadConfiguration(context.Background(), nil))
	// Nothing is defaulted here: an unset value is the gateway's to decide, and
	// a default typed in the agent would be a second opinion about it.
	require.Equal(t, resolved{}, svc.conf)
}

func configuration(values map[string]string) *basev0.Configuration {
	var configured []*basev0.ConfigurationValue
	for key, value := range values {
		configured = append(configured, &basev0.ConfigurationValue{Key: key, Value: value})
	}
	return &basev0.Configuration{Infos: []*basev0.ConfigurationInformation{{
		Name:                configurationGroup,
		ConfigurationValues: configured,
	}}}
}

func TestLoadConfigurationRuntimeOverridesSettings(t *testing.T) {
	svc := NewService()
	svc.Backend, svc.Database, svc.Dataset, svc.WarehouseLocation = "duckdb", "local.duckdb", "scratch", "eu"

	require.NoError(t, svc.LoadConfiguration(context.Background(), configuration(map[string]string{
		"SWH_BACKEND":  "bigquery",
		"SWH_DATABASE": "example-project",
		"SWH_DATASET":  "analytics",
		"SWH_LOCATION": "us-central1",
	})))
	require.Equal(t, resolved{backend: "bigquery", database: "example-project", dataset: "analytics", location: "us-central1"}, svc.conf)

	// A value the environment does not supply keeps the service's own setting.
	require.NoError(t, svc.LoadConfiguration(context.Background(), configuration(map[string]string{"SWH_DATASET": "other"})))
	require.Equal(t, resolved{backend: "duckdb", database: "local.duckdb", dataset: "other", location: "eu"}, svc.conf)
}

// A secret or a path delivered through a YAML block scalar or an env file
// arrives with a trailing newline. Untrimmed, the token is handed to consumers
// in one form and enforced in another, and a path names nothing.
func TestLoadConfigurationNormalizesWhitespace(t *testing.T) {
	svc := NewService()
	require.NoError(t, svc.LoadConfiguration(context.Background(), configuration(map[string]string{
		"SWH_AUTH_TOKEN":       "pinned-token\n",
		"SWH_CREDENTIALS_FILE": "  /secrets/key.json\n",
		"SWH_BACKEND":          "mem\n",
	})))
	require.Equal(t, "pinned-token", svc.conf.authToken)
	require.Equal(t, "/secrets/key.json", svc.conf.credentialsFile)
	require.Equal(t, "mem", svc.conf.backend)
}

func TestResolveGatewayToken(t *testing.T) {
	rt := NewRuntime()

	pinned := "operator-pinned"
	rt.conf.authToken = pinned
	got, err := rt.resolveGatewayToken()
	require.NoError(t, err)
	require.Equal(t, pinned, got, "an operator who pins a token is not silently overridden")

	rt.conf.authToken = ""
	first, err := rt.resolveGatewayToken()
	require.NoError(t, err)
	second, err := rt.resolveGatewayToken()
	require.NoError(t, err)
	require.Len(t, first, 48, "24 random bytes, hex encoded")
	require.NotEqual(t, first, second, "each run mints a fresh token, which is what revokes the last run's")
}

func TestResolveServingGRPCEndpoint(t *testing.T) {
	endpoint, err := resolveServingGRPCEndpoint(context.Background(), []*basev0.Endpoint{{Name: "grpc", Api: "grpc"}})
	require.NoError(t, err)
	require.Equal(t, "grpc", endpoint.Name)

	_, err = resolveServingGRPCEndpoint(context.Background(), nil)
	require.Error(t, err, "expected error when no grpc endpoint is present")
}

func TestConnectionString(t *testing.T) {
	require.Equal(t, "grpc://localhost:9465", NewService().createConnectionString("localhost:9465"))
}

func headlessService(t *testing.T) *Service {
	t.Helper()
	svc := NewService()
	identity := &basev0.ServiceIdentity{Workspace: "workspace", Module: "module", Name: "warehouse", Version: "1.2.3", WorkspacePath: t.TempDir(), RelativeToWorkspace: "."}
	require.NoError(t, svc.Base.HeadlessLoad(context.Background(), identity))
	return svc
}

func connectionValues(t *testing.T, svc *Service) map[string]*basev0.ConfigurationValue {
	t.Helper()
	conf, err := svc.CreateConnectionConfiguration(context.Background(), nil,
		&basev0.NetworkInstance{Address: "localhost:9465", Access: resources.NewNativeNetworkAccess()})
	require.NoError(t, err)
	require.Len(t, conf.Infos, 1)
	require.Equal(t, configurationGroup, conf.Infos[0].Name)
	values := map[string]*basev0.ConfigurationValue{}
	for _, value := range conf.Infos[0].ConfigurationValues {
		values[value.Key] = value
	}
	return values
}

// TestConnectionConfigurationCarriesTokenAsSecret pins how a consumer receives
// the gateway credential: as its own secret-marked value, never folded into the
// connection string or endpoint that get logged and templated everywhere.
func TestConnectionConfigurationCarriesTokenAsSecret(t *testing.T) {
	svc := headlessService(t)
	const token = "b6f1a0c9d8e7f6a5b4c3d2e1f0a9b8c7"
	svc.gatewayToken = token

	values := connectionValues(t, svc)

	require.Contains(t, values, "token")
	require.Equal(t, token, values["token"].Value)
	require.True(t, values["token"].Secret, "token must be marked secret so it is never logged or displayed")
	for _, key := range []string{"connection", "endpoint"} {
		require.NotContains(t, values[key].GetValue(), token, "%s leaks the token", key)
	}
	require.Equal(t, "grpc://localhost:9465", values["connection"].Value)
	require.Equal(t, "localhost:9465", values["endpoint"].Value)
}

// The Builder shares the Service but never generates a token: it must not
// advertise an empty credential a consumer would then try to present.
func TestConnectionConfigurationWithoutTokenOmitsIt(t *testing.T) {
	require.NotContains(t, connectionValues(t, headlessService(t)), "token")
}

func TestAgentInformationDocumentsTheConfigurationGroup(t *testing.T) {
	info, err := NewService().GetAgentInformation(context.Background(), &agentv0.AgentInformationRequest{})
	require.NoError(t, err)
	require.Len(t, info.GetConfigurationDetails(), 1)
	group := info.GetConfigurationDetails()[0]
	require.Equal(t, configurationGroup, group.GetName())
	var fields []string
	for _, field := range group.GetFields() {
		fields = append(fields, field.GetName())
	}
	require.Equal(t, []string{"connection", "endpoint", "token"}, fields)
	require.Contains(t, info.GetReadMe(), "warehouse")
}

func TestSettings_YAMLRoundTrip(t *testing.T) {
	var s Settings
	require.NoError(t, yaml.Unmarshal([]byte("backend: bigquery\ndatabase: example-project\ndataset: analytics\nlocation: eu\n"), &s))
	require.Equal(t, Settings{Backend: "bigquery", Database: "example-project", Dataset: "analytics", WarehouseLocation: "eu"}, s)
}

func TestDeploymentTemplates(t *testing.T) {
	cases := map[string]deploymentTemplateParameters{
		"nothing chosen": {ServicePort: gatewayContainerPort, ContainerPort: gatewayContainerPort},
		"bigquery": {
			Backend: "bigquery", Database: "example-project", Dataset: "analytics", Location: "us-central1",
			ServicePort: gatewayContainerPort, ContainerPort: gatewayContainerPort,
		},
		"another service port": {Backend: "mem", ServicePort: 9090, ContainerPort: gatewayContainerPort},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			dir := agenttesting.AssertKustomizeTemplatesWithOverlay(t, deploymentFS, &params, workloadPodOverlay())
			manifest, err := os.ReadFile(filepath.Join(dir, "base", "deployment.yaml"))
			require.NoError(t, err)
			body := string(manifest)

			env := deploymentEnv(t, body)
			for key, want := range map[string]string{
				"SWH_BACKEND": params.Backend, "SWH_DATABASE": params.Database,
				"SWH_DATASET": params.Dataset, "SWH_LOCATION": params.Location,
			} {
				if want == "" {
					require.NotContains(t, env, key, "an unset value must render no variable, leaving the choice to the gateway")
					continue
				}
				require.Equal(t, want, env[key])
			}
			require.Equal(t, ":9465", env["SWH_LISTEN"])
			// The gateway refuses an unauthenticated listener unless the manifest
			// says so, so a deployment that stops emitting this opt-out without
			// also delivering SWH_AUTH_TOKEN would crash-loop.
			require.Equal(t, "true", env["SWH_ALLOW_ANONYMOUS"], "the deployed profile must declare its unauthenticated listener")
			require.NotContains(t, env, "SWH_AUTH_TOKEN", "the manifest must not carry the gateway credential")
			// No rendering names a credentials path: nothing in this manifest
			// mounts one, so an emitted path would point at a file the pod does
			// not have. Deploy rejects a configured path instead. Matched on the
			// bare name so that explaining the absence in rendered YAML fails too.
			require.NotContains(t, body, "SWH_CREDENTIALS_FILE")
			requireProbeSemantics(t, body)
		})
	}
}

// deploymentEnv reads the gateway container's literal environment from a
// rendered Deployment, refusing a key rendered twice: the CLI's binding of the
// environment's configuration replaces literals by name.
func deploymentEnv(t *testing.T, body string) map[string]string {
	t.Helper()
	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct {
							Name  string `yaml:"name"`
							Value string `yaml:"value"`
						} `yaml:"env"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(body), &deployment))
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	env := map[string]string{}
	for _, e := range deployment.Spec.Template.Spec.Containers[0].Env {
		require.NotContains(t, env, e.Name, "env %s rendered twice; the CLI binding refuses a repeated key", e.Name)
		env[e.Name] = e.Value
	}
	return env
}

// requireProbeSemantics pins what the rendered probes attest. A TCP probe
// passes as soon as the gRPC port is bound, and the gateway's health service
// reports the process and not the warehouse, so all three probes check the
// overall service: naming one would attest something the gateway does not
// report, and a probe that followed a warehouse outage would drain every replica
// at once, since they share one warehouse and one credential set.
func requireProbeSemantics(t *testing.T, body string) {
	t.Helper()
	require.NotContains(t, body, "tcpSocket", "probes must not settle for a bound socket")

	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []map[string]any `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(body), &deployment))
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	container := deployment.Spec.Template.Spec.Containers[0]
	for _, name := range []string{"startupProbe", "readinessProbe", "livenessProbe"} {
		probe, ok := container[name].(map[string]any)
		require.True(t, ok, "%s is missing", name)
		grpc, isGRPC := probe["grpc"].(map[string]any)
		require.True(t, isGRPC, "%s is not a grpc probe: %v", name, probe)
		require.Equal(t, gatewayContainerPort, grpc["port"], name)
		require.NotContains(t, grpc, "service", "%s must check the overall service", name)
	}
}

// newDeployBuilder returns a Builder wired for a headless Deploy call, against a
// gateway image that was published.
func newDeployBuilder(t *testing.T, ctx context.Context) *Builder {
	t.Helper()
	withPublishedLock(t)
	builder := NewBuilder()
	identity := &basev0.ServiceIdentity{Workspace: "workspace", Module: "module", Name: "warehouse", Version: "1.2.3", WorkspacePath: t.TempDir(), RelativeToWorkspace: "."}
	require.NoError(t, builder.Base.HeadlessLoad(ctx, identity))
	builder.Base.Information = &services.Information{Service: resources.ToServiceWithCase(resources.ServiceIdentityFromProto(identity))}
	builder.Base.EnvironmentVariables.SetIdentity(identity)
	return builder
}

// deployRequest builds a Kubernetes DeploymentRequest carrying the given
// warehouse configuration values, rendering into destination.
func deployRequest(destination string, values map[string]string, profile builderv0.KubernetesOutputProfile) *builderv0.DeploymentRequest {
	return &builderv0.DeploymentRequest{
		Environment: &basev0.Environment{Name: "test", Fixture: "dev-admin"},
		Deployment: &builderv0.Deployment{Kind: &builderv0.Deployment_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeployment{
				Namespace:   "codefly",
				Destination: destination,
				Profile:     profile,
			},
		}},
		Configuration: configuration(values),
	}
}

const (
	ephemeral  = builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	restricted = builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1
)

func renderedDeployment(t *testing.T, destination string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(destination, "base", "deployment.yaml"))
	require.NoError(t, err)
	return string(body)
}

// TestDeployResolvesConfigurationIntoTheManifest drives the real Builder.Deploy
// path and asserts the rendered manifest names the configured values: the
// regression guard for a Deploy that never loaded the configuration, leaving the
// manifest on its zero value whatever the environment said. Every key goes
// through the full config-key -> resolved -> template wiring, so a typo'd map key
// fails here rather than shipping green.
func TestDeployResolvesConfigurationIntoTheManifest(t *testing.T) {
	ctx := context.Background()
	builder := newDeployBuilder(t, ctx)
	destination := t.TempDir()

	resp, err := builder.Deploy(ctx, deployRequest(destination, map[string]string{
		"SWH_BACKEND":  "bigquery",
		"SWH_DATABASE": "example-project",
		"SWH_DATASET":  "analytics",
		"SWH_LOCATION": "us-central1",
	}, ephemeral))
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())

	env := deploymentEnv(t, renderedDeployment(t, destination))
	require.Equal(t, "bigquery", env["SWH_BACKEND"])
	require.Equal(t, "example-project", env["SWH_DATABASE"])
	require.Equal(t, "analytics", env["SWH_DATASET"])
	require.Equal(t, "us-central1", env["SWH_LOCATION"])
	require.Equal(t, "warehouse", env["CODEFLY__SERVICE"], "the CLI binds environment configuration onto the container declaring this")
}

// The composed service's own settings are what a render with no environment
// values carries: the environment's configuration is bound onto the container
// after the render, replacing the SWH_* literals by name.
func TestDeployRendersTheServiceSettingsWhenTheEnvironmentSuppliesNone(t *testing.T) {
	ctx := context.Background()
	builder := newDeployBuilder(t, ctx)
	builder.Settings.Backend = "bigquery"
	destination := t.TempDir()

	resp, err := builder.Deploy(ctx, deployRequest(destination, nil, restricted))
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())
	require.Equal(t, "bigquery", deploymentEnv(t, renderedDeployment(t, destination))["SWH_BACKEND"])
}

func TestDeployRefusesWhatItHasNoWayToDeliver(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		values map[string]string
		want   string
	}{
		"a credentials file nothing mounts": {map[string]string{"SWH_CREDENTIALS_FILE": "/keys/sa.json"}, "SWH_CREDENTIALS_FILE cannot be delivered"},
		"a token the consumers would lack":  {map[string]string{"SWH_AUTH_TOKEN": "pinned"}, "SWH_AUTH_TOKEN cannot be delivered"},
		"a value that would write the manifest": {
			map[string]string{"SWH_DATASET": "x\"\n          - name: SWH_AUTH_TOKEN"}, "SWH_DATASET",
		},
		"a templating action": {map[string]string{"SWH_DATABASE": "{{ .Image }}"}, "SWH_DATABASE"},
	} {
		t.Run(name, func(t *testing.T) {
			builder := newDeployBuilder(t, ctx)
			destination := t.TempDir()

			resp, err := builder.Deploy(ctx, deployRequest(destination, tc.values, ephemeral))
			require.NoError(t, err)
			require.Equal(t, builderv0.DeploymentStatus_ERROR, resp.GetState().GetState())
			require.Contains(t, resp.GetState().GetMessage(), tc.want)
			entries, err := os.ReadDir(destination)
			require.NoError(t, err)
			require.Empty(t, entries, "a refused deploy must write nothing")
		})
	}
}

// With no digest recorded there is no image to deploy, and rendering one under
// a floating name is exactly what pinning exists to prevent.
func TestDeployRefusesWhileNoGatewayImageIsPublished(t *testing.T) {
	ctx := context.Background()
	builder := newDeployBuilder(t, ctx)
	gatewayImageLockJSON = []byte(`{"name": "` + registry + `/service-warehouse", "digest": ""}`)
	destination := t.TempDir()

	resp, err := builder.Deploy(ctx, deployRequest(destination, nil, restricted))
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_ERROR, resp.GetState().GetState())
	require.Contains(t, resp.GetState().GetMessage(), "no gateway image has been published")
}

// TestDeployRestrictedBindsTheWorkloadIdentity pins what infra has to grant
// against: in the restricted profile a hosted cell renders, the pod runs under
// its own ServiceAccount, named after the service, in the target namespace, so a
// warehouse IAM member for that Kubernetes principal authorizes exactly this
// workload and nothing else; and the image is the digest the lock records.
func TestDeployRestrictedBindsTheWorkloadIdentity(t *testing.T) {
	ctx := context.Background()
	builder := newDeployBuilder(t, ctx)
	destination := t.TempDir()

	resp, err := builder.Deploy(ctx, deployRequest(destination, map[string]string{
		"SWH_BACKEND": "bigquery", "SWH_DATABASE": "example-cell-platform",
	}, restricted))
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())

	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(destination, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		rel, _ := filepath.Rel(destination, path)
		files[rel] = string(data)
		return err
	}))
	// No Namespace and no Secret in a restricted tree; nothing secret-shaped.
	for rel, body := range files {
		require.NotContains(t, body, "kind: Namespace", rel)
		require.NotContains(t, body, "kind: Secret", rel)
		require.NotContains(t, body, "SWH_CREDENTIALS_FILE", rel)
		require.NotContains(t, body, "SWH_PASSWORD", rel)
		require.NotContains(t, body, "envFrom", rel)
	}

	account, ok := files[filepath.Join("base", "serviceaccount.yaml")]
	require.True(t, ok, "restricted render must carry the workload's ServiceAccount; files: %v", slices.Sorted(func(yield func(string) bool) {
		for name := range files {
			if !yield(name) {
				return
			}
		}
	}))
	var sa struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name        string            `yaml:"name"`
			Namespace   string            `yaml:"namespace"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(account), &sa))
	require.Equal(t, "ServiceAccount", sa.Kind)
	require.Equal(t, "warehouse", sa.Metadata.Name)
	require.Equal(t, "codefly", sa.Metadata.Namespace)
	require.Empty(t, sa.Metadata.Annotations, "a direct workload-identity grant needs no cloud-account annotation")

	body := files[filepath.Join("base", "deployment.yaml")]
	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					ServiceAccountName string `yaml:"serviceAccountName"`
					Containers         []struct {
						Image string `yaml:"image"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(body), &deployment))
	require.Equal(t, "warehouse", deployment.Spec.Template.Spec.ServiceAccountName)
	require.Equal(t, registry+"/service-warehouse@"+digest, deployment.Spec.Template.Spec.Containers[0].Image)
	requireProbeSemantics(t, body)
}

// TestDeployServicePublishesAllocatedPort: core allocates the gRPC endpoint an
// in-cluster port and hands consumers that port, so the Service must publish it
// and forward it to the container's listener.
func TestDeployServicePublishesAllocatedPort(t *testing.T) {
	ctx := context.Background()
	builder := newDeployBuilder(t, ctx)
	builder.GrpcEndpoint = &basev0.Endpoint{Name: "grpc", Module: "module", Service: "warehouse", Api: standards.GRPC}
	destination := t.TempDir()
	req := deployRequest(destination, nil, restricted)
	instance := resources.NewNetworkInstance("warehouse.codefly.svc.cluster.local", 9090)
	instance.Access = resources.NewContainerNetworkAccess()
	req.NetworkMappings = []*basev0.NetworkMapping{
		{Endpoint: builder.GrpcEndpoint, Instances: []*basev0.NetworkInstance{instance}},
		// Another service's endpoint is never published by this Service.
		{
			Endpoint:  &basev0.Endpoint{Name: "grpc", Module: "module", Service: "other", Api: standards.GRPC},
			Instances: []*basev0.NetworkInstance{instance},
		},
	}
	resp, err := builder.Deploy(ctx, req)
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())
	service, err := os.ReadFile(filepath.Join(destination, "base", "service.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(service), "    - name: grpc\n      port: 9090\n      targetPort: 9465\n")
}

// Without a mapping for the endpoint the Service keeps publishing the container
// port.
func TestDeployServiceFallsBackToContainerPort(t *testing.T) {
	ctx := context.Background()
	builder := newDeployBuilder(t, ctx)
	destination := t.TempDir()

	resp, err := builder.Deploy(ctx, deployRequest(destination, nil, ephemeral))
	require.NoError(t, err)
	require.Equal(t, builderv0.DeploymentStatus_SUCCESS, resp.GetState().GetState(), resp.GetState().GetMessage())
	service, err := os.ReadFile(filepath.Join(destination, "base", "service.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(service), "    - name: grpc\n      port: 9465\n      targetPort: 9465\n")
}

// The endpoint a Create scaffolds must be one core's endpoint model accepts, and
// reachable from the modules that depend on this service.
func TestCreateEndpointsDeclaresAnInternalGRPCEndpoint(t *testing.T) {
	builder := newDeployBuilder(t, context.Background())
	require.NoError(t, builder.CreateEndpoints(context.Background()))
	require.Len(t, builder.Endpoints, 1)
	endpoint := builder.Endpoints[0]
	require.Equal(t, standards.GRPC, endpoint.GetApi())
	require.Equal(t, resources.VisibilityInternal, endpoint.GetVisibility())
	require.True(t, strings.HasPrefix(endpoint.GetService(), "warehouse"), endpoint.GetService())
}
