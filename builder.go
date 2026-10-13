package main

import (
	"context"
	"embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/codefly-dev/core/agents/communicate"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/sbom"
	v0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/service-warehouse/internal/gatewayimage"
)

type Builder struct {
	*services.DefaultBuilder
	*Service
}

// deploymentTemplateParameters carries the values the Kubernetes templates need
// beyond the resolved configuration. An empty value renders no variable at all,
// leaving the choice to the gateway.
type deploymentTemplateParameters struct {
	Backend  string
	Database string
	Dataset  string
	Location string
	// ServicePort is the port the Kubernetes Service publishes: the in-cluster
	// port core allocated to the gRPC endpoint, the one every consumer is told
	// to dial. It forwards to ContainerPort.
	ServicePort uint32
	// ContainerPort is the port the gateway listens on inside its container,
	// which the template also hands the gateway as SWH_LISTEN.
	ContainerPort uint32
}

// renderedValue is what a configuration value may look like to be written into a
// manifest. The templates place these inside double-quoted YAML scalars, so a
// quote, a newline or a template action in one would not be a value but a way to
// write the rest of the manifest. Every real value (a backend kind, a BigQuery
// project, a dataset, a region) fits; one that does not is bound onto the
// container by name by the CLI instead of rendered here.
var renderedValue = regexp.MustCompile(`^[A-Za-z0-9._:/@-]*$`)

func NewBuilder() *Builder {
	service := NewService()
	return &Builder{
		DefaultBuilder: services.NewDefaultBuilder(service.Builder),
		Service:        service,
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()
	return s.Builder.LoadService(ctx, req, services.BuilderLoad{
		Settings:         s.Settings,
		Requirements:     requirements,
		FactoryTemplates: factoryFS,
		ResolveEndpoints: func(ctx context.Context, endpoints []*v0.Endpoint) error {
			endpoint, err := resolveServingGRPCEndpoint(ctx, endpoints)
			if err != nil {
				return err
			}
			s.GrpcEndpoint = endpoint
			return nil
		},
	})
}

func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	image, err := publishedGatewayImage()
	if err != nil {
		return s.Builder.AuditError(err)
	}
	return s.Builder.AuditContainer(ctx, req, image.FullName())
}

// SBOM answers a source-scope request with the package inventory this agent has
// always produced, and an image-scope request with evidence bound to the digest
// and platform of every image the service ships. A caller that resolved the
// images itself passes them as subjects; an empty subject list asks the agent to
// enumerate its own.
func (s *Builder) SBOM(ctx context.Context, req *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	if req.GetScope() != builderv0.SBOMScope_SBOM_SCOPE_IMAGE {
		image, err := publishedGatewayImage()
		if err != nil {
			return s.Builder.SBOMError(err)
		}
		return s.Builder.SBOMContainer(ctx, image.FullName())
	}
	if subjects := req.GetSubjects(); len(subjects) > 0 {
		return s.Builder.SBOMImages(ctx, subjects)
	}
	subjects, err := s.imageSubjects(ctx)
	if err != nil {
		return s.Builder.SBOMImageError(err)
	}
	return s.Builder.SBOMImages(ctx, subjects)
}

