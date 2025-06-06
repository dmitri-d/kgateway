package deployer

import (
	"context"
	"slices"

	"github.com/rotisserie/eris"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	api "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/kgateway-dev/kgateway/v2/api/v1alpha1"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/ir"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/wellknown"
	"github.com/kgateway-dev/kgateway/v2/pkg/deployer"
)

func NewGatewayParameters(cli client.Client) *GatewayParameters {
	return &GatewayParameters{
		cli:               cli,
		knownGWParameters: []client.Object{&v1alpha1.GatewayParameters{}}, // always include default GatewayParameters
		extraHVGenerators: make(map[schema.GroupKind]deployer.HelmValuesGenerator),
	}
}

type GatewayParameters struct {
	cli               client.Client
	extraHVGenerators map[schema.GroupKind]deployer.HelmValuesGenerator
	knownGWParameters []client.Object
}

type kGatewayParameters struct {
	cli    client.Client
	inputs *deployer.Inputs
}

func (gp *GatewayParameters) WithExtraGatewayParameters(params ...deployer.ExtraGatewayParameters) *GatewayParameters {
	for _, p := range params {
		gp.knownGWParameters = append(gp.knownGWParameters, p.Object)
		group := p.Object.GetObjectKind().GroupVersionKind().Group
		kind := p.Object.GetObjectKind().GroupVersionKind().Kind
		gp.extraHVGenerators[schema.GroupKind{Group: group, Kind: kind}] = p.Generator
	}
	return gp
}

func (gp *GatewayParameters) AllKnownGatewayParameters() []client.Object {
	return slices.Clone(gp.knownGWParameters)
}

func (gp *GatewayParameters) GetValues(ctx context.Context, gw *api.Gateway, inputs *deployer.Inputs) (map[string]any, error) {
	logger := log.FromContext(ctx)

	ref, err := gp.getGatewayParametersRef(ctx, gw)
	if err != nil {
		return nil, err
	}

	if g, ok := gp.extraHVGenerators[ref]; ok {
		return g.GetValues(ctx, gw, inputs)
	}
	logger.V(1).Info("using default GatewayParameters for Gateway",
		"gatewayName", gw.GetName(),
		"gatewayNamespace", gw.GetNamespace(),
	)

	return newKGatewayParameters(gp.cli, inputs).GetValues(ctx, gw)
}

func (gp *GatewayParameters) getGatewayParametersRef(ctx context.Context, gw *api.Gateway) (schema.GroupKind, error) {
	logger := log.FromContext(ctx)

	// attempt to get the GatewayParameters name from the Gateway. If we can't find it,
	// we'll check for the default GWP for the GatewayClass.
	if gw.Spec.Infrastructure == nil || gw.Spec.Infrastructure.ParametersRef == nil {
		logger.V(1).Info("no GatewayParameters found for Gateway, using default",
			"gatewayName", gw.GetName(),
			"gatewayNamespace", gw.GetNamespace(),
		)
		return gp.getDefaultGatewayParametersRef(ctx, gw)
	}

	return schema.GroupKind{
			Group: string(gw.Spec.Infrastructure.ParametersRef.Group),
			Kind:  string(gw.Spec.Infrastructure.ParametersRef.Kind)},
		nil
}

func (gp *GatewayParameters) getDefaultGatewayParametersRef(ctx context.Context, gw *api.Gateway) (schema.GroupKind, error) {
	gwc, err := getGatewayClassFromGateway(ctx, gp.cli, gw)
	if err != nil {
		return schema.GroupKind{}, err
	}

	if gwc.Spec.ParametersRef != nil {
		return schema.GroupKind{
				Group: string(gwc.Spec.ParametersRef.Group),
				Kind:  string(gwc.Spec.ParametersRef.Kind)},
			nil
	}

	return schema.GroupKind{}, nil
}

func newKGatewayParameters(cli client.Client, inputs *deployer.Inputs) *kGatewayParameters {
	return &kGatewayParameters{cli: cli, inputs: inputs}
}

func (h *kGatewayParameters) GetValues(ctx context.Context, gw *api.Gateway) (map[string]any, error) {
	gwParam, err := h.getGatewayParametersForGateway(ctx, gw)
	if err != nil {
		return nil, err
	}
	if gwParam != nil && gwParam.Spec.SelfManaged != nil {
		return nil, nil
	}
	vals, err := h.getValues(gw, gwParam)
	if err != nil {
		return nil, err
	}

	var jsonVals map[string]any
	err = jsonConvert(vals, &jsonVals)
	return jsonVals, err
}

