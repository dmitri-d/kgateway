package deployer

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	api "sigs.k8s.io/gateway-api/apis/v1"
)

type HelmValuesGenerator interface {
	GetValues(ctx context.Context, gw *api.Gateway, inputs *Inputs) (map[string]any, error)
}

type ExtraGatewayParameters struct {
	Object    client.Object
	Generator HelmValuesGenerator
}