// imageSubjects enumerates the images this service ships, and the way to reach
// them. The gateway is the only one: the service has no init, migration or
// sidecar image, and the warehouse behind it is not run by this service.
//
// Core derives the subjects from the same gateway metadata the release command
// uses, so release evidence describes the images the agent reports.
func (s *Builder) imageSubjects(ctx context.Context) ([]*builderv0.ImageSubject, error) {
	image, overridden, err := effectiveGatewayImage()
	if err != nil {
		return nil, err
	}
	published := sbom.PublishedImage{Service: s.Base.Unique(), Role: gatewayimage.Role, Reference: image.FullName()}
	if !overridden {
		published.Platforms = gatewayimage.Platforms
	}
	return sbom.ImageSubjects(ctx, published, overridden)
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	image, err := publishedGatewayImage()
	if err != nil {
		return s.Builder.DeployError(err)
	}
	s.Base.SetDockerImage(image)

	// Resolve the effective configuration from the deployment request; without
	// this the template only ever sees the zero-value config.
	if err := s.LoadConfiguration(ctx, req.GetConfiguration()); err != nil {
		return s.Builder.DeployError(err)
	}

	if err := s.rejectAnonymousDeploymentConfiguration(req); err != nil {
		return s.Builder.DeployError(err)
	}

	parameters := &deploymentTemplateParameters{
		Backend:       s.conf.backend,
		Database:      s.conf.database,
		Dataset:       s.conf.dataset,
		Location:      s.conf.location,
		ContainerPort: gatewayContainerPort,
	}
	servicePort, err := s.servicePort(ctx, req.GetNetworkMappings())
	if err != nil {
		return s.Builder.DeployError(err)
	}
	parameters.ServicePort = servicePort

	// A backend kind is not validated against a list here: the gateway owns the
	// set of backends it compiles in and refuses an unknown one at startup,
	// naming the registered kinds, and a second list in the agent would drift
	// from it. What this layer owns is the manifest, so it checks that every
	// value is one the manifest can carry.
	for _, value := range []struct{ name, value string }{
		{"SWH_BACKEND", parameters.Backend},
		{"SWH_DATABASE", parameters.Database},
		{"SWH_DATASET", parameters.Dataset},
		{"SWH_LOCATION", parameters.Location},
	} {
		if !renderedValue.MatchString(value.value) {
			return s.Builder.DeployError(fmt.Errorf("%s %q cannot be rendered into the manifest: "+
				"it may hold only letters, digits and . _ : / @ -; bind a value outside that set onto the container "+
				"through the environment's service configuration instead", value.name, value.value))
		}
	}
	// A credentials-file path cannot be honored by this deployment: nothing
	// projects the file into the pod, and the configuration channel carries the
	// path, never its contents. Emitting the variable anyway names a path that
	// does not exist, and the gateway then fails on its first query after
	// passing every probe. Keyless auth is the remedy where the platform has it
	// (a workload identity bound to the pod's ServiceAccount); elsewhere the
	// file has to be mounted by an overlay this agent does not own. Either way,
	// reject the path rather than render a manifest that lies about it.
	if s.conf.credentialsFile != "" {
		return s.Builder.DeployError(fmt.Errorf("SWH_CREDENTIALS_FILE cannot be delivered by this deployment: "+
			"nothing here mounts %s into the pod, so the gateway would name a path that does not exist; "+
			"unset it and bind the workload's Kubernetes ServiceAccount to a cloud identity (workload identity), "+
			"or mount the file from an overlay of your own and patch SWH_CREDENTIALS_FILE onto the container there",
			s.conf.credentialsFile))
	}
	// Deployment credentials are provided by the environment's secret store,
	// never embedded in manifests. Consumers bind the same key to warehouse.token.
	if s.conf.authToken != "" {
		return s.Builder.DeployError(fmt.Errorf("SWH_AUTH_TOKEN must be delivered as a Kubernetes secret reference, not a literal configuration value"))
	}
	ref := req.GetDeployment().GetKubernetes().GetSecretReferences()["SWH_AUTH_TOKEN"]
	if ref.GetName() == "" || ref.GetKey() == "" {
		return s.Builder.DeployError(fmt.Errorf("deployment requires an SWH_AUTH_TOKEN Kubernetes secret reference; bind the same secret key to consumers' warehouse.token configuration"))
	}
	return s.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Parameters:           parameters,
		PodOverlay:           workloadPodOverlay(),
	})
}

