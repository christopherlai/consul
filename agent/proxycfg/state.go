package proxycfg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/mitchellh/copystructure"
	"github.com/mitchellh/mapstructure"

	"github.com/hashicorp/consul/agent/cache"
	cachetype "github.com/hashicorp/consul/agent/cache-types"
	"github.com/hashicorp/consul/agent/connect"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/logging"
)

type CacheNotifier interface {
	Notify(ctx context.Context, t string, r cache.Request,
		correlationID string, ch chan<- cache.UpdateEvent) error
}

type Health interface {
	Notify(ctx context.Context, req structs.ServiceSpecificRequest, correlationID string, ch chan<- cache.UpdateEvent) error
}

const (
	coalesceTimeout                    = 200 * time.Millisecond
	rootsWatchID                       = "roots"
	leafWatchID                        = "leaf"
	intentionsWatchID                  = "intentions"
	serviceListWatchID                 = "service-list"
	federationStateListGatewaysWatchID = "federation-state-list-mesh-gateways"
	consulServerListWatchID            = "consul-server-list"
	datacentersWatchID                 = "datacenters"
	serviceResolversWatchID            = "service-resolvers"
	gatewayServicesWatchID             = "gateway-services"
	gatewayConfigWatchID               = "gateway-config"
	externalServiceIDPrefix            = "external-service:"
	serviceLeafIDPrefix                = "service-leaf:"
	serviceConfigIDPrefix              = "service-config:"
	serviceResolverIDPrefix            = "service-resolver:"
	serviceIntentionsIDPrefix          = "service-intentions:"
	intentionUpstreamsID               = "intention-upstreams"
	meshConfigEntryID                  = "mesh"
	svcChecksWatchIDPrefix             = cachetype.ServiceHTTPChecksName + ":"
	preparedQueryIDPrefix              = string(structs.UpstreamDestTypePreparedQuery) + ":"
	defaultPreparedQueryPollInterval   = 30 * time.Second
)

type stateConfig struct {
	logger                hclog.Logger
	source                *structs.QuerySource
	cache                 CacheNotifier
	health                Health
	dnsConfig             DNSConfig
	serverSNIFn           ServerSNIFunc
	intentionDefaultAllow bool
}

// state holds all the state needed to maintain the config for a registered
// connect-proxy service. When a proxy registration is changed, the entire state
// is discarded and a new one created.
type state struct {
	logger          hclog.Logger
	serviceInstance serviceInstance
	handler         kindHandler

	// cancel is set by Watch and called by Close to stop the goroutine started
	// in Watch.
	cancel func()

	ch     chan cache.UpdateEvent
	snapCh chan ConfigSnapshot
	reqCh  chan chan *ConfigSnapshot
}

type DNSConfig struct {
	Domain    string
	AltDomain string
}

type ServerSNIFunc func(dc, nodeName string) string

type serviceInstance struct {
	kind            structs.ServiceKind
	service         string
	proxyID         structs.ServiceID
	address         string
	port            int
	meta            map[string]string
	taggedAddresses map[string]structs.ServiceAddress
	proxyCfg        structs.ConnectProxyConfig
	token           string
}

func copyProxyConfig(ns *structs.NodeService) (structs.ConnectProxyConfig, error) {
	if ns == nil {
		return structs.ConnectProxyConfig{}, nil
	}
	// Copy the config map
	proxyCfgRaw, err := copystructure.Copy(ns.Proxy)
	if err != nil {
		return structs.ConnectProxyConfig{}, err
	}
	proxyCfg, ok := proxyCfgRaw.(structs.ConnectProxyConfig)
	if !ok {
		return structs.ConnectProxyConfig{}, errors.New("failed to copy proxy config")
	}

	// we can safely modify these since we just copied them
	for idx := range proxyCfg.Upstreams {
		us := &proxyCfg.Upstreams[idx]
		if us.DestinationType != structs.UpstreamDestTypePreparedQuery && us.DestinationNamespace == "" {
			// default the upstreams target namespace to the namespace of the proxy
			// doing this here prevents needing much more complex logic a bunch of other
			// places and makes tracking these upstreams simpler as we can dedup them
			// with the maps tracking upstream ids being watched.
			proxyCfg.Upstreams[idx].DestinationNamespace = ns.EnterpriseMeta.NamespaceOrDefault()
		}
	}

	return proxyCfg, nil
}

// newState populates the state struct by copying relevant fields from the
// NodeService and Token. We copy so that we can use them in a separate
// goroutine later without reasoning about races with the NodeService passed
// (especially for embedded fields like maps and slices).
//
// The returned state needs its required dependencies to be set before Watch
// can be called.
func newState(ns *structs.NodeService, token string, config stateConfig) (*state, error) {
	// 10 is fairly arbitrary here but allow for the 3 mandatory and a
	// reasonable number of upstream watches to all deliver their initial
	// messages in parallel without blocking the cache.Notify loops. It's not a
	// huge deal if we do for a short period so we don't need to be more
	// conservative to handle larger numbers of upstreams correctly but gives
	// some head room for normal operation to be non-blocking in most typical
	// cases.
	ch := make(chan cache.UpdateEvent, 10)

	s, err := newServiceInstanceFromNodeService(ns, token)
	if err != nil {
		return nil, err
	}

	var handler kindHandler
	h := handlerState{stateConfig: config, serviceInstance: s, ch: ch}

	switch ns.Kind {
	case structs.ServiceKindConnectProxy:
		handler = &handlerConnectProxy{handlerState: h}
	case structs.ServiceKindTerminatingGateway:
		h.stateConfig.logger = config.logger.Named(logging.TerminatingGateway)
		handler = &handlerTerminatingGateway{handlerState: h}
	case structs.ServiceKindMeshGateway:
		h.stateConfig.logger = config.logger.Named(logging.MeshGateway)
		handler = &handlerMeshGateway{handlerState: h}
	case structs.ServiceKindIngressGateway:
		handler = &handlerIngressGateway{handlerState: h}
	default:
		return nil, errors.New("not a connect-proxy, terminating-gateway, mesh-gateway, or ingress-gateway")
	}

	return &state{
		logger:          config.logger.With("proxy", s.proxyID, "kind", s.kind),
		serviceInstance: s,
		handler:         handler,
		ch:              ch,
		snapCh:          make(chan ConfigSnapshot, 1),
		reqCh:           make(chan chan *ConfigSnapshot, 1),
	}, nil
}

func newServiceInstanceFromNodeService(ns *structs.NodeService, token string) (serviceInstance, error) {
	proxyCfg, err := copyProxyConfig(ns)
	if err != nil {
		return serviceInstance{}, err
	}

	taggedAddresses := make(map[string]structs.ServiceAddress)
	for k, v := range ns.TaggedAddresses {
		taggedAddresses[k] = v
	}

	meta := make(map[string]string)
	for k, v := range ns.Meta {
		meta[k] = v
	}

	return serviceInstance{
		kind:            ns.Kind,
		service:         ns.Service,
		proxyID:         ns.CompoundServiceID(),
		address:         ns.Address,
		port:            ns.Port,
		meta:            meta,
		taggedAddresses: taggedAddresses,
		proxyCfg:        proxyCfg,
		token:           token,
	}, nil
}

type kindHandler interface {
	initialize(ctx context.Context) (ConfigSnapshot, error)
	handleUpdate(ctx context.Context, u cache.UpdateEvent, snap *ConfigSnapshot) error
}

// Watch initialized watches on all necessary cache data for the current proxy
// registration state and returns a chan to observe updates to the
// ConfigSnapshot that contains all necessary config state. The chan is closed
// when the state is Closed.
func (s *state) Watch() (<-chan ConfigSnapshot, error) {
	var ctx context.Context
	ctx, s.cancel = context.WithCancel(context.Background())

	snap, err := s.handler.initialize(ctx)
	if err != nil {
		s.cancel()
		return nil, err
	}

	go s.run(ctx, &snap)

	return s.snapCh, nil
}

// Close discards the state and stops any long-running watches.
func (s *state) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

type handlerState struct {
	stateConfig     // TODO: un-embed
	serviceInstance // TODO: un-embed
	ch              chan cache.UpdateEvent
}

type handlerMeshGateway struct {
	handlerState
}

type handlerTerminatingGateway struct {
	handlerState
}

type handlerConnectProxy struct {
	handlerState
}

type handlerIngressGateway struct {
	handlerState
}

func (s *handlerUpstreams) watchMeshGateway(ctx context.Context, dc string, upstreamID string) error {
	return s.cache.Notify(ctx, cachetype.InternalServiceDumpName, &structs.ServiceDumpRequest{
		Datacenter:     dc,
		QueryOptions:   structs.QueryOptions{Token: s.token},
		ServiceKind:    structs.ServiceKindMeshGateway,
		UseServiceKind: true,
		Source:         *s.source,
		EnterpriseMeta: *structs.DefaultEnterpriseMeta(),
	}, "mesh-gateway:"+dc+":"+upstreamID, s.ch)
}

type handlerUpstreams struct {
	handlerState
}

func (s *handlerUpstreams) watchConnectProxyService(ctx context.Context, correlationId string, target *structs.DiscoveryTarget) error {
	return s.stateConfig.cache.Notify(ctx, cachetype.HealthServicesName, &structs.ServiceSpecificRequest{
		Datacenter: target.Datacenter,
		QueryOptions: structs.QueryOptions{
			Token:  s.serviceInstance.token,
			Filter: target.Subset.Filter,
		},
		ServiceName: target.Service,
		Connect:     true,
		// Note that Identifier doesn't type-prefix for service any more as it's
		// the default and makes metrics and other things much cleaner. It's
		// simpler for us if we have the type to make things unambiguous.
		Source:         *s.stateConfig.source,
		EnterpriseMeta: *target.GetEnterpriseMetadata(),
	}, correlationId, s.ch)
}

