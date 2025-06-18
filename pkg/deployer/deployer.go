package deployer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/wellknown"
)

type ControlPlaneInfo struct {
	XdsHost string
	XdsPort uint32
}

// InferenceExtInfo defines the runtime state of Gateway API inference extensions.
type InferenceExtInfo struct{}

type ImageInfo struct {
	Registry   string
	Tag        string
	PullPolicy string
}

// A Deployer is responsible for deploying proxies and inference extensions.
type Deployer struct {
	controllerName                       string
	chart                                *chart.Chart
	cli                                  client.Client
	helmValues                           HelmValuesGenerator
	helmReleaseNameAndNamespaceGenerator func(obj client.Object) (string, string)
}

// NewDeployer creates a new gateway/inference pool/etc
// TODO [danehans]: Reloading the chart for every reconciliation is inefficient.
// See https://github.com/kgateway-dev/kgateway/issues/10672 for details.
func NewDeployer(controllerName string,
	cli client.Client,
	chart *chart.Chart,
	hvg HelmValuesGenerator,
	helmReleaseNameAndNamespaceGenerator func(obj client.Object) (string, string)) *Deployer {
	return &Deployer{
		controllerName:                       controllerName,
		cli:                                  cli,
		chart:                                chart,
		helmValues:                           hvg,
		helmReleaseNameAndNamespaceGenerator: helmReleaseNameAndNamespaceGenerator,
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

	release, err := install.RunWithContext(installCtx, d.chart, vals)
	if err != nil {
		return nil, fmt.Errorf("failed to render helm chart for %s.%s: %w", ns, name, err)
	}

	objs, err := ConvertYAMLToObjects(d.cli.Scheme(), []byte(release.Manifest))
	if err != nil {
		return nil, fmt.Errorf("failed to convert helm manifest yaml to objects for %s.%s: %w", ns, name, err)
	}
	return objs, nil
}

// GetObjsToDeploy does the following:
//
// * uses HelmValuesGenerator to perform lookup/merging etc to get a final set of helm values
//
// * use those helm values to render the helm chart the deployer was instantiated with into k8s objects
//
// * sets ownerRefs on all generated objects
//
// * returns the objects to be deployed by the caller
func (d *Deployer) GetObjsToDeploy(ctx context.Context, obj client.Object) ([]client.Object, error) {
	logger := log.FromContext(ctx)

	vals, err := d.helmValues.GetValues(ctx, obj)
	if err != nil {
		return nil, fmt.Errorf("failed to get helm values %s.%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	if vals == nil {
		return nil, nil
	}
	logger.V(1).Info("got deployer helm values",
		"name", obj.GetName(),
		"namespace", obj.GetNamespace(),
		"gvk", obj.GetObjectKind().GroupVersionKind().String(),
		"values", vals,
	)

	rname, rns := d.helmReleaseNameAndNamespaceGenerator(obj)
	objs, err := d.Render(rname, rns, vals)
	if err != nil {
		return nil, fmt.Errorf("failed to get objects to deploy %s.%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	// Ensure that each namespaced rendered object has its namespace and ownerRef set.
	for _, renderedObj := range objs {
		gvk := renderedObj.GetObjectKind().GroupVersionKind()
		if IsNamespaced(gvk) {
			if renderedObj.GetNamespace() == "" {
				renderedObj.SetNamespace(obj.GetNamespace())
			}
			// here we rely on client.Object interface to retrieve type metadata instead of using hard-coded values
			// this works for resources retrieved using kube api,
			// but these fields won't be set on newly instantiated objects
			renderedObj.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: obj.GetObjectKind().GroupVersionKind().GroupVersion().String(),
				Kind:       obj.GetObjectKind().GroupVersionKind().Kind,
				Name:       obj.GetName(),
				UID:        obj.GetUID(),
				Controller: ptr.To(true),
			}})
		} else {
			// TODO [danehans]: Not sure why a ns must be set for cluster-scoped objects:
			// "failed to apply object rbac.authorization.k8s.io/v1, Kind=ClusterRoleBinding
			// vllm-llama2-7b-pool-endpoint-picker: Namespace parameter required".
			renderedObj.SetNamespace("")
		}
	}

	return objs, nil
}

func (d *Deployer) DeployObjs(ctx context.Context, objs []client.Object) error {
	logger := log.FromContext(ctx)
	for _, obj := range objs {
		logger.V(1).Info("deploying object", "kind", obj.GetObjectKind(), "namespace", obj.GetNamespace(), "name", obj.GetName())
		if err := d.cli.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(d.controllerName)); err != nil {
			return fmt.Errorf("failed to apply object %s %s: %w", obj.GetObjectKind().GroupVersionKind().String(), obj.GetName(), err)
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