// getGatewayParametersForGateway returns the merged GatewayParameters object resulting from the default GwParams object and
// the GwParam object specifically associated with the given Gateway (if one exists).
func (h *kGatewayParameters) getGatewayParametersForGateway(ctx context.Context, gw *api.Gateway) (*v1alpha1.GatewayParameters, error) {
	logger := log.FromContext(ctx)

	// attempt to get the GatewayParameters name from the Gateway. If we can't find it,
	// we'll check for the default GWP for the GatewayClass.
	if gw.Spec.Infrastructure == nil || gw.Spec.Infrastructure.ParametersRef == nil {
		logger.V(1).Info("no GatewayParameters found for Gateway, using default",
			"gatewayName", gw.GetName(),
			"gatewayNamespace", gw.GetNamespace(),
		)
		return h.getDefaultGatewayParameters(ctx, gw)
	}

	gwpName := gw.Spec.Infrastructure.ParametersRef.Name
	if group := gw.Spec.Infrastructure.ParametersRef.Group; group != v1alpha1.GroupName {
		return nil, eris.Errorf("invalid group %s for GatewayParameters", group)
	}
	if kind := gw.Spec.Infrastructure.ParametersRef.Kind; kind != api.Kind(wellknown.GatewayParametersGVK.Kind) {
		return nil, eris.Errorf("invalid kind %s for GatewayParameters", kind)
	}

	// the GatewayParameters must live in the same namespace as the Gateway
	gwpNamespace := gw.GetNamespace()
	gwp := &v1alpha1.GatewayParameters{}
	err := h.cli.Get(ctx, client.ObjectKey{Namespace: gwpNamespace, Name: gwpName}, gwp)
	if err != nil {
		return nil, deployer.GetGatewayParametersError(err, gwpNamespace, gwpName, gw.GetNamespace(), gw.GetName(), "Gateway")
	}

	defaultGwp, err := h.getDefaultGatewayParameters(ctx, gw)
	if err != nil {
		return nil, err
	}

	mergedGwp := defaultGwp
	deployer.DeepMergeGatewayParameters(mergedGwp, gwp)
	return mergedGwp, nil
}

// gets the default GatewayParameters associated with the GatewayClass of the provided Gateway
func (h *kGatewayParameters) getDefaultGatewayParameters(ctx context.Context, gw *api.Gateway) (*v1alpha1.GatewayParameters, error) {
	gwc, err := getGatewayClassFromGateway(ctx, h.cli, gw)
	if err != nil {
		return nil, err
	}
	return h.getGatewayParametersForGatewayClass(ctx, gwc)
}

// Gets the GatewayParameters object associated with a given GatewayClass.
func (h *kGatewayParameters) getGatewayParametersForGatewayClass(ctx context.Context, gwc *api.GatewayClass) (*v1alpha1.GatewayParameters, error) {
	logger := log.FromContext(ctx)

	defaultGwp := deployer.GetInMemoryGatewayParameters(gwc.GetName(), h.inputs.ImageInfo)
	paramRef := gwc.Spec.ParametersRef
	if paramRef == nil {
		// when there is no parametersRef, just return the defaults
		return defaultGwp, nil
	}

	gwpName := paramRef.Name
	if gwpName == "" {
		err := eris.New("parametersRef.name cannot be empty when parametersRef is specified")
		logger.Error(err,
			"gatewayClassName", gwc.GetName(),
			"gatewayClassNamespace", gwc.GetNamespace(),
		)
		return nil, err
	}

	gwpNamespace := ""
	if paramRef.Namespace != nil {
		gwpNamespace = string(*paramRef.Namespace)
	}

	gwp := &v1alpha1.GatewayParameters{}
	err := h.cli.Get(ctx, client.ObjectKey{Namespace: gwpNamespace, Name: gwpName}, gwp)
	if err != nil {
		return nil, deployer.GetGatewayParametersError(
			err,
			gwpNamespace, gwpName,
			gwc.GetNamespace(), gwc.GetName(),
			"GatewayClass",
		)
	}

	// merge the explicit GatewayParameters with the defaults. this is
	// primarily done to ensure that the image registry and tag are
	// correctly set when they aren't overridden by the GatewayParameters.
	mergedGwp := defaultGwp
	deployer.DeepMergeGatewayParameters(mergedGwp, gwp)
	return mergedGwp, nil
}

