package deployer

import "github.com/rotisserie/eris"

var (
	GatewayParametersError    = eris.New("could not retrieve GatewayParameters")
	GetGatewayParametersError = func(err error, gwpNamespace, gwpName, gwNamespace, gwName, resourceType string) error {
		wrapped := eris.Wrap(err, GatewayParametersError.Error())
		return eris.Wrapf(wrapped, "(%s.%s) for %s (%s.%s)",
			gwpNamespace, gwpName, resourceType, gwNamespace, gwName)
	}
	NilDeployerInputsErr = eris.New("nil inputs to NewDeployer")
)
