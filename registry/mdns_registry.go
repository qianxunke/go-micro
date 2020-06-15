// Package mdns is a multicast dns registry
package registry

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/micro/go-micro/v2/logger"
	"github.com/micro/go-micro/v2/util/mdns"
)

type mdnsTxt struct {
	Service   string
	Version   string
	Endpoints []*Endpoint
	Metadata  map[string]string
}

type mdnsEntry struct {
	id   string
	node *mdns.Server
}

type mdnsRegistry struct {
	opts Options
	// the default domain
	domain string

	sync.Mutex
	// services grouped by domain
	services map[string]map[string][]*mdnsEntry

	mtx sync.RWMutex

	// watchers
	watchers map[string]*mdnsWatcher

	// listener
	listener chan *mdns.ServiceEntry
}

type mdnsWatcher struct {
	id   string
	wo   WatchOptions
	ch   chan *mdns.ServiceEntry
	exit chan struct{}
	// the mdns domain
	domain string
	// the registry
	registry *mdnsRegistry
}

func encode(txt *mdnsTxt) ([]string, error) {
	b, err := json.Marshal(txt)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	defer buf.Reset()

	w := zlib.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	w.Close()

	encoded := hex.EncodeToString(buf.Bytes())

	// individual txt limit
	if len(encoded) <= 255 {
		return []string{encoded}, nil
	}

	// split encoded string
	var record []string

	for len(encoded) > 255 {
		record = append(record, encoded[:255])
		encoded = encoded[255:]
	}

	record = append(record, encoded)

	return record, nil
}

func decode(record []string) (*mdnsTxt, error) {
	encoded := strings.Join(record, "")

	hr, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, err
	}

	br := bytes.NewReader(hr)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, err
	}

	rbuf, err := ioutil.ReadAll(zr)
	if err != nil {
		return nil, err
	}

	var txt *mdnsTxt

	if err := json.Unmarshal(rbuf, &txt); err != nil {
		return nil, err
	}

	return txt, nil
}
func newRegistry(opts ...Option) Registry {
	options := Options{
		Context: context.Background(),
		Timeout: time.Millisecond * 100,
		Domain:  "micro",
	}

	for _, o := range opts {
		o(&options)
	}

	return &mdnsRegistry{
		opts:     options,
		services: make(map[string]map[string][]*mdnsEntry),
		watchers: make(map[string]*mdnsWatcher),
	}
}

func (m *mdnsRegistry) Init(opts ...Option) error {
	for _, o := range opts {
		o(&m.opts)
	}
	return nil
}

func (m *mdnsRegistry) Options() Options {
	return m.opts
}

func (m *mdnsRegistry) Register(service *Service, opts ...RegisterOption) error {
	m.Lock()
	defer m.Unlock()

	// parse the options, fallback to the default domain
	var options RegisterOptions
	for _, o := range opts {
		o(&options)
	}
	if len(options.Domain) == 0 {
		options.Domain = m.opts.Domain
	}

	// ensure the domain exists in the map
	if _, ok := m.services[options.Domain]; !ok {
		m.services[options.Domain] = make(map[string][]*mdnsEntry)
	}

	entries, ok := m.services[options.Domain][service.Name]
	// first entry, create wildcard used for list queries
	if !ok {
		s, err := mdns.NewMDNSService(
			service.Name,
			"_services",
			options.Domain+".",
			"",
			9999,
			[]net.IP{net.ParseIP("0.0.0.0")},
			nil,
		)
		if err != nil {
			return err
		}

		srv, err := mdns.NewServer(&mdns.Config{Zone: &mdns.DNSSDService{MDNSService: s}})
		if err != nil {
			return err
		}

		// append the wildcard entry
		entries = append(entries, &mdnsEntry{id: "*", node: srv})
	}

	var gerr error

	for _, node := range service.Nodes {
		var seen bool
		var e *mdnsEntry

		for _, entry := range entries {
			if node.Id == entry.id {
				seen = true
				e = entry
				break
			}
		}

		// already registered, continue
		if seen {
			continue
			// doesn't exist
		} else {
			e = &mdnsEntry{}
		}

		txt, err := encode(&mdnsTxt{
			Service:   service.Name,
			Version:   service.Version,
			Endpoints: service.Endpoints,
			Metadata:  node.Metadata,
		})

		if err != nil {
			gerr = err
			continue
		}

		host, pt, err := net.SplitHostPort(node.Address)
		if err != nil {
			gerr = err
			continue
		}
		port, _ := strconv.Atoi(pt)

		if logger.V(logger.DebugLevel, logger.DefaultLogger) {
			logger.Debugf("[mdns] registry create new service with ip: %s for: %s", net.ParseIP(host).String(), host)
		}
		// we got here, new node
		s, err := mdns.NewMDNSService(
			node.Id,
			service.Name,
			options.Domain+".",
			"",
			port,
			[]net.IP{net.ParseIP(host)},
			txt,
		)
		if err != nil {
			gerr = err
			continue
		}

		srv, err := mdns.NewServer(&mdns.Config{Zone: s})
		if err != nil {
			gerr = err
			continue
		}

		e.id = node.Id
		e.node = srv
		entries = append(entries, e)
	}

	// save
	m.services[options.Domain][service.Name] = entries

	return gerr
}

