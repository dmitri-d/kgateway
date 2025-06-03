package deployer

import (
	"context"

	"github.com/kgateway-dev/kgateway/v2/api/v1alpha1"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/ir"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/wellknown"
	"github.com/rotisserie/eris"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	api "sigs.k8s.io/gateway-api/apis/v1"
)

type HelmValues interface {
	GetValues(ctx context.Context, gw *api.Gateway) (*helmConfig, error)
}

type kGatewayHelmValues struct {
	cli    client.Client
	inputs *Inputs
}

func (h *kGatewayHelmValues) GetValues(ctx context.Context, gw *api.Gateway) (*helmConfig, error) {
	gwParam, err := h.getGatewayParametersForGateway(ctx, gw)
	if err != nil {
		return nil, err
	}
	// If this is a self-managed Gateway, skip gateway auto provisioning
	if gwParam != nil && gwParam.Spec.SelfManaged != nil {
		return nil, nil
	}

	return h.getValues(gw, nil)
}

// getGatewayParametersForGateway returns the merged GatewayParameters object resulting from the default GwParams object and
// the GwParam object specifically associated with the given Gateway (if one exists).
func (h *kGatewayHelmValues) getGatewayParametersForGateway(ctx context.Context, gw *api.Gateway) (*v1alpha1.GatewayParameters, error) {
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
		return nil, getGatewayParametersError(err, gwpNamespace, gwpName, gw.GetNamespace(), gw.GetName(), "Gateway")
	}

	defaultGwp, err := h.getDefaultGatewayParameters(ctx, gw)
	if err != nil {
		return nil, err
	}

	mergedGwp := defaultGwp
	deepMergeGatewayParameters(mergedGwp, gwp)
	return mergedGwp, nil
}

// gets the default GatewayParameters associated with the GatewayClass of the provided Gateway
func (h *kGatewayHelmValues) getDefaultGatewayParameters(ctx context.Context, gw *api.Gateway) (*v1alpha1.GatewayParameters, error) {
	gwc, err := h.getGatewayClassFromGateway(ctx, gw)
	if err != nil {
		return nil, err
	}
	return h.getGatewayParametersForGatewayClass(ctx, gwc)
}

// Gets the GatewayParameters object associated with a given GatewayClass.
func (h *kGatewayHelmValues) getGatewayParametersForGatewayClass(ctx context.Context, gwc *api.GatewayClass) (*v1alpha1.GatewayParameters, error) {
	logger := log.FromContext(ctx)

	defaultGwp := getInMemoryGatewayParameters(gwc.GetName(), h.inputs.ImageInfo)
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
		return nil, getGatewayParametersError(
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
	deepMergeGatewayParameters(mergedGwp, gwp)
	return mergedGwp, nil
}

func (h *kGatewayHelmValues) getGatewayClassFromGateway(ctx context.Context, gw *api.Gateway) (*api.GatewayClass, error) {
	if gw == nil {
		return nil, eris.New("nil Gateway")
	}
	if gw.Spec.GatewayClassName == "" {
		return nil, eris.New("GatewayClassName must not be empty")
	}

	gwc := &api.GatewayClass{}
	err := h.cli.Get(ctx, client.ObjectKey{Name: string(gw.Spec.GatewayClassName)}, gwc)
	if err != nil {
		return nil, eris.Errorf("failed to get GatewayClass for Gateway %s/%s", gw.GetName(), gw.GetNamespace())
	}

	return gwc, nil
}

func (d *kGatewayHelmValues) getValues(gw *api.Gateway, gwParam *v1alpha1.GatewayParameters) (*helmConfig, error) {
	gwKey := ir.ObjectSource{
		Group:     wellknown.GatewayGVK.GroupKind().Group,
		Kind:      wellknown.GatewayGVK.GroupKind().Kind,
		Name:      gw.GetName(),
		Namespace: gw.GetNamespace(),
	}
	irGW := d.inputs.CommonCollections.GatewayIndex.Gateways.GetKey(gwKey.ResourceName())

	// construct the default values
	vals := &helmConfig{
		Gateway: &helmGateway{
			Name:             &gw.Name,
			GatewayName:      &gw.Name,
			GatewayNamespace: &gw.Namespace,
			Ports:            getPortsValues(irGW, gwParam),
			Xds: &helmXds{
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
	gateway.Service = getServiceValues(svcConfig)
	// serviceaccount values
	gateway.ServiceAccount = getServiceAccountValues(svcAccountConfig)
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
	compLogLevelStr, err := ComponentLogLevelsToString(compLogLevels)
	if err != nil {
		return nil, err
	}
	gateway.ComponentLogLevel = &compLogLevelStr

	gateway.Resources = envoyContainerConfig.GetResources()
	gateway.SecurityContext = envoyContainerConfig.GetSecurityContext()
	gateway.Image = getImageValues(envoyContainerConfig.GetImage())

	// istio values
	gateway.Istio = getIstioValues(d.inputs.IstioAutoMtlsEnabled, istioConfig)
	gateway.SdsContainer = getSdsContainerValues(sdsContainerConfig)
	gateway.IstioContainer = getIstioContainerValues(istioContainerConfig)

	// ai values
	gateway.AIExtension, err = getAIExtensionValues(aiExtensionConfig)
	if err != nil {
		return nil, err
	}

	// TODO(npolshak): Currently we are using the same chart for both data planes. Should revisit having a separate chart for agentgateway: https://github.com/kgateway-dev/kgateway/issues/11240
	// agentgateway integration values
	gateway.AgentGateway, err = getAgentGatewayValues(agentGatewayConfig)
	if err != nil {
		return nil, err
	}

	gateway.Stats = getStatsValues(statsConfig)

	return vals, nil
}