// initialize sets up the watches needed based on current proxy registration
// state.
func (s *handlerConnectProxy) initialize(ctx context.Context) (ConfigSnapshot, error) {
	snap := newConfigSnapshotFromServiceInstance(s.serviceInstance, s.stateConfig)
	snap.ConnectProxy.DiscoveryChain = make(map[string]*structs.CompiledDiscoveryChain)
	snap.ConnectProxy.WatchedDiscoveryChains = make(map[string]context.CancelFunc)
	snap.ConnectProxy.WatchedUpstreams = make(map[string]map[string]context.CancelFunc)
	snap.ConnectProxy.WatchedUpstreamEndpoints = make(map[string]map[string]structs.CheckServiceNodes)
	snap.ConnectProxy.WatchedGateways = make(map[string]map[string]context.CancelFunc)
	snap.ConnectProxy.WatchedGatewayEndpoints = make(map[string]map[string]structs.CheckServiceNodes)
	snap.ConnectProxy.WatchedServiceChecks = make(map[structs.ServiceID][]structs.CheckType)
	snap.ConnectProxy.PreparedQueryEndpoints = make(map[string]structs.CheckServiceNodes)
	snap.ConnectProxy.UpstreamConfig = make(map[string]*structs.Upstream)
	snap.ConnectProxy.PassthroughUpstreams = make(map[string]ServicePassthroughAddrs)

	// Watch for root changes
	err := s.cache.Notify(ctx, cachetype.ConnectCARootName, &structs.DCSpecificRequest{
		Datacenter:   s.source.Datacenter,
		QueryOptions: structs.QueryOptions{Token: s.token},
		Source:       *s.source,
	}, rootsWatchID, s.ch)
	if err != nil {
		return snap, err
	}

	// Watch the leaf cert
	err = s.cache.Notify(ctx, cachetype.ConnectCALeafName, &cachetype.ConnectCALeafRequest{
		Datacenter:     s.source.Datacenter,
		Token:          s.token,
		Service:        s.proxyCfg.DestinationServiceName,
		EnterpriseMeta: s.proxyID.EnterpriseMeta,
	}, leafWatchID, s.ch)
	if err != nil {
		return snap, err
	}

	// Watch for intention updates
	err = s.cache.Notify(ctx, cachetype.IntentionMatchName, &structs.IntentionQueryRequest{
		Datacenter:   s.source.Datacenter,
		QueryOptions: structs.QueryOptions{Token: s.token},
		Match: &structs.IntentionQueryMatch{
			Type: structs.IntentionMatchDestination,
			Entries: []structs.IntentionMatchEntry{
				{
					Namespace: s.proxyID.NamespaceOrDefault(),
					Name:      s.proxyCfg.DestinationServiceName,
				},
			},
		},
	}, intentionsWatchID, s.ch)
	if err != nil {
		return snap, err
	}

	// Watch for service check updates
	err = s.cache.Notify(ctx, cachetype.ServiceHTTPChecksName, &cachetype.ServiceHTTPChecksRequest{
		ServiceID:      s.proxyCfg.DestinationServiceID,
		EnterpriseMeta: s.proxyID.EnterpriseMeta,
	}, svcChecksWatchIDPrefix+structs.ServiceIDString(s.proxyCfg.DestinationServiceID, &s.proxyID.EnterpriseMeta), s.ch)
	if err != nil {
		return snap, err
	}

	// default the namespace to the namespace of this proxy service
	currentNamespace := s.proxyID.NamespaceOrDefault()

	if s.proxyCfg.Mode == structs.ProxyModeTransparent {
		// When in transparent proxy we will infer upstreams from intentions with this source
		err := s.cache.Notify(ctx, cachetype.IntentionUpstreamsName, &structs.ServiceSpecificRequest{
			Datacenter:     s.source.Datacenter,
			QueryOptions:   structs.QueryOptions{Token: s.token},
			ServiceName:    s.proxyCfg.DestinationServiceName,
			EnterpriseMeta: structs.NewEnterpriseMeta(s.proxyID.NamespaceOrEmpty()),
		}, intentionUpstreamsID, s.ch)
		if err != nil {
			return snap, err
		}

		err = s.cache.Notify(ctx, cachetype.ConfigEntryName, &structs.ConfigEntryQuery{
			Kind:           structs.MeshConfig,
			Name:           structs.MeshConfigMesh,
			Datacenter:     s.source.Datacenter,
			QueryOptions:   structs.QueryOptions{Token: s.token},
			EnterpriseMeta: *structs.DefaultEnterpriseMeta(),
		}, meshConfigEntryID, s.ch)
		if err != nil {
			return snap, err
		}
	}

	// Watch for updates to service endpoints for all upstreams
	for i := range s.proxyCfg.Upstreams {
		u := s.proxyCfg.Upstreams[i]

		// Store defaults keyed under wildcard so they can be applied to centrally configured upstreams
		if u.DestinationName == structs.WildcardSpecifier {
			snap.ConnectProxy.UpstreamConfig[u.DestinationID().String()] = &u
			continue
		}

		// This can be true if the upstream is a synthetic entry populated from centralized upstream config.
		// Watches should not be created for them.
		if u.CentrallyConfigured {
			continue
		}
		snap.ConnectProxy.UpstreamConfig[u.Identifier()] = &u

		dc := s.source.Datacenter
		if u.Datacenter != "" {
			dc = u.Datacenter
		}
		if s.proxyCfg.Mode == structs.ProxyModeTransparent && (dc == "" || dc == s.source.Datacenter) {
			// In transparent proxy mode, watches for upstreams in the local DC are handled by the IntentionUpstreams watch.
			continue
		}

		ns := currentNamespace
		if u.DestinationNamespace != "" {
			ns = u.DestinationNamespace
		}

		cfg, err := parseReducedUpstreamConfig(u.Config)
		if err != nil {
			// Don't hard fail on a config typo, just warn. We'll fall back on
			// the plain discovery chain if there is an error so it's safe to
			// continue.
			s.logger.Warn("failed to parse upstream config",
				"upstream", u.Identifier(),
				"error", err,
			)
		}

		switch u.DestinationType {
		case structs.UpstreamDestTypePreparedQuery:
			err = s.cache.Notify(ctx, cachetype.PreparedQueryName, &structs.PreparedQueryExecuteRequest{
				Datacenter:    dc,
				QueryOptions:  structs.QueryOptions{Token: s.token, MaxAge: defaultPreparedQueryPollInterval},
				QueryIDOrName: u.DestinationName,
				Connect:       true,
				Source:        *s.source,
			}, "upstream:"+u.Identifier(), s.ch)
			if err != nil {
				return snap, err
			}

		case structs.UpstreamDestTypeService:
			fallthrough

		case "": // Treat unset as the default Service type
			err = s.cache.Notify(ctx, cachetype.CompiledDiscoveryChainName, &structs.DiscoveryChainRequest{
				Datacenter:             s.source.Datacenter,
				QueryOptions:           structs.QueryOptions{Token: s.token},
				Name:                   u.DestinationName,
				EvaluateInDatacenter:   dc,
				EvaluateInNamespace:    ns,
				OverrideMeshGateway:    s.proxyCfg.MeshGateway.OverlayWith(u.MeshGateway),
				OverrideProtocol:       cfg.Protocol,
				OverrideConnectTimeout: cfg.ConnectTimeout(),
			}, "discovery-chain:"+u.Identifier(), s.ch)
			if err != nil {
				return snap, fmt.Errorf("failed to watch discovery chain for %s: %v", u.Identifier(), err)
			}

		default:
			return snap, fmt.Errorf("unknown upstream type: %q", u.DestinationType)
		}
	}

	return snap, nil
}

// reducedUpstreamConfig represents the basic opaque config values that are now
// managed with the discovery chain but for backwards compatibility reasons
// should still affect how the proxy is configured.
//
// The full-blown config is agent/xds.UpstreamConfig
type reducedUpstreamConfig struct {
	Protocol         string `mapstructure:"protocol"`
	ConnectTimeoutMs int    `mapstructure:"connect_timeout_ms"`
}

func (c *reducedUpstreamConfig) ConnectTimeout() time.Duration {
	return time.Duration(c.ConnectTimeoutMs) * time.Millisecond
}

func parseReducedUpstreamConfig(m map[string]interface{}) (reducedUpstreamConfig, error) {
	var cfg reducedUpstreamConfig
	err := mapstructure.WeakDecode(m, &cfg)
	return cfg, err
}

