package consul

import (
	"crypto/x509"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/command/connect/proxy"
)

const (
	defaultDownstreamBindAddr = "0.0.0.0"
	defaultUpstreamBindAddr   = "127.0.0.1"

	errorWaitTime             = 5 * time.Second
	preparedQueryPollInterval = 30 * time.Second
)

type upstream struct {
	LocalBindAddress string
	LocalBindPort    int
	Name             string
	Datacenter       string
	Protocol         string
	Nodes            []*api.ServiceEntry

	done bool
}

type downstream struct {
	LocalBindAddress  string
	LocalBindPort     int
	Protocol          string
	TargetAddress     string
	TargetPort        int
	EnableForwardFor  bool
	AppNameHeaderName string
}

type certLeaf struct {
	Cert []byte
	Key  []byte

	done bool
}

type Watcher struct {
	service     string
	serviceName string
	consul      *api.Client
	token       string
	C           chan Config

	lock  sync.Mutex
	ready sync.WaitGroup

	upstreams  map[string]*upstream
	downstream downstream
	certCAs    [][]byte
	certCAPool *x509.CertPool
	leaf       *certLeaf

	update chan struct{}
	log    Logger
}

// New builds a new watcher
func New(service string, consul *api.Client, log Logger) *Watcher {
	return &Watcher{
		service: service,
		consul:  consul,

		C:         make(chan Config),
		upstreams: make(map[string]*upstream),
		update:    make(chan struct{}, 1),
		log:       log,
	}
}

func (w *Watcher) Run() error {
	proxyID, err := proxy.LookupProxyIDForSidecar(w.consul, w.service)
	if err != nil {
		return err
	}

	svc, _, err := w.consul.Agent().Service(w.service, &api.QueryOptions{})
	if err != nil {
		return err
	}

	w.serviceName = svc.Service

	w.ready.Add(4)

	go w.watchCA()
	go w.watchLeaf()
	go w.watchService(proxyID, w.handleProxyChange)
	go w.watchService(w.service, func(first bool, srv *api.AgentService) {
		w.downstream.TargetPort = srv.Port
		if first {
			w.ready.Done()
		}
	})

	w.ready.Wait()

	for range w.update {
		w.C <- w.genCfg()
	}

	return nil
}

func (w *Watcher) handleProxyChange(first bool, srv *api.AgentService) {
	w.downstream.LocalBindAddress = defaultDownstreamBindAddr
	w.downstream.LocalBindPort = srv.Port
	w.downstream.TargetAddress = defaultUpstreamBindAddr

	if srv.Proxy != nil && srv.Proxy.Config != nil {
		if c, ok := srv.Proxy.Config["protocol"].(string); ok {
			w.downstream.Protocol = c
		}
		if b, ok := srv.Proxy.Config["bind_address"].(string); ok {
			w.downstream.LocalBindAddress = b
		}
		if a, ok := srv.Proxy.Config["local_service_address"].(string); ok {
			w.downstream.TargetAddress = a
		}
		if f, ok := srv.Proxy.Config["enable_forwardfor"].(bool); ok {
			w.downstream.EnableForwardFor = f
		}
		if a, ok := srv.Proxy.Config["appname_header"].(string); ok {
			w.downstream.AppNameHeaderName = a
		}
	}

	keep := make(map[string]bool)

	if srv.Proxy != nil {
		for _, up := range srv.Proxy.Upstreams {
			name := fmt.Sprintf("%s_%s", up.DestinationType, up.DestinationName)
			keep[name] = true
			w.lock.Lock()
			_, ok := w.upstreams[name]
			w.lock.Unlock()
			if !ok {
				switch up.DestinationType {
				case api.UpstreamDestTypePreparedQuery:
					w.startUpstreamPreparedQuery(up, name)
				default:
					w.startUpstreamService(up, name)
				}
			}
		}
	}

	for name := range w.upstreams {
		if !keep[name] {
			w.removeUpstream(name)
		}
	}

	if first {
		w.ready.Done()
	}
}