func (m *mdnsRegistry) Deregister(service *Service, opts ...DeregisterOption) error {
	m.Lock()
	defer m.Unlock()

	// parse the options, fallback to the default domain
	var options DeregisterOptions
	for _, o := range opts {
		o(&options)
	}
	if len(options.Domain) == 0 {
		options.Domain = m.opts.Domain
	}

	// ensure the domain exists in the map
	if _, ok := m.services[options.Domain]; !ok {
		m.services[options.Domain] = make(map[string][]*mdnsEntry)
	}

	var newEntries []*mdnsEntry

	// loop existing entries, check if any match, shutdown those that do
	for _, entry := range m.services[options.Domain][service.Name] {
		var remove bool

		for _, node := range service.Nodes {
			if node.Id == entry.id {
				entry.node.Shutdown()
				remove = true
				break
			}
		}

		// keep it?
		if !remove {
			newEntries = append(newEntries, entry)
		}
	}

	// last entry is the wildcard for list queries. Remove it.
	if len(newEntries) == 1 && newEntries[0].id == "*" {
		newEntries[0].node.Shutdown()
		delete(m.services, service.Name)
	} else {
		m.services[options.Domain][service.Name] = newEntries
	}

	return nil
}

func (m *mdnsRegistry) GetService(service string, opts ...GetOption) ([]*Service, error) {
	// parse the options, fallback to the default domain
	var options GetOptions
	for _, o := range opts {
		o(&options)
	}
	if len(options.Domain) == 0 {
		m.Lock()
		options.Domain = m.opts.Domain
		m.Unlock()
	}

	serviceMap := make(map[string]*Service)
	entries := make(chan *mdns.ServiceEntry, 10)
	done := make(chan bool)

	p := mdns.DefaultParams(service)
	// set context with timeout
	var cancel context.CancelFunc
	p.Context, cancel = context.WithTimeout(context.Background(), m.opts.Timeout)
	defer cancel()
	// set entries channel
	p.Entries = entries
	// set the domain
	p.Domain = options.Domain

	go func() {
		for {
			select {
			case e := <-entries:
				// list record so skip
				if p.Service == "_services" {
					continue
				}
				// ensure the domain matches
				if !strings.HasSuffix(e.Name, "."+p.Domain+".") {
					continue
				}
				if e.TTL == 0 {
					continue
				}

				txt, err := decode(e.InfoFields)
				if err != nil {
					continue
				}

				if txt.Service != service {
					continue
				}

				s, ok := serviceMap[txt.Version]
				if !ok {
					s = &Service{
						Name:      txt.Service,
						Version:   txt.Version,
						Endpoints: txt.Endpoints,
					}
				}
				addr := ""
				// prefer ipv4 addrs
				if len(e.AddrV4) > 0 {
					addr = e.AddrV4.String()
					// else use ipv6
				} else if len(e.AddrV6) > 0 {
					addr = "[" + e.AddrV6.String() + "]"
				} else {
					if logger.V(logger.InfoLevel, logger.DefaultLogger) {
						logger.Infof("[mdns]: invalid endpoint received: %v", e)
					}
					continue
				}
				s.Nodes = append(s.Nodes, &Node{
					Id:       strings.TrimSuffix(e.Name, "."+p.Service+"."+p.Domain+"."),
					Address:  fmt.Sprintf("%s:%d", addr, e.Port),
					Metadata: txt.Metadata,
				})

				serviceMap[txt.Version] = s
			case <-p.Context.Done():
				close(done)
				return
			}
		}
	}()

	// execute the query
	if err := mdns.Query(p); err != nil {
		return nil, err
	}

	// wait for completion
	<-done

	// if no services were returned, error
	if len(serviceMap) == 0 {
		return nil, ErrNotFound
	}

	// create list and return
	services := make([]*Service, 0, len(serviceMap))

	for _, service := range serviceMap {
		services = append(services, service)
	}

	return services, nil
}

func (m *mdnsRegistry) ListServices(opts ...ListOption) ([]*Service, error) {
	// parse the options, fallback to the default domain
	var options ListOptions
	for _, o := range opts {
		o(&options)
	}
	if len(options.Domain) == 0 {
		m.Lock()
		options.Domain = m.opts.Domain
		m.Unlock()
	}

	serviceMap := make(map[string]bool)
	entries := make(chan *mdns.ServiceEntry, 10)
	done := make(chan bool)

	p := mdns.DefaultParams("_services")
	// set context with timeout
	var cancel context.CancelFunc
	p.Context, cancel = context.WithTimeout(context.Background(), m.opts.Timeout)
	defer cancel()
	// set entries channel
	p.Entries = entries
	// set domain
	p.Domain = options.Domain

	var services []*Service

	go func() {
		for {
			select {
			case e := <-entries:
				if e.TTL == 0 {
					continue
				}
				if !strings.HasSuffix(e.Name, p.Domain+".") {
					continue
				}
				name := strings.TrimSuffix(e.Name, "."+p.Service+"."+p.Domain+".")
				if !serviceMap[name] {
					serviceMap[name] = true
					services = append(services, &Service{Name: name})
				}
			case <-p.Context.Done():
				close(done)
				return
			}
		}
	}()

	// execute query
	if err := mdns.Query(p); err != nil {
		return nil, err
	}

	// wait till done
	<-done

	return services, nil
}