// initWatchesTerminatingGateway sets up the initial watches needed based on the terminating-gateway registration
func (s *handlerTerminatingGateway) initialize(ctx context.Context) (ConfigSnapshot, error) {
	snap := newConfigSnapshotFromServiceInstance(s.serviceInstance, s.stateConfig)
	// Watch for root changes
	err := s.cache.Notify(ctx, cachetype.ConnectCARootName, &structs.DCSpecificRequest{
		Datacenter:   s.source.Datacenter,
		QueryOptions: structs.QueryOptions{Token: s.token},
		Source:       *s.source,
	}, rootsWatchID, s.ch)
	if err != nil {
		s.logger.Error("failed to register watch for root changes", "error", err)
		return snap, err
	}

	// Watch for the terminating-gateway's linked services
	err = s.cache.Notify(ctx, cachetype.GatewayServicesName, &structs.ServiceSpecificRequest{
		Datacenter:     s.source.Datacenter,
		QueryOptions:   structs.QueryOptions{Token: s.token},
		ServiceName:    s.service,
		EnterpriseMeta: s.proxyID.EnterpriseMeta,
	}, gatewayServicesWatchID, s.ch)
	if err != nil {
		s.logger.Error("failed to register watch for linked services", "error", err)
		return snap, err
	}

	snap.TerminatingGateway.WatchedServices = make(map[structs.ServiceName]context.CancelFunc)
	snap.TerminatingGateway.WatchedIntentions = make(map[structs.ServiceName]context.CancelFunc)
	snap.TerminatingGateway.Intentions = make(map[structs.ServiceName]structs.Intentions)
	snap.TerminatingGateway.WatchedLeaves = make(map[structs.ServiceName]context.CancelFunc)
	snap.TerminatingGateway.ServiceLeaves = make(map[structs.ServiceName]*structs.IssuedCert)
	snap.TerminatingGateway.WatchedConfigs = make(map[structs.ServiceName]context.CancelFunc)
	snap.TerminatingGateway.ServiceConfigs = make(map[structs.ServiceName]*structs.ServiceConfigResponse)
	snap.TerminatingGateway.WatchedResolvers = make(map[structs.ServiceName]context.CancelFunc)
	snap.TerminatingGateway.ServiceResolvers = make(map[structs.ServiceName]*structs.ServiceResolverConfigEntry)
	snap.TerminatingGateway.ServiceResolversSet = make(map[structs.ServiceName]bool)
	snap.TerminatingGateway.ServiceGroups = make(map[structs.ServiceName]structs.CheckServiceNodes)
	snap.TerminatingGateway.GatewayServices = make(map[structs.ServiceName]structs.GatewayService)
	snap.TerminatingGateway.HostnameServices = make(map[structs.ServiceName]structs.CheckServiceNodes)
	return snap, nil
}

// initWatchesMeshGateway sets up the watches needed based on the current mesh gateway registration
func (s *handlerMeshGateway) initialize(ctx context.Context) (ConfigSnapshot, error) {
	snap := newConfigSnapshotFromServiceInstance(s.serviceInstance, s.stateConfig)
	// Watch for root changes
	err := s.cache.Notify(ctx, cachetype.ConnectCARootName, &structs.DCSpecificRequest{
		Datacenter:   s.source.Datacenter,
		QueryOptions: structs.QueryOptions{Token: s.token},
		Source:       *s.source,
	}, rootsWatchID, s.ch)
	if err != nil {
		return snap, err
	}

	// Watch for all services
	err = s.cache.Notify(ctx, cachetype.CatalogServiceListName, &structs.DCSpecificRequest{
		Datacenter:     s.source.Datacenter,
		QueryOptions:   structs.QueryOptions{Token: s.token},
		Source:         *s.source,
		EnterpriseMeta: *structs.WildcardEnterpriseMeta(),
	}, serviceListWatchID, s.ch)

	if err != nil {
		return snap, err
	}

	if s.meta[structs.MetaWANFederationKey] == "1" {
		// Conveniently we can just use this service meta attribute in one
		// place here to set the machinery in motion and leave the conditional
		// behavior out of the rest of the package.
		err = s.cache.Notify(ctx, cachetype.FederationStateListMeshGatewaysName, &structs.DCSpecificRequest{
			Datacenter:   s.source.Datacenter,
			QueryOptions: structs.QueryOptions{Token: s.token},
			Source:       *s.source,
		}, federationStateListGatewaysWatchID, s.ch)
		if err != nil {
			return snap, err
		}

		err = s.health.Notify(ctx, structs.ServiceSpecificRequest{
			Datacenter:   s.source.Datacenter,
			QueryOptions: structs.QueryOptions{Token: s.token},
			ServiceName:  structs.ConsulServiceName,
		}, consulServerListWatchID, s.ch)
		if err != nil {
			return snap, err
		}
	}

	// Eventually we will have to watch connect enable instances for each service as well as the
	// destination services themselves but those notifications will be setup later. However we
	// cannot setup those watches until we know what the services are. from the service list
	// watch above

	err = s.cache.Notify(ctx, cachetype.CatalogDatacentersName, &structs.DatacentersRequest{
		QueryOptions: structs.QueryOptions{Token: s.token, MaxAge: 30 * time.Second},
	}, datacentersWatchID, s.ch)
	if err != nil {
		return snap, err
	}

	// Once we start getting notified about the datacenters we will setup watches on the
	// gateways within those other datacenters. We cannot do that here because we don't
	// know what they are yet.

	// Watch service-resolvers so we can setup service subset clusters
	err = s.cache.Notify(ctx, cachetype.ConfigEntriesName, &structs.ConfigEntryQuery{
		Datacenter:     s.source.Datacenter,
		QueryOptions:   structs.QueryOptions{Token: s.token},
		Kind:           structs.ServiceResolver,
		EnterpriseMeta: *structs.WildcardEnterpriseMeta(),
	}, serviceResolversWatchID, s.ch)
	if err != nil {
		s.logger.Named(logging.MeshGateway).
			Error("failed to register watch for service-resolver config entries", "error", err)
		return snap, err
	}

	snap.MeshGateway.WatchedServices = make(map[structs.ServiceName]context.CancelFunc)
	snap.MeshGateway.WatchedDatacenters = make(map[string]context.CancelFunc)
	snap.MeshGateway.ServiceGroups = make(map[structs.ServiceName]structs.CheckServiceNodes)
	snap.MeshGateway.GatewayGroups = make(map[string]structs.CheckServiceNodes)
	snap.MeshGateway.ServiceResolvers = make(map[structs.ServiceName]*structs.ServiceResolverConfigEntry)
	snap.MeshGateway.HostnameDatacenters = make(map[string]structs.CheckServiceNodes)
	// there is no need to initialize the map of service resolvers as we
	// fully rebuild it every time we get updates
	return snap, err
}

func (s *handlerIngressGateway) initialize(ctx context.Context) (ConfigSnapshot, error) {
	snap := newConfigSnapshotFromServiceInstance(s.serviceInstance, s.stateConfig)
	// Watch for root changes
	err := s.cache.Notify(ctx, cachetype.ConnectCARootName, &structs.DCSpecificRequest{
		Datacenter:   s.source.Datacenter,
		QueryOptions: structs.QueryOptions{Token: s.token},
		Source:       *s.source,
	}, rootsWatchID, s.ch)
	if err != nil {
		return snap, err
	}

	// Watch this ingress gateway's config entry
	err = s.cache.Notify(ctx, cachetype.ConfigEntryName, &structs.ConfigEntryQuery{
		Kind:           structs.IngressGateway,
		Name:           s.service,
		Datacenter:     s.source.Datacenter,
		QueryOptions:   structs.QueryOptions{Token: s.token},
		EnterpriseMeta: s.proxyID.EnterpriseMeta,
	}, gatewayConfigWatchID, s.ch)
	if err != nil {
		return snap, err
	}

	// Watch the ingress-gateway's list of upstreams
	err = s.cache.Notify(ctx, cachetype.GatewayServicesName, &structs.ServiceSpecificRequest{
		Datacenter:     s.source.Datacenter,
		QueryOptions:   structs.QueryOptions{Token: s.token},
		ServiceName:    s.service,
		EnterpriseMeta: s.proxyID.EnterpriseMeta,
	}, gatewayServicesWatchID, s.ch)
	if err != nil {
		return snap, err
	}

	snap.IngressGateway.WatchedDiscoveryChains = make(map[string]context.CancelFunc)
	snap.IngressGateway.DiscoveryChain = make(map[string]*structs.CompiledDiscoveryChain)
	snap.IngressGateway.WatchedUpstreams = make(map[string]map[string]context.CancelFunc)
	snap.IngressGateway.WatchedUpstreamEndpoints = make(map[string]map[string]structs.CheckServiceNodes)
	snap.IngressGateway.WatchedGateways = make(map[string]map[string]context.CancelFunc)
	snap.IngressGateway.WatchedGatewayEndpoints = make(map[string]map[string]structs.CheckServiceNodes)
	return snap, nil
}

func newConfigSnapshotFromServiceInstance(s serviceInstance, config stateConfig) ConfigSnapshot {
	// TODO: use serviceInstance type in ConfigSnapshot
	return ConfigSnapshot{
		Kind:                  s.kind,
		Service:               s.service,
		ProxyID:               s.proxyID,
		Address:               s.address,
		Port:                  s.port,
		ServiceMeta:           s.meta,
		TaggedAddresses:       s.taggedAddresses,
		Proxy:                 s.proxyCfg,
		Datacenter:            config.source.Datacenter,
		ServerSNIFn:           config.serverSNIFn,
		IntentionDefaultAllow: config.intentionDefaultAllow,
	}
}