func (d *kGatewayParameters) getValues(gw *api.Gateway, gwParam *v1alpha1.GatewayParameters) (*deployer.HelmConfig, error) {
	gwKey := ir.ObjectSource{
		Group:     wellknown.GatewayGVK.GroupKind().Group,
		Kind:      wellknown.GatewayGVK.GroupKind().Kind,
		Name:      gw.GetName(),
		Namespace: gw.GetNamespace(),
	}
	irGW := d.inputs.CommonCollections.GatewayIndex.Gateways.GetKey(gwKey.ResourceName())

	// construct the default values
	vals := &deployer.HelmConfig{
		Gateway: &deployer.HelmGateway{
			Name:             &gw.Name,
			GatewayName:      &gw.Name,
			GatewayNamespace: &gw.Namespace,
			Ports:            deployer.GetPortsValues(irGW, gwParam),
			Xds: &deployer.HelmXds{
				// The xds host/port MUST map to the Service definition for the Control Plane
				// This is the socket address that the Proxy will connect to on startup, to receive xds updates
				Host: &d.inputs.ControlPlane.XdsHost,
				Port: &d.inputs.ControlPlane.XdsPort,
			},
		},
	}

	// if there is no GatewayParameters, return the values as is
	if gwParam == nil {
		return vals, nil
	}

	// extract all the custom values from the GatewayParameters
	// (note: if we add new fields to GatewayParameters, they will
	// need to be plumbed through here as well)

	// Apply the floating user ID if it is set
	if gwParam.Spec.Kube.GetFloatingUserId() != nil && *gwParam.Spec.Kube.GetFloatingUserId() {
		applyFloatingUserId(gwParam.Spec.Kube)
	}

	kubeProxyConfig := gwParam.Spec.Kube
	deployConfig := kubeProxyConfig.GetDeployment()
	podConfig := kubeProxyConfig.GetPodTemplate()
	envoyContainerConfig := kubeProxyConfig.GetEnvoyContainer()
	svcConfig := kubeProxyConfig.GetService()
	svcAccountConfig := kubeProxyConfig.GetServiceAccount()
	istioConfig := kubeProxyConfig.GetIstio()

	sdsContainerConfig := kubeProxyConfig.GetSdsContainer()
	statsConfig := kubeProxyConfig.GetStats()
	istioContainerConfig := istioConfig.GetIstioProxyContainer()
	aiExtensionConfig := kubeProxyConfig.GetAiExtension()
	agentGatewayConfig := kubeProxyConfig.GetAgentGateway()

	gateway := vals.Gateway
	// deployment values
	gateway.ReplicaCount = deployConfig.GetReplicas()

	// service values
	gateway.Service = deployer.GetServiceValues(svcConfig)
	// serviceaccount values
	gateway.ServiceAccount = deployer.GetServiceAccountValues(svcAccountConfig)
	// pod template values
	gateway.ExtraPodAnnotations = podConfig.GetExtraAnnotations()
	gateway.ExtraPodLabels = podConfig.GetExtraLabels()
	gateway.ImagePullSecrets = podConfig.GetImagePullSecrets()
	gateway.PodSecurityContext = podConfig.GetSecurityContext()
	gateway.NodeSelector = podConfig.GetNodeSelector()
	gateway.Affinity = podConfig.GetAffinity()
	gateway.Tolerations = podConfig.GetTolerations()
	gateway.ReadinessProbe = podConfig.GetReadinessProbe()
	gateway.LivenessProbe = podConfig.GetLivenessProbe()
	gateway.GracefulShutdown = podConfig.GetGracefulShutdown()
	gateway.TerminationGracePeriodSeconds = podConfig.GetTerminationGracePeriodSeconds()

	// envoy container values
	logLevel := envoyContainerConfig.GetBootstrap().GetLogLevel()
	compLogLevels := envoyContainerConfig.GetBootstrap().GetComponentLogLevels()
	gateway.LogLevel = logLevel
	compLogLevelStr, err := deployer.ComponentLogLevelsToString(compLogLevels)
	if err != nil {
		return nil, err
	}
	gateway.ComponentLogLevel = &compLogLevelStr

	gateway.Resources = envoyContainerConfig.GetResources()
	gateway.SecurityContext = envoyContainerConfig.GetSecurityContext()
	gateway.Image = deployer.GetImageValues(envoyContainerConfig.GetImage())

	// istio values
	gateway.Istio = deployer.GetIstioValues(d.inputs.IstioAutoMtlsEnabled, istioConfig)
	gateway.SdsContainer = deployer.GetSdsContainerValues(sdsContainerConfig)
	gateway.IstioContainer = deployer.GetIstioContainerValues(istioContainerConfig)

	// ai values
	gateway.AIExtension, err = deployer.GetAIExtensionValues(aiExtensionConfig)
	if err != nil {
		return nil, err
	}

	// TODO(npolshak): Currently we are using the same chart for both data planes. Should revisit having a separate chart for agentgateway: https://github.com/kgateway-dev/kgateway/issues/11240
	// agentgateway integration values
	gateway.AgentGateway, err = deployer.GetAgentGatewayValues(agentGatewayConfig)
	if err != nil {
		return nil, err
	}

	gateway.Stats = deployer.GetStatsValues(statsConfig)

	return vals, nil
}

func getGatewayClassFromGateway(ctx context.Context, cli client.Client, gw *api.Gateway) (*api.GatewayClass, error) {
	if gw == nil {
		return nil, eris.New("nil Gateway")
	}
	if gw.Spec.GatewayClassName == "" {
		return nil, eris.New("GatewayClassName must not be empty")
	}

	gwc := &api.GatewayClass{}
	err := cli.Get(ctx, client.ObjectKey{Name: string(gw.Spec.GatewayClassName)}, gwc)
	if err != nil {
		return nil, eris.Errorf("failed to get GatewayClass for Gateway %s/%s", gw.GetName(), gw.GetNamespace())
	}

	return gwc, nil
}
