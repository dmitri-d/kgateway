package deployer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	infextv1a2 "sigs.k8s.io/gateway-api-inference-extension/api/v1alpha2"
	api "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/wellknown"

	common "github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/collections"
)

type ControlPlaneInfo struct {
	XdsHost string
	XdsPort uint32
}

// InferenceExtInfo defines the runtime state of Gateway API inference extensions.
type InferenceExtInfo struct{}

// Inputs is the set of options used to configure the deployer deployment.
type Inputs struct {
	ControllerName       string
	Dev                  bool
	IstioAutoMtlsEnabled bool
	ControlPlane         ControlPlaneInfo
	InferenceExtension   *InferenceExtInfo
	ImageInfo            *ImageInfo
	CommonCollections    *common.CommonCollections
}

type ImageInfo struct {
	Registry   string
	Tag        string
	PullPolicy string
}

// A Deployer is responsible for deploying proxies and inference extensions.
type Deployer struct {
	chart *chart.Chart
	cli   client.Client

	inputs     *Inputs
	helmValues HelmValuesGenerator
}

// NewDeployer creates a new gateway
// TODO [danehans]: Reloading the chart for every reconciliation is inefficient.
// See https://github.com/kgateway-dev/kgateway/issues/10672 for details.
func NewDeployer(cli client.Client, chart *chart.Chart, inputs *Inputs, hvg HelmValuesGenerator) *Deployer {
	return &Deployer{
		cli:        cli,
		chart:      chart,
		inputs:     inputs,
		helmValues: hvg,
	}
}

func JsonConvert(in *HelmConfig, out interface{}) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (d *Deployer) RenderChartToObjects(ns, name string, vals map[string]any) ([]client.Object, error) {
	objs, err := d.Render(name, ns, vals)
	if err != nil {
		return nil, err
	}

	for _, obj := range objs {
		obj.SetNamespace(ns)
	}

	return objs, nil
}

func (d *Deployer) getInferExtVals(pool *infextv1a2.InferencePool) (*HelmConfig, error) {
	if d.inputs.InferenceExtension == nil {
		return nil, fmt.Errorf("inference extension input not defined for deployer")
	}

	if pool == nil {
		return nil, fmt.Errorf("inference pool is not defined for deployer")
	}

	// construct the default values
	vals := &HelmConfig{
		InferenceExtension: &HelmInferenceExtension{
			EndpointPicker: &HelmEndpointPickerExtension{
				PoolName:      pool.Name,
				PoolNamespace: pool.Namespace,
			},
		},
	}

	return vals, nil
}

// Render relies on a `helm install` to render the Chart with the injected values
// It returns the list of Objects that are rendered, and an optional error if rendering failed,
// or converting the rendered manifests to objects failed.
func (d *Deployer) Render(name, ns string, vals map[string]any) ([]client.Object, error) {
	mem := driver.NewMemory()
	mem.SetNamespace(ns)
	cfg := &action.Configuration{
		Releases: storage.Init(mem),
	}
	install := action.NewInstall(cfg)
	install.Namespace = ns
	install.ReleaseName = name

	// We rely on the Install object in `clientOnly` mode
	// This means that there is no i/o (i.e. no reads/writes to k8s) that would need to be cancelled.
	// This essentially guarantees that this function terminates quickly and doesn't block the rest of the controller.
	install.ClientOnly = true
	installCtx := context.Background()

	chartType := "gateway"
	if d.inputs.InferenceExtension != nil {
		chartType = "inference extension"
	}

	release, err := install.RunWithContext(installCtx, d.chart, vals)
	if err != nil {
		return nil, fmt.Errorf("failed to render helm chart for %s %s.%s: %w", chartType, ns, name, err)
	}

	objs, err := ConvertYAMLToObjects(d.cli.Scheme(), []byte(release.Manifest))
	if err != nil {
		return nil, fmt.Errorf("failed to convert helm manifest yaml to objects for %s %s.%s: %w", chartType, ns, name, err)
	}
	return objs, nil
}

// GetObjsToDeploy does the following:
//
// * performs GatewayParameters lookup/merging etc to get a final set of helm values
//
// * use those helm values to render the internal `kgateway` helm chart into k8s objects
//
// * sets ownerRefs on all generated objects
//
// * returns the objects to be deployed by the caller
func (d *Deployer) GetObjsToDeploy(ctx context.Context, gw *api.Gateway) ([]client.Object, error) {
	logger := log.FromContext(ctx)

	vals, err := d.helmValues.GetValues(ctx, gw, d.inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to get helm values for gateway %s.%s: %w", gw.GetNamespace(), gw.GetName(), err)
	}
	if vals == nil {
		return nil, nil
	}
	logger.V(1).Info("got deployer helm values",
		"gatewayName", gw.GetName(),
		"gatewayNamespace", gw.GetNamespace(),
		"values", vals,
	)

	objs, err := d.RenderChartToObjects(gw.Namespace, gw.Name, vals)
	if err != nil {
		return nil, fmt.Errorf("failed to get objects to deploy for gateway %s.%s: %w", gw.GetNamespace(), gw.GetName(), err)
	}

	// Set owner ref
	for _, obj := range objs {
		obj.SetOwnerReferences([]metav1.OwnerReference{{
			Kind:       gw.Kind,
			APIVersion: gw.APIVersion,
			Controller: ptr.To(true),
			UID:        gw.UID,
			Name:       gw.Name,
		}})
	}

	return objs, nil
}