func (s *state) run(ctx context.Context, snap *ConfigSnapshot) {
	// Close the channel we return from Watch when we stop so consumers can stop
	// watching and clean up their goroutines. It's important we do this here and
	// not in Close since this routine sends on this chan and so might panic if it
	// gets closed from another goroutine.
	defer close(s.snapCh)

	// This turns out to be really fiddly/painful by just using time.Timer.C
	// directly in the code below since you can't detect when a timer is stopped
	// vs waiting in order to know to reset it. So just use a chan to send
	// ourselves messages.
	sendCh := make(chan struct{})
	var coalesceTimer *time.Timer

	for {
		select {
		case <-ctx.Done():
			return
		case u := <-s.ch:
			s.logger.Trace("A blocking query returned; handling snapshot update")

			if err := s.handler.handleUpdate(ctx, u, snap); err != nil {
				s.logger.Error("Failed to handle update from watch",
					"id", u.CorrelationID, "error", err,
				)
				continue
			}

		case <-sendCh:
			// Make a deep copy of snap so we don't mutate any of the embedded structs
			// etc on future updates.
			snapCopy, err := snap.Clone()
			if err != nil {
				s.logger.Error("Failed to copy config snapshot for proxy", "error", err)
				continue
			}

			select {
			// Try to send
			case s.snapCh <- *snapCopy:
				s.logger.Trace("Delivered new snapshot to proxy config watchers")

				// Allow the next change to trigger a send
				coalesceTimer = nil

				// Skip rest of loop - there is nothing to send since nothing changed on
				// this iteration
				continue

			// Avoid blocking if a snapshot is already buffered in snapCh as this can result in a deadlock.
			// See PR #9689 for more details.
			default:
				s.logger.Trace("Failed to deliver new snapshot to proxy config watchers")

				// Reset the timer to retry later. This is to ensure we attempt to redeliver the updated snapshot shortly.
				if coalesceTimer == nil {
					coalesceTimer = time.AfterFunc(coalesceTimeout, func() {
						sendCh <- struct{}{}
					})
				}

				// Do not reset coalesceTimer since we just queued a timer-based refresh
				continue
			}

		case replyCh := <-s.reqCh:
			s.logger.Trace("A proxy config snapshot was requested")

			if !snap.Valid() {
				// Not valid yet just respond with nil and move on to next task.
				replyCh <- nil

				s.logger.Trace("The proxy's config snapshot is not valid yet")
				continue
			}
			// Make a deep copy of snap so we don't mutate any of the embedded structs
			// etc on future updates.
			snapCopy, err := snap.Clone()
			if err != nil {
				s.logger.Error("Failed to copy config snapshot for proxy", "error", err)
				continue
			}
			replyCh <- snapCopy

			// Skip rest of loop - there is nothing to send since nothing changed on
			// this iteration
			continue
		}

		// Check if snap is complete enough to be a valid config to deliver to a
		// proxy yet.
		if snap.Valid() {
			// Don't send it right away, set a short timer that will wait for updates
			// from any of the other cache values and deliver them all together.
			if coalesceTimer == nil {
				coalesceTimer = time.AfterFunc(coalesceTimeout, func() {
					// This runs in another goroutine so we can't just do the send
					// directly here as access to snap is racy. Instead, signal the main
					// loop above.
					sendCh <- struct{}{}
				})
			}
		}
	}
}

func (s *handlerConnectProxy) handleUpdate(ctx context.Context, u cache.UpdateEvent, snap *ConfigSnapshot) error {
	if u.Err != nil {
		return fmt.Errorf("error filling agent cache: %v", u.Err)
	}

	switch {
	case u.CorrelationID == rootsWatchID:
		roots, ok := u.Result.(*structs.IndexedCARoots)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		snap.Roots = roots
	case u.CorrelationID == intentionsWatchID:
		resp, ok := u.Result.(*structs.IndexedIntentionMatches)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		if len(resp.Matches) > 0 {
			// RPC supports matching multiple services at once but we only ever
			// query with the one service we represent currently so just pick
			// the one result set up.
			snap.ConnectProxy.Intentions = resp.Matches[0]
		}
		snap.ConnectProxy.IntentionsSet = true

	case u.CorrelationID == intentionUpstreamsID:
		resp, ok := u.Result.(*structs.IndexedServiceList)
		if !ok {
			return fmt.Errorf("invalid type for response %T", u.Result)
		}

		seenServices := make(map[string]struct{})
		for _, svc := range resp.Services {
			seenServices[svc.String()] = struct{}{}

			cfgMap := make(map[string]interface{})
			u, ok := snap.ConnectProxy.UpstreamConfig[svc.String()]
			if ok {
				cfgMap = u.Config
			} else {
				// Use the centralized upstream defaults if they exist and there isn't specific configuration for this upstream
				// This is only relevant to upstreams from intentions because for explicit upstreams the defaulting is handled
				// by the ResolveServiceConfig endpoint.
				wildcardSID := structs.NewServiceID(structs.WildcardSpecifier, structs.WildcardEnterpriseMeta())
				defaults, ok := snap.ConnectProxy.UpstreamConfig[wildcardSID.String()]
				if ok {
					u = defaults
					cfgMap = defaults.Config
					snap.ConnectProxy.UpstreamConfig[svc.String()] = defaults
				}
			}

			cfg, err := parseReducedUpstreamConfig(cfgMap)
			if err != nil {
				// Don't hard fail on a config typo, just warn. We'll fall back on
				// the plain discovery chain if there is an error so it's safe to
				// continue.
				s.logger.Warn("failed to parse upstream config",
					"upstream", u.Identifier(),
					"error", err,
				)
			}

			meshGateway := s.proxyCfg.MeshGateway
			if u != nil {
				meshGateway = meshGateway.OverlayWith(u.MeshGateway)
			}
			watchOpts := discoveryChainWatchOpts{
				id:          svc.String(),
				name:        svc.Name,
				namespace:   svc.NamespaceOrDefault(),
				datacenter:  s.source.Datacenter,
				cfg:         cfg,
				meshGateway: meshGateway,
			}
			up := &handlerUpstreams{handlerState: s.handlerState}
			err = up.watchDiscoveryChain(ctx, snap, watchOpts)
			if err != nil {
				return fmt.Errorf("failed to watch discovery chain for %s: %v", svc.String(), err)
			}
		}

		// Clean up data from services that were not in the update
		for sn := range snap.ConnectProxy.WatchedUpstreams {
			if upstream, ok := snap.ConnectProxy.UpstreamConfig[sn]; ok && upstream.Datacenter != "" && upstream.Datacenter != s.source.Datacenter {
				continue
			}
			if _, ok := seenServices[sn]; !ok {
				delete(snap.ConnectProxy.WatchedUpstreams, sn)
			}
		}
		for sn := range snap.ConnectProxy.WatchedUpstreamEndpoints {
			if upstream, ok := snap.ConnectProxy.UpstreamConfig[sn]; ok && upstream.Datacenter != "" && upstream.Datacenter != s.source.Datacenter {
				continue
			}
			if _, ok := seenServices[sn]; !ok {
				delete(snap.ConnectProxy.WatchedUpstreamEndpoints, sn)
			}
		}
		for sn := range snap.ConnectProxy.WatchedGateways {
			if upstream, ok := snap.ConnectProxy.UpstreamConfig[sn]; ok && upstream.Datacenter != "" && upstream.Datacenter != s.source.Datacenter {
				continue
			}
			if _, ok := seenServices[sn]; !ok {
				delete(snap.ConnectProxy.WatchedGateways, sn)
			}
		}
		for sn := range snap.ConnectProxy.WatchedGatewayEndpoints {
			if upstream, ok := snap.ConnectProxy.UpstreamConfig[sn]; ok && upstream.Datacenter != "" && upstream.Datacenter != s.source.Datacenter {
				continue
			}
			if _, ok := seenServices[sn]; !ok {
				delete(snap.ConnectProxy.WatchedGatewayEndpoints, sn)
			}
		}
		for sn, cancelFn := range snap.ConnectProxy.WatchedDiscoveryChains {
			if upstream, ok := snap.ConnectProxy.UpstreamConfig[sn]; ok && upstream.Datacenter != "" && upstream.Datacenter != s.source.Datacenter {
				continue
			}
			if _, ok := seenServices[sn]; !ok {
				cancelFn()
				delete(snap.ConnectProxy.WatchedDiscoveryChains, sn)
				delete(snap.ConnectProxy.DiscoveryChain, sn)
			}
		}

	case strings.HasPrefix(u.CorrelationID, "upstream:"+preparedQueryIDPrefix):
		resp, ok := u.Result.(*structs.PreparedQueryExecuteResponse)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		pq := strings.TrimPrefix(u.CorrelationID, "upstream:")
		snap.ConnectProxy.PreparedQueryEndpoints[pq] = resp.Nodes

	case strings.HasPrefix(u.CorrelationID, svcChecksWatchIDPrefix):
		resp, ok := u.Result.([]structs.CheckType)
		if !ok {
			return fmt.Errorf("invalid type for service checks response: %T, want: []structs.CheckType", u.Result)
		}
		svcID := structs.ServiceIDFromString(strings.TrimPrefix(u.CorrelationID, svcChecksWatchIDPrefix))
		snap.ConnectProxy.WatchedServiceChecks[svcID] = resp

	case u.CorrelationID == meshConfigEntryID:
		resp, ok := u.Result.(*structs.ConfigEntryResponse)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		if resp.Entry != nil {
			meshConf, ok := resp.Entry.(*structs.MeshConfigEntry)
			if !ok {
				return fmt.Errorf("invalid type for config entry: %T", resp.Entry)
			}
			snap.ConnectProxy.MeshConfig = meshConf
		} else {
			snap.ConnectProxy.MeshConfig = nil
		}
		snap.ConnectProxy.MeshConfigSet = true

	default:
		return (*handlerUpstreams)(s).handleUpdateUpstreams(ctx, u, snap)
	}
	return nil
}

