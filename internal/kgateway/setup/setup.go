package setup

import (
	"context"
	"log/slog"
	"net"

	envoycache "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	xdsserver "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
	istiokube "istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/kubetypes"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/admin"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/controller"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/extensions2/common"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/krtcollections"
	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/wellknown"
	"github.com/kgateway-dev/kgateway/v2/pkg/client/clientset/versioned"
	"github.com/kgateway-dev/kgateway/v2/pkg/deployer"
	"github.com/kgateway-dev/kgateway/v2/pkg/logging"
	"github.com/kgateway-dev/kgateway/v2/pkg/metrics"
	sdk "github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk"
	"github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/collections"
	"github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/krtutil"
	"github.com/kgateway-dev/kgateway/v2/pkg/settings"
	"github.com/kgateway-dev/kgateway/v2/pkg/utils/envutils"
)

type Server interface {
	Start(ctx context.Context) error
}

func WithGatewayControllerName(name string) func(*setup) {
	return func(s *setup) {
		s.gatewayControllerName = name
	}
}

func WithGatewayClassName(name string) func(*setup) {
	return func(s *setup) {
		s.gatewayClassName = name
	}
}

func WithWaypointClassName(name string) func(*setup) {
	return func(s *setup) {
		s.waypointClassName = name
	}
}

func WithAgentGatewayClassName(name string) func(*setup) {
	return func(s *setup) {
		s.agentGatewayClassName = name
	}
}

func WithExtraPlugins(extraPlugins func(ctx context.Context, commoncol *common.CommonCollections) []sdk.Plugin) func(*setup) {
	return func(s *setup) {
		s.extraPlugins = extraPlugins
	}
}

func ExtraGatewayParameters(extraGatewayParameters func(cli client.Client, inputs *deployer.Inputs) []deployer.ExtraGatewayParameters) func(*setup) {
	return func(s *setup) {
		s.extraGatewayParameters = extraGatewayParameters
	}
}

func WithRestConfig(rc *rest.Config) func(*setup) {
	return func(s *setup) {
		s.restConfig = rc
	}
}

func WithControllerManagerOptions(opts *ctrl.Options) func(*setup) {
	return func(s *setup) {
		s.ctrlMgrOptions = opts
	}
}

func WithExtraXDSCallbacks(extraXDSCallbacks xdsserver.Callbacks) func(*setup) {
	return func(s *setup) {
		s.extraXDSCallbacks = extraXDSCallbacks
	}
}

// used for tests only to get access to dynamically assigned port number
func WithXDSListener(l net.Listener) func(*setup) {
	return func(s *setup) {
		s.xdsListener = l
	}
}

func WithExtraManagerConfig(mgrConfigFuncs ...func(ctx context.Context, mgr manager.Manager, objectFilter kubetypes.DynamicObjectFilter) error) func(*setup) {
	return func(s *setup) {
		s.extraManagerConfig = mgrConfigFuncs
	}
}

func WithKrtDebugger(dbg *krt.DebugHandler) func(*setup) {
	return func(s *setup) {
		s.krtDebugger = dbg
	}
}

func WithGlobalSettings(settings *settings.Settings) func(*setup) {
	return func(s *setup) {
		s.globalSettings = settings
	}
}

type setup struct {
	gatewayControllerName  string
	gatewayClassName       string
	waypointClassName      string
	agentGatewayClassName  string
	extraPlugins           func(ctx context.Context, commoncol *common.CommonCollections) []sdk.Plugin
	extraGatewayParameters func(cli client.Client, inputs *deployer.Inputs) []deployer.ExtraGatewayParameters
	extraXDSCallbacks      xdsserver.Callbacks
	xdsListener            net.Listener
	restConfig             *rest.Config
	ctrlMgrOptions         *ctrl.Options
	// extra controller manager config, like adding registering additional controllers
	extraManagerConfig []func(ctx context.Context, mgr manager.Manager, objectFilter kubetypes.DynamicObjectFilter) error
	krtDebugger        *krt.DebugHandler
	globalSettings     *settings.Settings
}

