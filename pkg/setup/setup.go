package setup

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	core "github.com/kgateway-dev/kgateway/v2/internal/kgateway/setup"
	"github.com/kgateway-dev/kgateway/v2/pkg/deployer"
	sdk "github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk"
	common "github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/collections"
)

type Options struct {
	ExtraPlugins           func(ctx context.Context, commoncol *common.CommonCollections) []sdk.Plugin
	ExtraGatewayParameters func(cli client.Client, inputs *deployer.Inputs) []deployer.ExtraGatewayParameters
}

func New(opts Options) core.Server {
	// internal setup already accepted functional-options; we wrap only extras.
	return core.New(core.WithExtraPlugins(opts.ExtraPlugins), core.ExtraGatewayParameters(opts.ExtraGatewayParameters))
}