func (s *handlerUpstreams) handleUpdateUpstreams(ctx context.Context, u cache.UpdateEvent, snap *ConfigSnapshot) error {
	if u.Err != nil {
		return fmt.Errorf("error filling agent cache: %v", u.Err)
	}

	upstreamsSnapshot := &snap.ConnectProxy.ConfigSnapshotUpstreams
	if snap.Kind == structs.ServiceKindIngressGateway {
		upstreamsSnapshot = &snap.IngressGateway.ConfigSnapshotUpstreams
	}

	switch {
	case u.CorrelationID == leafWatchID:
		leaf, ok := u.Result.(*structs.IssuedCert)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		upstreamsSnapshot.Leaf = leaf

	case strings.HasPrefix(u.CorrelationID, "discovery-chain:"):
		resp, ok := u.Result.(*structs.DiscoveryChainResponse)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		svc := strings.TrimPrefix(u.CorrelationID, "discovery-chain:")
		upstreamsSnapshot.DiscoveryChain[svc] = resp.Chain

		if err := s.resetWatchesFromChain(ctx, svc, resp.Chain, upstreamsSnapshot); err != nil {
			return err
		}

	case strings.HasPrefix(u.CorrelationID, "upstream-target:"):
		resp, ok := u.Result.(*structs.IndexedCheckServiceNodes)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		correlationID := strings.TrimPrefix(u.CorrelationID, "upstream-target:")
		targetID, svc, ok := removeColonPrefix(correlationID)
		if !ok {
			return fmt.Errorf("invalid correlation id %q", u.CorrelationID)
		}

		if _, ok := upstreamsSnapshot.WatchedUpstreamEndpoints[svc]; !ok {
			upstreamsSnapshot.WatchedUpstreamEndpoints[svc] = make(map[string]structs.CheckServiceNodes)
		}
		upstreamsSnapshot.WatchedUpstreamEndpoints[svc][targetID] = resp.Nodes

		var passthroughAddrs map[string]ServicePassthroughAddrs

		for _, node := range resp.Nodes {
			if snap.Proxy.Mode == structs.ProxyModeTransparent && node.Service.Proxy.TransparentProxy.DialedDirectly {
				if passthroughAddrs == nil {
					passthroughAddrs = make(map[string]ServicePassthroughAddrs)
				}

				svc := node.Service.CompoundServiceName()

				// Overwrite the name if it's a connect proxy (as opposed to Connect native).
				// We don't reference the proxy name directly for things like SNI, but rather the name
				// of the destination. The enterprise meta of a proxy will always be the same as that of
				// the destination service, so that remains intact.
				if node.Service.Kind == structs.ServiceKindConnectProxy {
					dst := node.Service.Proxy.DestinationServiceName
					if dst == "" {
						dst = node.Service.Proxy.DestinationServiceID
					}
					svc.Name = dst
				}

				sni := connect.ServiceSNI(
					svc.Name,
					"",
					svc.NamespaceOrDefault(),
					snap.Datacenter,
					snap.Roots.TrustDomain)

				if _, ok := upstreamsSnapshot.PassthroughUpstreams[svc.String()]; !ok {
					upstreamsSnapshot.PassthroughUpstreams[svc.String()] = ServicePassthroughAddrs{
						SNI: sni,

						// Stored in a set because it's possible for these to be duplicated
						// when the upstream-target is targeted by multiple discovery chains.
						Addrs: make(map[string]struct{}),
					}
				}
				addr, _ := node.BestAddress(false)
				upstreamsSnapshot.PassthroughUpstreams[svc.String()].Addrs[addr] = struct{}{}
			}
		}

	case strings.HasPrefix(u.CorrelationID, "mesh-gateway:"):
		resp, ok := u.Result.(*structs.IndexedNodesWithGateways)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		correlationID := strings.TrimPrefix(u.CorrelationID, "mesh-gateway:")
		dc, svc, ok := removeColonPrefix(correlationID)
		if !ok {
			return fmt.Errorf("invalid correlation id %q", u.CorrelationID)
		}
		if _, ok = upstreamsSnapshot.WatchedGatewayEndpoints[svc]; !ok {
			upstreamsSnapshot.WatchedGatewayEndpoints[svc] = make(map[string]structs.CheckServiceNodes)
		}
		upstreamsSnapshot.WatchedGatewayEndpoints[svc][dc] = resp.Nodes
	default:
		return fmt.Errorf("unknown correlation ID: %s", u.CorrelationID)
	}
	return nil
}

func removeColonPrefix(s string) (string, string, bool) {
	idx := strings.Index(s, ":")
	if idx == -1 {
		return "", "", false
	}
	return s[0:idx], s[idx+1:], true
}

func (s *handlerUpstreams) resetWatchesFromChain(
	ctx context.Context,
	id string,
	chain *structs.CompiledDiscoveryChain,
	snap *ConfigSnapshotUpstreams,
) error {
	s.logger.Trace("resetting watches for discovery chain", "id", id)
	if chain == nil {
		return fmt.Errorf("not possible to arrive here with no discovery chain")
	}

	// Initialize relevant sub maps.
	if _, ok := snap.WatchedUpstreams[id]; !ok {
		snap.WatchedUpstreams[id] = make(map[string]context.CancelFunc)
	}
	if _, ok := snap.WatchedUpstreamEndpoints[id]; !ok {
		snap.WatchedUpstreamEndpoints[id] = make(map[string]structs.CheckServiceNodes)
	}
	if _, ok := snap.WatchedGateways[id]; !ok {
		snap.WatchedGateways[id] = make(map[string]context.CancelFunc)
	}
	if _, ok := snap.WatchedGatewayEndpoints[id]; !ok {
		snap.WatchedGatewayEndpoints[id] = make(map[string]structs.CheckServiceNodes)
	}

	// We could invalidate this selectively based on a hash of the relevant
	// resolver information, but for now just reset anything about this
	// upstream when the chain changes in any way.
	//
	// TODO(rb): content hash based add/remove
	for targetID, cancelFn := range snap.WatchedUpstreams[id] {
		s.logger.Trace("stopping watch of target",
			"upstream", id,
			"chain", chain.ServiceName,
			"target", targetID,
		)
		delete(snap.WatchedUpstreams[id], targetID)
		delete(snap.WatchedUpstreamEndpoints[id], targetID)
		cancelFn()
	}

	var (
		watchedChainEndpoints bool
		needGateways          = make(map[string]struct{})
	)

	chainID := chain.ID()
	for _, target := range chain.Targets {
		if target.ID == chainID {
			watchedChainEndpoints = true
		}

		opts := targetWatchOpts{
			upstreamID: id,
			chainID:    target.ID,
			service:    target.Service,
			filter:     target.Subset.Filter,
			datacenter: target.Datacenter,
			entMeta:    target.GetEnterpriseMetadata(),
		}
		err := s.watchUpstreamTarget(ctx, snap, opts)
		if err != nil {
			return fmt.Errorf("failed to watch target %q for upstream %q", target.ID, id)
		}

		// We'll get endpoints from the gateway query, but the health still has
		// to come from the backing service query.
		switch target.MeshGateway.Mode {
		case structs.MeshGatewayModeRemote:
			needGateways[target.Datacenter] = struct{}{}
		case structs.MeshGatewayModeLocal:
			needGateways[s.source.Datacenter] = struct{}{}
		}
	}

	// If the discovery chain's targets do not lead to watching all endpoints
	// for the upstream, then create a separate watch for those too.
	// This is needed in transparent mode because if there is some service A that
	// redirects to service B, the dialing proxy needs to associate A's virtual IP
	// with A's discovery chain.
	//
	// Outside of transparent mode we only watch the chain target, B,
	// since A is a virtual service and traffic will not be sent to it.
	if !watchedChainEndpoints && s.proxyCfg.Mode == structs.ProxyModeTransparent {
		chainEntMeta := structs.NewEnterpriseMeta(chain.Namespace)

		opts := targetWatchOpts{
			upstreamID: id,
			chainID:    chainID,
			service:    chain.ServiceName,
			filter:     "",
			datacenter: chain.Datacenter,
			entMeta:    &chainEntMeta,
		}
		err := s.watchUpstreamTarget(ctx, snap, opts)
		if err != nil {
			return fmt.Errorf("failed to watch target %q for upstream %q", chainID, id)
		}
	}

	for dc := range needGateways {
		if _, ok := snap.WatchedGateways[id][dc]; ok {
			continue
		}

		s.logger.Trace("initializing watch of mesh gateway in datacenter",
			"upstream", id,
			"chain", chain.ServiceName,
			"datacenter", dc,
		)

		ctx, cancel := context.WithCancel(ctx)
		err := s.watchMeshGateway(ctx, dc, id)
		if err != nil {
			cancel()
			return err
		}

		snap.WatchedGateways[id][dc] = cancel
	}

	for dc, cancelFn := range snap.WatchedGateways[id] {
		if _, ok := needGateways[dc]; ok {
			continue
		}
		s.logger.Trace("stopping watch of mesh gateway in datacenter",
			"upstream", id,
			"chain", chain.ServiceName,
			"datacenter", dc,
		)
		delete(snap.WatchedGateways[id], dc)
		delete(snap.WatchedGatewayEndpoints[id], dc)
		cancelFn()
	}

	return nil
}

type targetWatchOpts struct {
	upstreamID string
	chainID    string
	service    string
	filter     string
	datacenter string
	entMeta    *structs.EnterpriseMeta
}

