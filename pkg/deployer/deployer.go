package deployer

import common "github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/collections"

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