func (w *Watcher) startUpstreamService(up api.Upstream, name string) {
	w.log.Infof("consul: watching upstream for service %s", up.DestinationName)

	u := &upstream{
		LocalBindAddress: up.LocalBindAddress,
		LocalBindPort:    up.LocalBindPort,
		Name:             name,
		Datacenter:       up.Datacenter,
	}

	if p, ok := up.Config["protocol"].(string); ok {
		u.Protocol = p
	}

	w.lock.Lock()
	w.upstreams[name] = u
	w.lock.Unlock()

	go func() {
		index := uint64(0)
		for {
			if u.done {
				return
			}
			nodes, meta, err := w.consul.Health().Connect(up.DestinationName, "", true, &api.QueryOptions{
				Datacenter: up.Datacenter,
				WaitTime:   10 * time.Minute,
				WaitIndex:  index,
			})
			if err != nil {
				w.log.Errorf("consul: error fetching service definition for service %s: %s", up.DestinationName, err)
				time.Sleep(errorWaitTime)
				index = 0
				continue
			}
			changed := index != meta.LastIndex
			index = meta.LastIndex

			if changed {
				w.lock.Lock()
				u.Nodes = nodes
				w.lock.Unlock()
				w.notifyChanged()
			}
		}
	}()
}

func (w *Watcher) startUpstreamPreparedQuery(up api.Upstream, name string) {
	w.log.Infof("consul: watching upstream for prepared_query %s", up.DestinationName)

	u := &upstream{
		LocalBindAddress: up.LocalBindAddress,
		LocalBindPort:    up.LocalBindPort,
		Name:             name,
		Datacenter:       up.Datacenter,
	}

	if p, ok := up.Config["protocol"].(string); ok {
		u.Protocol = p
	}

	interval := preparedQueryPollInterval
	if p, ok := up.Config["poll_interval"].(string); ok {
		dur, err := time.ParseDuration(p)
		if err != nil {
			w.log.Errorf(
				"consul: upstream %s %s: invalid poll interval %s: %s",
				up.DestinationType,
				up.DestinationName,
				p,
				err,
			)
			return
		}
		interval = dur
	}

	w.lock.Lock()
	w.upstreams[name] = u
	w.lock.Unlock()

	go func() {
		var last []*api.ServiceEntry
		for {
			if u.done {
				return
			}
			nodes, _, err := w.consul.PreparedQuery().Execute(up.DestinationName, &api.QueryOptions{
				Connect:    true,
				Datacenter: up.Datacenter,
				WaitTime:   10 * time.Minute,
			})
			if err != nil {
				w.log.Errorf("consul: error fetching service definition for service %s: %s", up.DestinationName, err)
				time.Sleep(errorWaitTime)
				continue
			}

			nodesP := []*api.ServiceEntry{}
			for i := range nodes.Nodes {
				nodesP = append(nodesP, &nodes.Nodes[i])
			}

			if !reflect.DeepEqual(last, nodesP) {
				w.lock.Lock()
				u.Nodes = nodesP
				w.lock.Unlock()
				w.notifyChanged()
				last = nodesP
			}

			time.Sleep(interval)
		}
	}()
}

func (w *Watcher) removeUpstream(name string) {
	w.log.Infof("consul: removing upstream for service %s", name)

	w.lock.Lock()
	w.upstreams[name].done = true
	delete(w.upstreams, name)
	w.lock.Unlock()
}

func (w *Watcher) watchLeaf() {
	w.log.Debugf("consul: watching leaf cert for %s", w.serviceName)

	var lastIndex uint64
	first := true
	for {
		cert, meta, err := w.consul.Agent().ConnectCALeaf(w.serviceName, &api.QueryOptions{
			WaitTime:  10 * time.Minute,
			WaitIndex: lastIndex,
		})
		if err != nil {
			w.log.Errorf("consul error fetching leaf cert for service %s: %s", w.serviceName, err)
			time.Sleep(errorWaitTime)
			lastIndex = 0
			continue
		}

		changed := lastIndex != meta.LastIndex
		lastIndex = meta.LastIndex

		if changed {
			w.log.Infof("consul: leaf cert for service %s changed, serial: %s, valid before: %s, valid after: %s", w.serviceName, cert.SerialNumber, cert.ValidBefore, cert.ValidAfter)
			w.lock.Lock()
			if w.leaf == nil {
				w.leaf = &certLeaf{}
			}
			w.leaf.Cert = []byte(cert.CertPEM)
			w.leaf.Key = []byte(cert.PrivateKeyPEM)
			w.lock.Unlock()
			w.notifyChanged()
		}

		if first {
			w.log.Infof("consul: leaf cert for %s ready", w.serviceName)
			w.ready.Done()
			first = false
		}
	}
}