func (s *handlerUpstreams) watchUpstreamTarget(ctx context.Context, snap *ConfigSnapshotUpstreams, opts targetWatchOpts) error {
	s.logger.Trace("initializing watch of target",
		"upstream", opts.upstreamID,
		"chain", opts.service,
		"target", opts.chainID,
	)

	var finalMeta structs.EnterpriseMeta
	finalMeta.Merge(opts.entMeta)

	correlationID := "upstream-target:" + opts.chainID + ":" + opts.upstreamID

	ctx, cancel := context.WithCancel(ctx)
	err := s.health.Notify(ctx, structs.ServiceSpecificRequest{
		Datacenter: opts.datacenter,
		QueryOptions: structs.QueryOptions{
			Token:  s.token,
			Filter: opts.filter,
		},
		ServiceName: opts.service,
		Connect:     true,
		// Note that Identifier doesn't type-prefix for service any more as it's
		// the default and makes metrics and other things much cleaner. It's
		// simpler for us if we have the type to make things unambiguous.
		Source:         *s.source,
		EnterpriseMeta: finalMeta,
	}, correlationID, s.ch)

	if err != nil {
		cancel()
		return err
	}
	snap.WatchedUpstreams[opts.upstreamID][opts.chainID] = cancel

	return nil
}

func (s *handlerTerminatingGateway) handleUpdate(ctx context.Context, u cache.UpdateEvent, snap *ConfigSnapshot) error {
	if u.Err != nil {
		return fmt.Errorf("error filling agent cache: %v", u.Err)
	}
	logger := s.logger

	switch {
	case u.CorrelationID == rootsWatchID:
		roots, ok := u.Result.(*structs.IndexedCARoots)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		snap.Roots = roots

	// Update watches based on the current list of services associated with the terminating-gateway
	case u.CorrelationID == gatewayServicesWatchID:
		services, ok := u.Result.(*structs.IndexedGatewayServices)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		svcMap := make(map[structs.ServiceName]struct{})
		for _, svc := range services.Services {
			// Make sure to add every service to this map, we use it to cancel watches below.
			svcMap[svc.Service] = struct{}{}

			// Store the gateway <-> service mapping for TLS origination
			snap.TerminatingGateway.GatewayServices[svc.Service] = *svc

			// Watch the health endpoint to discover endpoints for the service
			if _, ok := snap.TerminatingGateway.WatchedServices[svc.Service]; !ok {
				ctx, cancel := context.WithCancel(ctx)
				err := s.health.Notify(ctx, structs.ServiceSpecificRequest{
					Datacenter:     s.source.Datacenter,
					QueryOptions:   structs.QueryOptions{Token: s.token},
					ServiceName:    svc.Service.Name,
					EnterpriseMeta: svc.Service.EnterpriseMeta,

					// The gateway acts as the service's proxy, so we do NOT want to discover other proxies
					Connect: false,
				}, externalServiceIDPrefix+svc.Service.String(), s.ch)

				if err != nil {
					logger.Error("failed to register watch for external-service",
						"service", svc.Service.String(),
						"error", err,
					)
					cancel()
					return err
				}
				snap.TerminatingGateway.WatchedServices[svc.Service] = cancel
			}

			// Watch intentions with this service as their destination
			// The gateway will enforce intentions for connections to the service
			if _, ok := snap.TerminatingGateway.WatchedIntentions[svc.Service]; !ok {
				ctx, cancel := context.WithCancel(ctx)
				err := s.cache.Notify(ctx, cachetype.IntentionMatchName, &structs.IntentionQueryRequest{
					Datacenter:   s.source.Datacenter,
					QueryOptions: structs.QueryOptions{Token: s.token},
					Match: &structs.IntentionQueryMatch{
						Type: structs.IntentionMatchDestination,
						Entries: []structs.IntentionMatchEntry{
							{
								Namespace: svc.Service.NamespaceOrDefault(),
								Name:      svc.Service.Name,
							},
						},
					},
				}, serviceIntentionsIDPrefix+svc.Service.String(), s.ch)

				if err != nil {
					logger.Error("failed to register watch for service-intentions",
						"service", svc.Service.String(),
						"error", err,
					)
					cancel()
					return err
				}
				snap.TerminatingGateway.WatchedIntentions[svc.Service] = cancel
			}

			// Watch leaf certificate for the service
			// This cert is used to terminate mTLS connections on the service's behalf
			if _, ok := snap.TerminatingGateway.WatchedLeaves[svc.Service]; !ok {
				ctx, cancel := context.WithCancel(ctx)
				err := s.cache.Notify(ctx, cachetype.ConnectCALeafName, &cachetype.ConnectCALeafRequest{
					Datacenter:     s.source.Datacenter,
					Token:          s.token,
					Service:        svc.Service.Name,
					EnterpriseMeta: svc.Service.EnterpriseMeta,
				}, serviceLeafIDPrefix+svc.Service.String(), s.ch)

				if err != nil {
					logger.Error("failed to register watch for a service-leaf",
						"service", svc.Service.String(),
						"error", err,
					)
					cancel()
					return err
				}
				snap.TerminatingGateway.WatchedLeaves[svc.Service] = cancel
			}

			// Watch service configs for the service.
			// These are used to determine the protocol for the target service.
			if _, ok := snap.TerminatingGateway.WatchedConfigs[svc.Service]; !ok {
				ctx, cancel := context.WithCancel(ctx)
				err := s.cache.Notify(ctx, cachetype.ResolvedServiceConfigName, &structs.ServiceConfigRequest{
					Datacenter:     s.source.Datacenter,
					QueryOptions:   structs.QueryOptions{Token: s.token},
					Name:           svc.Service.Name,
					EnterpriseMeta: svc.Service.EnterpriseMeta,
				}, serviceConfigIDPrefix+svc.Service.String(), s.ch)

				if err != nil {
					logger.Error("failed to register watch for a resolved service config",
						"service", svc.Service.String(),
						"error", err,
					)
					cancel()
					return err
				}
				snap.TerminatingGateway.WatchedConfigs[svc.Service] = cancel
			}

			// Watch service resolvers for the service
			// These are used to create clusters and endpoints for the service subsets
			if _, ok := snap.TerminatingGateway.WatchedResolvers[svc.Service]; !ok {
				ctx, cancel := context.WithCancel(ctx)
				err := s.cache.Notify(ctx, cachetype.ConfigEntriesName, &structs.ConfigEntryQuery{
					Datacenter:     s.source.Datacenter,
					QueryOptions:   structs.QueryOptions{Token: s.token},
					Kind:           structs.ServiceResolver,
					Name:           svc.Service.Name,
					EnterpriseMeta: svc.Service.EnterpriseMeta,
				}, serviceResolverIDPrefix+svc.Service.String(), s.ch)

				if err != nil {
					logger.Error("failed to register watch for a service-resolver",
						"service", svc.Service.String(),
						"error", err,
					)
					cancel()
					return err
				}
				snap.TerminatingGateway.WatchedResolvers[svc.Service] = cancel
			}
		}

		// Delete gateway service mapping for services that were not in the update
		for sn := range snap.TerminatingGateway.GatewayServices {
			if _, ok := svcMap[sn]; !ok {
				delete(snap.TerminatingGateway.GatewayServices, sn)
			}
		}

		// Clean up services with hostname mapping for services that were not in the update
		for sn := range snap.TerminatingGateway.HostnameServices {
			if _, ok := svcMap[sn]; !ok {
				delete(snap.TerminatingGateway.HostnameServices, sn)
			}
		}

		// Cancel service instance watches for services that were not in the update
		for sn, cancelFn := range snap.TerminatingGateway.WatchedServices {
			if _, ok := svcMap[sn]; !ok {
				logger.Debug("canceling watch for service", "service", sn.String())
				delete(snap.TerminatingGateway.WatchedServices, sn)
				delete(snap.TerminatingGateway.ServiceGroups, sn)
				cancelFn()
			}
		}

		// Cancel leaf cert watches for services that were not in the update
		for sn, cancelFn := range snap.TerminatingGateway.WatchedLeaves {
			if _, ok := svcMap[sn]; !ok {
				logger.Debug("canceling watch for leaf cert", "service", sn.String())
				delete(snap.TerminatingGateway.WatchedLeaves, sn)
				delete(snap.TerminatingGateway.ServiceLeaves, sn)
				cancelFn()
			}
		}

		// Cancel service config watches for services that were not in the update
		for sn, cancelFn := range snap.TerminatingGateway.WatchedConfigs {
			if _, ok := svcMap[sn]; !ok {
				logger.Debug("canceling watch for resolved service config", "service", sn.String())
				delete(snap.TerminatingGateway.WatchedConfigs, sn)
				delete(snap.TerminatingGateway.ServiceConfigs, sn)
				cancelFn()
			}
		}

		// Cancel service-resolver watches for services that were not in the update
		for sn, cancelFn := range snap.TerminatingGateway.WatchedResolvers {
			if _, ok := svcMap[sn]; !ok {
				logger.Debug("canceling watch for service-resolver", "service", sn.String())
				delete(snap.TerminatingGateway.WatchedResolvers, sn)
				delete(snap.TerminatingGateway.ServiceResolvers, sn)
				delete(snap.TerminatingGateway.ServiceResolversSet, sn)
				cancelFn()
			}
		}

		// Cancel intention watches for services that were not in the update
		for sn, cancelFn := range snap.TerminatingGateway.WatchedIntentions {
			if _, ok := svcMap[sn]; !ok {
				logger.Debug("canceling watch for intention", "service", sn.String())
				delete(snap.TerminatingGateway.WatchedIntentions, sn)
				delete(snap.TerminatingGateway.Intentions, sn)
				cancelFn()
			}
		}

	case strings.HasPrefix(u.CorrelationID, externalServiceIDPrefix):
		resp, ok := u.Result.(*structs.IndexedCheckServiceNodes)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		sn := structs.ServiceNameFromString(strings.TrimPrefix(u.CorrelationID, externalServiceIDPrefix))
		delete(snap.TerminatingGateway.ServiceGroups, sn)
		delete(snap.TerminatingGateway.HostnameServices, sn)

		if len(resp.Nodes) > 0 {
			snap.TerminatingGateway.ServiceGroups[sn] = resp.Nodes
			snap.TerminatingGateway.HostnameServices[sn] = hostnameEndpoints(
				s.logger, snap.Datacenter, resp.Nodes)
		}

	// Store leaf cert for watched service
	case strings.HasPrefix(u.CorrelationID, serviceLeafIDPrefix):
		leaf, ok := u.Result.(*structs.IssuedCert)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		sn := structs.ServiceNameFromString(strings.TrimPrefix(u.CorrelationID, serviceLeafIDPrefix))
		snap.TerminatingGateway.ServiceLeaves[sn] = leaf

	case strings.HasPrefix(u.CorrelationID, serviceConfigIDPrefix):
		serviceConfig, ok := u.Result.(*structs.ServiceConfigResponse)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		sn := structs.ServiceNameFromString(strings.TrimPrefix(u.CorrelationID, serviceConfigIDPrefix))
		snap.TerminatingGateway.ServiceConfigs[sn] = serviceConfig

	case strings.HasPrefix(u.CorrelationID, serviceResolverIDPrefix):
		configEntries, ok := u.Result.(*structs.IndexedConfigEntries)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		sn := structs.ServiceNameFromString(strings.TrimPrefix(u.CorrelationID, serviceResolverIDPrefix))
		// There should only ever be one entry for a service resolver within a namespace
		if len(configEntries.Entries) == 1 {
			if resolver, ok := configEntries.Entries[0].(*structs.ServiceResolverConfigEntry); ok {
				snap.TerminatingGateway.ServiceResolvers[sn] = resolver
			}
		}
		snap.TerminatingGateway.ServiceResolversSet[sn] = true

	case strings.HasPrefix(u.CorrelationID, serviceIntentionsIDPrefix):
		resp, ok := u.Result.(*structs.IndexedIntentionMatches)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		sn := structs.ServiceNameFromString(strings.TrimPrefix(u.CorrelationID, serviceIntentionsIDPrefix))

		if len(resp.Matches) > 0 {
			// RPC supports matching multiple services at once but we only ever
			// query with the one service we represent currently so just pick
			// the one result set up.
			snap.TerminatingGateway.Intentions[sn] = resp.Matches[0]
		}

	default:
		// do nothing
	}

	return nil
}