// GetEndpointPickerObjs renders endpoint picker objects using the configured helm chart.
// It builds Helm values from the given pool and renders objects required by the endpoint picker extension.
func (d *Deployer) GetEndpointPickerObjs(pool *infextv1a2.InferencePool) ([]client.Object, error) {
	// Build the helm values for the inference extension.
	vals, err := d.getInferExtVals(pool)
	if err != nil {
		return nil, err
	}

	// Convert the helm values struct.
	var convertedVals map[string]any
	if err := JsonConvert(vals, &convertedVals); err != nil {
		return nil, fmt.Errorf("failed to convert inference extension helm values: %w", err)
	}

	// Use a unique release name for the endpoint picker child objects.
	releaseName := fmt.Sprintf("%s-endpoint-picker", pool.Name)
	objs, err := d.Render(releaseName, pool.Namespace, convertedVals)
	if err != nil {
		return nil, fmt.Errorf("failed to render inference extension objects: %w", err)
	}

	// Ensure that each namespaced rendered object has its namespace and ownerRef set.
	for _, obj := range objs {
		gvk := obj.GetObjectKind().GroupVersionKind()
		if IsNamespaced(gvk) {
			if obj.GetNamespace() == "" {
				obj.SetNamespace(pool.Namespace)
			}
			obj.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: pool.APIVersion,
				Kind:       pool.Kind,
				Name:       pool.Name,
				UID:        pool.UID,
				Controller: ptr.To(true),
			}})
		} else {
			// TODO [danehans]: Not sure why a ns must be set for cluster-scoped objects:
			// "failed to apply object rbac.authorization.k8s.io/v1, Kind=ClusterRoleBinding
			// vllm-llama2-7b-pool-endpoint-picker: Namespace parameter required".
			obj.SetNamespace("")
		}
	}

	return objs, nil
}

func (d *Deployer) DeployObjs(ctx context.Context, objs []client.Object) error {
	logger := log.FromContext(ctx)
	for _, obj := range objs {
		logger.V(1).Info("deploying object", "kind", obj.GetObjectKind(), "namespace", obj.GetNamespace(), "name", obj.GetName())
		if err := d.cli.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(d.inputs.ControllerName)); err != nil {
			return fmt.Errorf("failed to apply object %s %s: %w", obj.GetObjectKind().GroupVersionKind().String(), obj.GetName(), err)
		}
	}
	return nil
}

// EnsureFinalizer adds the InferencePool finalizer to the given pool if it’s not already present.
// The deployer requires InferencePools to be finalized to remove cluster-scoped resources.
// This can be removed if the endpoint picker no longer requires cluster-scoped resources.
// See: https://github.com/kubernetes-sigs/gateway-api-inference-extension/issues/224 for details.
func (d *Deployer) EnsureFinalizer(ctx context.Context, pool *infextv1a2.InferencePool) error {
	if slices.Contains(pool.Finalizers, wellknown.InferencePoolFinalizer) {
		return nil
	}
	pool.Finalizers = append(pool.Finalizers, wellknown.InferencePoolFinalizer)
	return d.cli.Update(ctx, pool)
}

// CleanupClusterScopedResources deletes the ClusterRoleBinding for the given pool.
// TODO [danehans]: EPP should use role and rolebinding RBAC: https://github.com/kubernetes-sigs/gateway-api-inference-extension/issues/224
func (d *Deployer) CleanupClusterScopedResources(ctx context.Context, pool *infextv1a2.InferencePool) error {
	// The same release name as in the Helm template.
	releaseName := fmt.Sprintf("%s-endpoint-picker", pool.Name)

	// Delete the ClusterRoleBinding.
	var crb rbacv1.ClusterRoleBinding
	if err := d.cli.Get(ctx, client.ObjectKey{Name: releaseName}, &crb); err == nil {
		if err := d.cli.Delete(ctx, &crb); err != nil {
			return fmt.Errorf("failed to delete ClusterRoleBinding %s: %w", releaseName, err)
		}
	}

	return nil
}

// IsNamespaced returns true if the resource is namespaced.
func IsNamespaced(gvk schema.GroupVersionKind) bool {
	return gvk != wellknown.ClusterRoleBindingGVK
}

func ConvertYAMLToObjects(scheme *runtime.Scheme, yamlData []byte) ([]client.Object, error) {
	var objs []client.Object

	// Split the YAML manifest into separate documents
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(yamlData), 4096)
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		// try to translate to real objects, so they are easier to query later
		gvk := obj.GetObjectKind().GroupVersionKind()
		if realObj, err := scheme.New(gvk); err == nil {
			if realObj, ok := realObj.(client.Object); ok {
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, realObj); err == nil {
					objs = append(objs, realObj)
					continue
				}
			}
		} else if len(obj.Object) == 0 {
			// This can happen with an "empty" document
			continue
		}

		objs = append(objs, &obj)
	}

	return objs, nil
}