func (w *Watcher) watchService(service string, handler func(first bool, srv *api.AgentService)) {
	w.log.Infof("consul: watching service %s", service)

	hash := ""
	first := true
	for {
		srv, meta, err := w.consul.Agent().Service(service, &api.QueryOptions{
			WaitHash: hash,
			WaitTime: 10 * time.Minute,
		})
		if err != nil {
			w.log.Errorf("consul: error fetching service %s definition: %s", service, err)
			time.Sleep(errorWaitTime)
			hash = ""
			continue
		}

		changed := hash != meta.LastContentHash
		hash = meta.LastContentHash

		if changed {
			w.log.Debugf("consul: service %s changed", service)
			handler(first, srv)
			w.notifyChanged()
		}

		first = false
	}
}

func (w *Watcher) watchCA() {
	w.log.Debugf("consul: watching ca certs")

	first := true
	var lastIndex uint64
	for {
		caList, meta, err := w.consul.Agent().ConnectCARoots(&api.QueryOptions{
			WaitIndex: lastIndex,
			WaitTime:  10 * time.Minute,
		})
		if err != nil {
			w.log.Errorf("consul: error fetching cas: %s", err)
			time.Sleep(errorWaitTime)
			lastIndex = 0
			continue
		}

		changed := lastIndex != meta.LastIndex
		lastIndex = meta.LastIndex

		if changed {
			w.log.Infof("consul: CA certs changed, active root id: %s", caList.ActiveRootID)
			w.lock.Lock()
			w.certCAs = w.certCAs[:0]
			w.certCAPool = x509.NewCertPool()
			for _, ca := range caList.Roots {
				w.certCAs = append(w.certCAs, []byte(ca.RootCertPEM))
				ok := w.certCAPool.AppendCertsFromPEM([]byte(ca.RootCertPEM))
				if !ok {
					w.log.Warnf("consul: unable to add CA certificate to pool for root id: %s", caList.ActiveRootID)
				}
			}
			w.lock.Unlock()
			w.notifyChanged()
		}

		if first {
			w.log.Infof("consul: CA certs ready")
			w.ready.Done()
			first = false
		}
	}
}

func (w *Watcher) genCfg() Config {
	w.log.Debugf("generating configuration for service %s[%s]...", w.serviceName, w.service)
	w.lock.Lock()
	serviceInstancesAlive := 0
	serviceInstancesTotal := 0
	defer func() {
		w.lock.Unlock()
		w.log.Debugf("done generating configuration, instances: %d/%d total",
			serviceInstancesAlive, serviceInstancesTotal)
	}()

	config := Config{
		ServiceName: w.serviceName,
		ServiceID:   w.service,
		CAsPool:     w.certCAPool,
		Downstream: Downstream{
			LocalBindAddress:  w.downstream.LocalBindAddress,
			LocalBindPort:     w.downstream.LocalBindPort,
			TargetAddress:     w.downstream.TargetAddress,
			TargetPort:        w.downstream.TargetPort,
			Protocol:          w.downstream.Protocol,
			EnableForwardFor:  w.downstream.EnableForwardFor,
			AppNameHeaderName: w.downstream.AppNameHeaderName,

			TLS: TLS{
				CAs:  w.certCAs,
				Cert: w.leaf.Cert,
				Key:  w.leaf.Key,
			},
		},
	}

	for _, up := range w.upstreams {
		upstream := Upstream{
			Name:             up.Name,
			LocalBindAddress: up.LocalBindAddress,
			LocalBindPort:    up.LocalBindPort,
			Protocol:         up.Protocol,
			TLS: TLS{
				CAs:  w.certCAs,
				Cert: w.leaf.Cert,
				Key:  w.leaf.Key,
			},
		}
		for _, s := range up.Nodes {
			serviceInstancesTotal++

			name := s.Node.Node

			address := s.Service.Address
			if address == "" {
				address = s.Node.Address
			}

			weight := 1
			switch s.Checks.AggregatedStatus() {
			case api.HealthPassing:
				weight = s.Service.Weights.Passing
			case api.HealthWarning:
				weight = s.Service.Weights.Warning
			default:
				continue
			}
			if weight == 0 {
				continue
			}
			serviceInstancesAlive++

			upstream.Nodes = append(upstream.Nodes, UpstreamNode{
				Name:    name,
				Address: address,
				Port:    s.Service.Port,
				Weight:  weight,
			})
		}

		sort.Slice(upstream.Nodes, func(i, j int) bool {
			return upstream.Nodes[i].Name < upstream.Nodes[j].Name
		})

		config.Upstreams = append(config.Upstreams, upstream)
	}

	return config
}

func (w *Watcher) notifyChanged() {
	select {
	case w.update <- struct{}{}:
	default:
	}
}