func (s *handlerMeshGateway) handleUpdate(ctx context.Context, u cache.UpdateEvent, snap *ConfigSnapshot) error {
	if u.Err != nil {
		return fmt.Errorf("error filling agent cache: %v", u.Err)
	}

	meshLogger := s.logger.Named(logging.MeshGateway)

	switch u.CorrelationID {
	case rootsWatchID:
		roots, ok := u.Result.(*structs.IndexedCARoots)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		snap.Roots = roots
	case federationStateListGatewaysWatchID:
		dcIndexedNodes, ok := u.Result.(*structs.DatacenterIndexedCheckServiceNodes)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		snap.MeshGateway.FedStateGateways = dcIndexedNodes.DatacenterNodes

		for dc, nodes := range dcIndexedNodes.DatacenterNodes {
			snap.MeshGateway.HostnameDatacenters[dc] = hostnameEndpoints(
				s.logger.Named(logging.MeshGateway), snap.Datacenter, nodes)
		}

		for dc := range snap.MeshGateway.HostnameDatacenters {
			if _, ok := dcIndexedNodes.DatacenterNodes[dc]; !ok {
				delete(snap.MeshGateway.HostnameDatacenters, dc)
			}
		}

	case serviceListWatchID:
		services, ok := u.Result.(*structs.IndexedServiceList)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		svcMap := make(map[structs.ServiceName]struct{})
		for _, svc := range services.Services {
			// Make sure to add every service to this map, we use it to cancel
			// watches below.
			svcMap[svc] = struct{}{}

			if _, ok := snap.MeshGateway.WatchedServices[svc]; !ok {
				ctx, cancel := context.WithCancel(ctx)
				err := s.health.Notify(ctx, structs.ServiceSpecificRequest{
					Datacenter:     s.source.Datacenter,
					QueryOptions:   structs.QueryOptions{Token: s.token},
					ServiceName:    svc.Name,
					Connect:        true,
					EnterpriseMeta: svc.EnterpriseMeta,
				}, fmt.Sprintf("connect-service:%s", svc.String()), s.ch)

				if err != nil {
					meshLogger.Error("failed to register watch for connect-service",
						"service", svc.String(),
						"error", err,
					)
					cancel()
					return err
				}
				snap.MeshGateway.WatchedServices[svc] = cancel
			}
		}

		for sid, cancelFn := range snap.MeshGateway.WatchedServices {
			if _, ok := svcMap[sid]; !ok {
				meshLogger.Debug("canceling watch for service", "service", sid.String())
				// TODO (gateways) Should the sid also be deleted from snap.MeshGateway.ServiceGroups?
				//                 Do those endpoints get cleaned up some other way?
				delete(snap.MeshGateway.WatchedServices, sid)
				cancelFn()
			}
		}

		snap.MeshGateway.WatchedServicesSet = true
	case datacentersWatchID:
		datacentersRaw, ok := u.Result.(*[]string)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		if datacentersRaw == nil {
			return fmt.Errorf("invalid response with a nil datacenter list")
		}

		datacenters := *datacentersRaw

		for _, dc := range datacenters {
			if dc == s.source.Datacenter {
				continue
			}

			if _, ok := snap.MeshGateway.WatchedDatacenters[dc]; !ok {
				ctx, cancel := context.WithCancel(ctx)
				err := s.cache.Notify(ctx, cachetype.InternalServiceDumpName, &structs.ServiceDumpRequest{
					Datacenter:     dc,
					QueryOptions:   structs.QueryOptions{Token: s.token},
					ServiceKind:    structs.ServiceKindMeshGateway,
					UseServiceKind: true,
					Source:         *s.source,
					EnterpriseMeta: *structs.DefaultEnterpriseMeta(),
				}, fmt.Sprintf("mesh-gateway:%s", dc), s.ch)

				if err != nil {
					meshLogger.Error("failed to register watch for mesh-gateway",
						"datacenter", dc,
						"error", err,
					)
					cancel()
					return err
				}

				snap.MeshGateway.WatchedDatacenters[dc] = cancel
			}
		}

		for dc, cancelFn := range snap.MeshGateway.WatchedDatacenters {
			found := false
			for _, dcCurrent := range datacenters {
				if dcCurrent == dc {
					found = true
					break
				}
			}

			if !found {
				delete(snap.MeshGateway.WatchedDatacenters, dc)
				cancelFn()
			}
		}
	case serviceResolversWatchID:
		configEntries, ok := u.Result.(*structs.IndexedConfigEntries)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		resolvers := make(map[structs.ServiceName]*structs.ServiceResolverConfigEntry)
		for _, entry := range configEntries.Entries {
			if resolver, ok := entry.(*structs.ServiceResolverConfigEntry); ok {
				resolvers[structs.NewServiceName(resolver.Name, &resolver.EnterpriseMeta)] = resolver
			}
		}
		snap.MeshGateway.ServiceResolvers = resolvers

	case consulServerListWatchID:
		resp, ok := u.Result.(*structs.IndexedCheckServiceNodes)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		// Do some initial sanity checks to avoid doing something dumb.
		for _, csn := range resp.Nodes {
			if csn.Service.Service != structs.ConsulServiceName {
				return fmt.Errorf("expected service name %q but got %q",
					structs.ConsulServiceName, csn.Service.Service)
			}
			if csn.Node.Datacenter != snap.Datacenter {
				return fmt.Errorf("expected datacenter %q but got %q",
					snap.Datacenter, csn.Node.Datacenter)
			}
		}

		snap.MeshGateway.ConsulServers = resp.Nodes

	default:
		switch {
		case strings.HasPrefix(u.CorrelationID, "connect-service:"):
			resp, ok := u.Result.(*structs.IndexedCheckServiceNodes)
			if !ok {
				return fmt.Errorf("invalid type for response: %T", u.Result)
			}

			sn := structs.ServiceNameFromString(strings.TrimPrefix(u.CorrelationID, "connect-service:"))

			if len(resp.Nodes) > 0 {
				snap.MeshGateway.ServiceGroups[sn] = resp.Nodes
			} else if _, ok := snap.MeshGateway.ServiceGroups[sn]; ok {
				delete(snap.MeshGateway.ServiceGroups, sn)
			}
		case strings.HasPrefix(u.CorrelationID, "mesh-gateway:"):
			resp, ok := u.Result.(*structs.IndexedNodesWithGateways)
			if !ok {
				return fmt.Errorf("invalid type for response: %T", u.Result)
			}

			dc := strings.TrimPrefix(u.CorrelationID, "mesh-gateway:")
			delete(snap.MeshGateway.GatewayGroups, dc)
			delete(snap.MeshGateway.HostnameDatacenters, dc)

			if len(resp.Nodes) > 0 {
				snap.MeshGateway.GatewayGroups[dc] = resp.Nodes
				snap.MeshGateway.HostnameDatacenters[dc] = hostnameEndpoints(
					s.logger.Named(logging.MeshGateway), snap.Datacenter, resp.Nodes)
			}
		default:
			// do nothing for now
		}
	}

	return nil
}