func (m *mdnsRegistry) Watch(opts ...WatchOption) (Watcher, error) {
	// parse the options, fallback to the default domain
	var wo WatchOptions
	for _, o := range opts {
		o(&wo)
	}
	if len(wo.Domain) == 0 {
		m.Lock()
		wo.Domain = m.opts.Domain
		m.Unlock()
	}

	md := &mdnsWatcher{
		id:       uuid.New().String(),
		wo:       wo,
		ch:       make(chan *mdns.ServiceEntry, 32),
		exit:     make(chan struct{}),
		domain:   wo.Domain,
		registry: m,
	}

	m.mtx.Lock()
	defer m.mtx.Unlock()

	// save the watcher
	m.watchers[md.id] = md

	// check of the listener exists
	if m.listener != nil {
		return md, nil
	}

	// start the listener
	go func() {
		// go to infinity
		for {
			m.mtx.Lock()

			// just return if there are no watchers
			if len(m.watchers) == 0 {
				m.listener = nil
				m.mtx.Unlock()
				return
			}

			// check existing listener
			if m.listener != nil {
				m.mtx.Unlock()
				return
			}

			// reset the listener
			exit := make(chan struct{})
			ch := make(chan *mdns.ServiceEntry, 32)
			m.listener = ch

			m.mtx.Unlock()

			// send messages to the watchers
			go func() {
				send := func(w *mdnsWatcher, e *mdns.ServiceEntry) {
					select {
					case w.ch <- e:
					default:
					}
				}

				for {
					select {
					case <-exit:
						return
					case e, ok := <-ch:
						if !ok {
							return
						}
						m.mtx.RLock()
						// send service entry to all watchers
						for _, w := range m.watchers {
							send(w, e)
						}
						m.mtx.RUnlock()
					}
				}

			}()

			// start listening, blocking call
			mdns.Listen(ch, exit)

			// mdns.Listen has unblocked
			// kill the saved listener
			m.mtx.Lock()
			m.listener = nil
			close(ch)
			m.mtx.Unlock()
		}
	}()

	return md, nil
}

func (m *mdnsRegistry) String() string {
	return "mdns"
}

func (m *mdnsWatcher) entryToRecord(e *mdns.ServiceEntry) *Result {
	txt, err := decode(e.InfoFields)
	if err != nil {
		return nil
	}

	if len(txt.Service) == 0 || len(txt.Version) == 0 {
		return nil
	}

	// Filter watch options
	// wo.Service: Only keep services we care about
	if len(m.wo.Service) > 0 && txt.Service != m.wo.Service {
		return nil
	}

	var action string
	if e.TTL == 0 {
		action = "delete"
	} else {
		action = "create"
	}

	service := &Service{
		Name:      txt.Service,
		Version:   txt.Version,
		Endpoints: txt.Endpoints,
	}

	// skip anything without the domain we care about
	suffix := fmt.Sprintf(".%s.%s.", service.Name, m.domain)
	if !strings.HasSuffix(e.Name, suffix) {
		return nil
	}

	var addr string
	if len(e.AddrV4) > 0 {
		addr = e.AddrV4.String()
	} else if len(e.AddrV6) > 0 {
		addr = "[" + e.AddrV6.String() + "]"
	} else {
		addr = e.Addr.String()
	}

	service.Nodes = append(service.Nodes, &Node{
		Id:       strings.TrimSuffix(e.Name, suffix),
		Address:  fmt.Sprintf("%s:%d", addr, e.Port),
		Metadata: txt.Metadata,
	})

	return &Result{Action: action, Service: service}
}

func (m *mdnsWatcher) Chan() chan *Result {
	c := make(chan *Result)

	go func() {
		for {
			select {
			case e := <-m.ch:
				if result := m.entryToRecord(e); result != nil {
					c <- result
				}
			case <-m.exit:
				close(c)
				return
			}
		}
	}()

	return c
}

func (m *mdnsWatcher) Next() (*Result, error) {
	for {
		select {
		case e := <-m.ch:
			if result := m.entryToRecord(e); result != nil {
				return result, nil
			}
		case <-m.exit:
			return nil, ErrWatcherStopped
		}
	}
}

func (m *mdnsWatcher) Stop() {
	select {
	case <-m.exit:
		return
	default:
		close(m.exit)
		// remove self from the registry
		m.registry.mtx.Lock()
		delete(m.registry.watchers, m.id)
		m.registry.mtx.Unlock()
	}
}

// NewRegistry returns a new default registry which is mdns
func NewRegistry(opts ...Option) Registry {
	return newRegistry(opts...)
}