var _ Server = &setup{}

func New(opts ...func(*setup)) *setup {
	s := &setup{
		gatewayControllerName: wellknown.DefaultGatewayControllerName,
		gatewayClassName:      wellknown.DefaultGatewayClassName,
		waypointClassName:     wellknown.DefaultWaypointClassName,
		agentGatewayClassName: wellknown.DefaultAgentGatewayClassName,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *setup) Start(ctx context.Context) error {
	// load global settings
	slog.Info("starting kgateway")

	globalSettings := s.globalSettings
	if globalSettings == nil {
		var err error
		globalSettings, err = settings.BuildSettings()
		if err != nil {
			slog.Error("error loading settings from env", "error", err)
		}
	}

	setupLogging(globalSettings.LogLevel)
	slog.Info("global settings loaded", "settings", *globalSettings)

	var restConfig *rest.Config
	if s.restConfig != nil {
		restConfig = s.restConfig
	} else {
		restConfig = ctrl.GetConfigOrDie()
	}

	var mgrOpts *ctrl.Options
	if s.ctrlMgrOptions != nil {
		mgrOpts = s.ctrlMgrOptions
	} else {
		mgrOpts = &ctrl.Options{
			BaseContext:      func() context.Context { return ctx },
			Scheme:           runtime.NewScheme(),
			PprofBindAddress: "127.0.0.1:9099",
			// if you change the port here, also change the port "health" in the helmchart.
			HealthProbeBindAddress: ":9093",
			Metrics: metricsserver.Options{
				BindAddress: ":9092",
			},
		}
	}

	metrics.SetRegistry(globalSettings.EnableBuiltinDefaultMetrics, nil)
	metrics.SetActive(!(mgrOpts.Metrics.BindAddress == "" || mgrOpts.Metrics.BindAddress == "0"))

	mgr, err := ctrl.NewManager(restConfig, *mgrOpts)
	if err != nil {
		slog.Error("unable to start controller manager")
		return err
	}

	if err := controller.AddToScheme(mgr.GetScheme()); err != nil {
		slog.Error("unable to extend scheme")
		return err
	}

	uniqueClientCallbacks, uccBuilder := krtcollections.NewUniquelyConnectedClients(s.extraXDSCallbacks)
	cache, err := startControlPlane(
		ctx, s.xdsListener, globalSettings.XdsServiceBindAddress, globalSettings.XdsServicePort, uniqueClientCallbacks)
	if err != nil {
		return err
	}
	krtDbg := s.krtDebugger
	if krtDbg == nil {
		krtDbg = new(krt.DebugHandler)
	}

	setupOpts := &controller.SetupOpts{
		Cache:          cache,
		KrtDebugger:    krtDbg,
		GlobalSettings: globalSettings,
	}

	istioClient, err := CreateKubeClient(restConfig)
	if err != nil {
		return err
	}

	cli, err := versioned.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	slog.Info("creating krt collections")
	krtOpts := krtutil.NewKrtOptions(ctx.Done(), setupOpts.KrtDebugger)

	commoncol, err := collections.NewCommonCollections(
		ctx,
		krtOpts,
		istioClient,
		cli,
		mgr.GetClient(),
		s.gatewayControllerName,
		*globalSettings,
	)
	if err != nil {
		slog.Error("error creating common collections")
		return err
	}
	// mergedPlugins := s.pluginFactoryWithBuiltin()(ctx, commoncol)
	// commoncol.InitPlugins(ctx, mergedPlugins, *globalSettings)

	for _, mgrCfgFunc := range s.extraManagerConfig {
		err := mgrCfgFunc(ctx, mgr, commoncol.DiscoveryNamespacesFilter)
		if err != nil {
			return err
		}
	}

	BuildKgatewayWithConfig(
		ctx, mgr, s.gatewayControllerName, s.gatewayClassName, s.waypointClassName,
		s.agentGatewayClassName, setupOpts, restConfig, istioClient, commoncol, uccBuilder, s.extraPlugins, s.extraGatewayParameters)

	slog.Info("starting admin server")
	go admin.RunAdminServer(ctx, setupOpts)

	slog.Info("starting manager")
	return mgr.Start(ctx)
}

func startControlPlane(
	ctx context.Context,
	l net.Listener,
	ip string,
	port uint32,
	callbacks xdsserver.Callbacks,
) (envoycache.SnapshotCache, error) {
	addr := net.ParseIP(ip)
	return NewControlPlane(ctx, l, &net.TCPAddr{IP: addr, Port: int(port)}, callbacks)
}

func BuildKgatewayWithConfig(
	ctx context.Context,
	mgr manager.Manager,
	gatewayControllerName string,
	gatewayClassName string,
	waypointClassName string,
	agentGatewayClassName string,
	setupOpts *controller.SetupOpts,
	restConfig *rest.Config,
	kubeClient istiokube.Client,
	commonCollections *collections.CommonCollections,
	uccBuilder krtcollections.UniquelyConnectedClientsBulider,
	extraPlugins func(ctx context.Context, commoncol *common.CommonCollections) []sdk.Plugin,
	extraGatewayParameters func(cli client.Client, inputs *deployer.Inputs) []deployer.ExtraGatewayParameters,
) error {
	slog.Info("creating krt collections")
	krtOpts := krtutil.NewKrtOptions(ctx.Done(), setupOpts.KrtDebugger)

	augmentedPods := krtcollections.NewPodsCollection(kubeClient, krtOpts)
	augmentedPodsForUcc := augmentedPods
	if envutils.IsEnvTruthy("DISABLE_POD_LOCALITY_XDS") {
		augmentedPodsForUcc = nil
	}

	ucc := uccBuilder(ctx, krtOpts, augmentedPodsForUcc)

	slog.Info("initializing controller")
	c, err := controller.NewControllerBuilder(ctx, controller.StartConfig{
		Manager:                  mgr,
		ControllerName:           gatewayControllerName,
		GatewayClassName:         gatewayClassName,
		WaypointGatewayClassName: waypointClassName,
		AgentGatewayClassName:    agentGatewayClassName,
		ExtraPlugins:             extraPlugins,
		ExtraGatewayParameters:   extraGatewayParameters,
		RestConfig:               restConfig,
		SetupOpts:                setupOpts,
		Client:                   kubeClient,
		AugmentedPods:            augmentedPods,
		UniqueClients:            ucc,
		Dev:                      logging.MustGetLevel(logging.DefaultComponent) <= logging.LevelTrace,
		KrtOptions:               krtOpts,
		CommonCollections:        commonCollections,
	})
	if err != nil {
		slog.Error("failed initializing controller: ", "error", err)
		return err
	}

	slog.Info("waiting for cache sync")
	kubeClient.RunAndWait(ctx.Done())

	return c.Build(ctx)
}

// setupLogging configures the global slog logger
func setupLogging(levelStr string) {
	if levelStr == "" {
		return
	}
	level, err := logging.ParseLevel(levelStr)
	if err != nil {
		slog.Error("failed to parse log level, defaulting to info", "error", err)
		return
	}
	// set all loggers to the specified level
	logging.Reset(level)
	// set controller-runtime logger
	controllerLogger := logr.FromSlogHandler(logging.New("controller-runtime").Handler())
	ctrl.SetLogger(controllerLogger)
	// set klog logger
	klogLogger := logr.FromSlogHandler(logging.New("klog").Handler())
	klog.SetLogger(klogLogger)
}

func CreateKubeClient(restConfig *rest.Config) (istiokube.Client, error) {
	restCfg := istiokube.NewClientConfigForRestConfig(restConfig)
	client, err := istiokube.NewClient(restCfg, "")
	if err != nil {
		return nil, err
	}
	istiokube.EnableCrdWatcher(client)
	return client, nil
}