func (s *handlerIngressGateway) handleUpdate(ctx context.Context, u cache.UpdateEvent, snap *ConfigSnapshot) error {
	if u.Err != nil {
		return fmt.Errorf("error filling agent cache: %v", u.Err)
	}

	switch {
	case u.CorrelationID == rootsWatchID:
		roots, ok := u.Result.(*structs.IndexedCARoots)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		snap.Roots = roots
	case u.CorrelationID == gatewayConfigWatchID:
		resp, ok := u.Result.(*structs.ConfigEntryResponse)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}
		gatewayConf, ok := resp.Entry.(*structs.IngressGatewayConfigEntry)
		if !ok {
			return fmt.Errorf("invalid type for config entry: %T", resp.Entry)
		}

		snap.IngressGateway.TLSEnabled = gatewayConf.TLS.Enabled
		snap.IngressGateway.TLSSet = true

		if err := s.watchIngressLeafCert(ctx, snap); err != nil {
			return err
		}

	case u.CorrelationID == gatewayServicesWatchID:
		services, ok := u.Result.(*structs.IndexedGatewayServices)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		// Update our upstreams and watches.
		var hosts []string
		watchedSvcs := make(map[string]struct{})
		upstreamsMap := make(map[IngressListenerKey]structs.Upstreams)
		for _, service := range services.Services {
			u := makeUpstream(service)

			watchOpts := discoveryChainWatchOpts{
				id:         u.Identifier(),
				name:       u.DestinationName,
				namespace:  u.DestinationNamespace,
				datacenter: s.source.Datacenter,
			}
			up := &handlerUpstreams{handlerState: s.handlerState}
			err := up.watchDiscoveryChain(ctx, snap, watchOpts)
			if err != nil {
				return fmt.Errorf("failed to watch discovery chain for %s: %v", u.Identifier(), err)
			}
			watchedSvcs[u.Identifier()] = struct{}{}

			hosts = append(hosts, service.Hosts...)

			id := IngressListenerKey{Protocol: service.Protocol, Port: service.Port}
			upstreamsMap[id] = append(upstreamsMap[id], u)
		}

		snap.IngressGateway.Upstreams = upstreamsMap
		snap.IngressGateway.Hosts = hosts
		snap.IngressGateway.HostsSet = true

		for id, cancelFn := range snap.IngressGateway.WatchedDiscoveryChains {
			if _, ok := watchedSvcs[id]; !ok {
				cancelFn()
				delete(snap.IngressGateway.WatchedDiscoveryChains, id)
			}
		}

		if err := s.watchIngressLeafCert(ctx, snap); err != nil {
			return err
		}

	default:
		return (*handlerUpstreams)(s).handleUpdateUpstreams(ctx, u, snap)
	}

	return nil
}

// Note: Ingress gateways are always bound to ports and never unix sockets.
// This means LocalBindPort is the only possibility
func makeUpstream(g *structs.GatewayService) structs.Upstream {
	upstream := structs.Upstream{
		DestinationName:      g.Service.Name,
		DestinationNamespace: g.Service.NamespaceOrDefault(),
		LocalBindPort:        g.Port,
		IngressHosts:         g.Hosts,
		// Pass the protocol that was configured on the ingress listener in order
		// to force that protocol on the Envoy listener.
		Config: map[string]interface{}{
			"protocol": g.Protocol,
		},
	}

	return upstream
}

type discoveryChainWatchOpts struct {
	id          string
	name        string
	namespace   string
	datacenter  string
	cfg         reducedUpstreamConfig
	meshGateway structs.MeshGatewayConfig
}

func (s *handlerUpstreams) watchDiscoveryChain(ctx context.Context, snap *ConfigSnapshot, opts discoveryChainWatchOpts) error {
	if _, ok := snap.ConnectProxy.WatchedDiscoveryChains[opts.id]; ok {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	err := s.cache.Notify(ctx, cachetype.CompiledDiscoveryChainName, &structs.DiscoveryChainRequest{
		Datacenter:             s.source.Datacenter,
		QueryOptions:           structs.QueryOptions{Token: s.token},
		Name:                   opts.name,
		EvaluateInDatacenter:   opts.datacenter,
		EvaluateInNamespace:    opts.namespace,
		OverrideProtocol:       opts.cfg.Protocol,
		OverrideConnectTimeout: opts.cfg.ConnectTimeout(),
		OverrideMeshGateway:    opts.meshGateway,
	}, "discovery-chain:"+opts.id, s.ch)
	if err != nil {
		cancel()
		return err
	}

	switch s.kind {
	case structs.ServiceKindIngressGateway:
		snap.IngressGateway.WatchedDiscoveryChains[opts.id] = cancel
	case structs.ServiceKindConnectProxy:
		snap.ConnectProxy.WatchedDiscoveryChains[opts.id] = cancel
	default:
		cancel()
		return fmt.Errorf("unsupported kind %s", s.kind)
	}

	return nil
}

func (s *handlerIngressGateway) generateIngressDNSSANs(snap *ConfigSnapshot) []string {
	// Update our leaf cert watch with wildcard entries for our DNS domains as well as any
	// configured custom hostnames from the service.
	if !snap.IngressGateway.TLSEnabled {
		return nil
	}

	var dnsNames []string
	namespaces := make(map[string]struct{})
	for _, upstreams := range snap.IngressGateway.Upstreams {
		for _, u := range upstreams {
			namespaces[u.DestinationNamespace] = struct{}{}
		}
	}

	for ns := range namespaces {
		// The default namespace is special cased in DNS resolution, so special
		// case it here.
		if ns == structs.IntentionDefaultNamespace {
			ns = ""
		} else {
			ns = ns + "."
		}

		dnsNames = append(dnsNames, fmt.Sprintf("*.ingress.%s%s", ns, s.dnsConfig.Domain))
		dnsNames = append(dnsNames, fmt.Sprintf("*.ingress.%s%s.%s", ns, s.source.Datacenter, s.dnsConfig.Domain))
		if s.dnsConfig.AltDomain != "" {
			dnsNames = append(dnsNames, fmt.Sprintf("*.ingress.%s%s", ns, s.dnsConfig.AltDomain))
			dnsNames = append(dnsNames, fmt.Sprintf("*.ingress.%s%s.%s", ns, s.source.Datacenter, s.dnsConfig.AltDomain))
		}
	}

	dnsNames = append(dnsNames, snap.IngressGateway.Hosts...)

	return dnsNames
}

func (s *handlerIngressGateway) watchIngressLeafCert(ctx context.Context, snap *ConfigSnapshot) error {
	if !snap.IngressGateway.TLSSet || !snap.IngressGateway.HostsSet {
		return nil
	}

	// Watch the leaf cert
	if snap.IngressGateway.LeafCertWatchCancel != nil {
		snap.IngressGateway.LeafCertWatchCancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	err := s.cache.Notify(ctx, cachetype.ConnectCALeafName, &cachetype.ConnectCALeafRequest{
		Datacenter:     s.source.Datacenter,
		Token:          s.token,
		Service:        s.service,
		DNSSAN:         s.generateIngressDNSSANs(snap),
		EnterpriseMeta: s.proxyID.EnterpriseMeta,
	}, leafWatchID, s.ch)
	if err != nil {
		cancel()
		return err
	}
	snap.IngressGateway.LeafCertWatchCancel = cancel

	return nil
}

// CurrentSnapshot synchronously returns the current ConfigSnapshot if there is
// one ready. If we don't have one yet because not all necessary parts have been
// returned (i.e. both roots and leaf cert), nil is returned.
func (s *state) CurrentSnapshot() *ConfigSnapshot {
	// Make a chan for the response to be sent on
	ch := make(chan *ConfigSnapshot, 1)
	s.reqCh <- ch
	// Wait for the response
	return <-ch
}

// Changed returns whether or not the passed NodeService has had any of the
// fields we care about for config state watching changed or a different token.
func (s *state) Changed(ns *structs.NodeService, token string) bool {
	if ns == nil {
		return true
	}

	proxyCfg, err := copyProxyConfig(ns)
	if err != nil {
		s.logger.Warn("Failed to parse proxy config and will treat the new service as unchanged")
	}

	i := s.serviceInstance
	return ns.Kind != i.kind ||
		i.proxyID != ns.CompoundServiceID() ||
		i.address != ns.Address ||
		i.port != ns.Port ||
		!reflect.DeepEqual(i.proxyCfg, proxyCfg) ||
		i.token != token
}

// hostnameEndpoints returns all CheckServiceNodes that have hostnames instead of IPs as the address.
// Envoy cannot resolve hostnames provided through EDS, so we exclusively use CDS for these clusters.
// If there is a mix of hostnames and addresses we exclusively use the hostnames, since clusters cannot discover
// services with both EDS and DNS.
func hostnameEndpoints(logger hclog.Logger, localDC string, nodes structs.CheckServiceNodes) structs.CheckServiceNodes {
	var (
		hasIP       bool
		hasHostname bool
		resp        structs.CheckServiceNodes
	)

	for _, n := range nodes {
		addr, _ := n.BestAddress(localDC != n.Node.Datacenter)
		if net.ParseIP(addr) != nil {
			hasIP = true
			continue
		}
		hasHostname = true
		resp = append(resp, n)
	}

	if hasHostname && hasIP {
		dc := nodes[0].Node.Datacenter
		sn := nodes[0].Service.CompoundServiceName()

		logger.Warn("service contains instances with mix of hostnames and IP addresses; only hostnames will be passed to Envoy",
			"dc", dc, "service", sn.String())
	}
	return resp
}