// rejectAnonymousDeploymentConfiguration reserves this key for the template's
// literal false. Reject its presence, regardless of value, group or delivery
// mechanism: a reference's value cannot be known at render time. Core v0.19.4
// exposes secret references but no ConfigMap-reference field; raw configuration
// and environment-manager values cover its ConfigMap/Secret data paths.
func (s *Builder) rejectAnonymousDeploymentConfiguration(req *builderv0.DeploymentRequest) error {
	reject := func(key string) error {
		if key == "SWH_ALLOW_ANONYMOUS" || strings.HasSuffix(key, "__SWH_ALLOW_ANONYMOUS") {
			return fmt.Errorf("SWH_ALLOW_ANONYMOUS is only an explicit local gateway opt-in; the key is reserved in deployments, including values and references")
		}
		return nil
	}
	for key := range req.GetDeployment().GetKubernetes().GetSecretReferences() {
		if err := reject(key); err != nil {
			return err
		}
	}
	configurations := append([]*v0.Configuration{req.GetConfiguration()}, req.GetDependenciesConfigurations()...)
	for _, configuration := range configurations {
		for _, info := range configuration.GetInfos() {
			for _, value := range info.GetConfigurationValues() {
				if err := reject(value.GetKey()); err != nil {
					return err
				}
			}
		}
	}
	values, err := s.EnvironmentVariables.All()
	if err != nil {
		return err
	}
	for _, value := range values {
		if err := reject(value.Key); err != nil {
			return err
		}
	}
	return nil
}

// servicePort is the port the rendered Service publishes for the gRPC endpoint:
// the in-cluster port core allocated to it, which is what every consumer is
// given to dial. With no container mapping for this endpoint in the request (a
// render with no network input) it keeps the container port, so that render is
// unchanged.
func (s *Builder) servicePort(ctx context.Context, mappings []*v0.NetworkMapping) (uint32, error) {
	if s.GrpcEndpoint != nil {
		for _, mapping := range mappings {
			endpoint := mapping.GetEndpoint()
			if endpoint.GetApi() != standards.GRPC ||
				endpoint.GetModule() != s.GrpcEndpoint.GetModule() ||
				endpoint.GetService() != s.GrpcEndpoint.GetService() ||
				endpoint.GetName() != s.GrpcEndpoint.GetName() {
				continue
			}
			// An external endpoint is reached through its DNS entry, never this
			// Service, and core gives it no container view.
			if resources.IsExternalEndpoint(endpoint) {
				continue
			}
			instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, mappings, endpoint, resources.NewContainerNetworkAccess())
			if err != nil {
				return 0, err
			}
			return instance.GetPort(), nil
		}
	}
	return gatewayContainerPort, nil
}

// workloadPodOverlay gives the gateway its own Kubernetes ServiceAccount, named
// after the service (core defaults an unnamed account to the service's DNS
// name) and rendered in the namespace the deployment targets. Running under the
// namespace default would make the gateway's cloud identity the one every other
// workload in that namespace shares: keyless warehouses (workload identity on
// GKE, and the AKS/EKS equivalents) grant on the Kubernetes principal, so a
// grant to `default` is a grant to all of them. With a dedicated account the
// warehouse's IAM can name exactly this workload.
//
// The account carries no annotations: a direct workload-identity grant needs
// none. An environment that declares a runtime identity for this service has
// the CLI project it onto this same account (same name), merging annotations.
func workloadPodOverlay() *services.PodTemplateOverlay {
	return &services.PodTemplateOverlay{ServiceAccount: &services.WorkloadServiceAccount{}}
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()
	if s.Settings.Backend == "" {
		s.Settings.Backend = "mem"
	}
	if err := s.Templates(ctx, s.Information, services.WithFactory(factoryFS)); err != nil {
		return s.Builder.CreateError(err)
	}
	if err := s.CreateEndpoints(ctx); err != nil {
		return s.Builder.CreateErrorf(err, "cannot create endpoints")
	}
	return s.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {
	grpc, err := resources.LoadGrpcAPI(ctx, nil)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load grpc api")
	}
	endpoint := s.Base.BaseEndpoint(standards.GRPC)
	// Reachable by the other modules of the workspace that depend on it. Core's
	// reach vocabulary is private, internal and public: the cross-module reach
	// service-object-storage declares as "external" is "internal" here, and
	// "external" is now a location, which this service's endpoint is not.
	endpoint.Visibility = resources.VisibilityInternal
	s.GrpcEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToGrpcAPI(grpc))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create grpc endpoint")
	}
	s.Endpoints = []*v0.Endpoint{s.GrpcEndpoint}
	s.Wool.Debug("created endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(s.Endpoints)))
	return nil
}

func (s *Builder) Communicate(stream builderv0.Builder_CommunicateServer) error {
	asker := communicate.NewQuestionAsker(stream)
	_, err := asker.RunSequence(nil)
	return err
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
