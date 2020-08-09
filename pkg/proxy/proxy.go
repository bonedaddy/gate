package proxy

import (
	"context"
	"errors"
	"fmt"
	"go.minekube.com/common/minecraft/component"
	"go.minekube.com/common/minecraft/component/codec/legacy"
	"go.minekube.com/gate/internal/health"
	"go.minekube.com/gate/internal/util/auth"
	"go.minekube.com/gate/pkg/config"
	"go.minekube.com/gate/pkg/event"
	"go.minekube.com/gate/pkg/proto"
	"go.minekube.com/gate/pkg/proto/packet/plugin"
	"go.minekube.com/gate/pkg/proxy/message"
	"go.minekube.com/gate/pkg/util"
	"go.minekube.com/gate/pkg/util/favicon"
	"go.minekube.com/gate/pkg/util/sets"
	"go.uber.org/zap"
	rpc "google.golang.org/grpc/health/grpc_health_v1"
	"io/ioutil"
	"net"
	"strings"
	"sync"
	"time"
)

// Proxy is the "Gate" for proxying and managing
// Minecraft connections in a network.
type Proxy struct {
	*connect
	config           *config.Config
	event            *event.Manager
	channelRegistrar *ChannelRegistrar
	authenticator    *auth.Authenticator

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup

	motd    *component.Text
	favicon favicon.Favicon

	mu      sync.RWMutex                // Protects following fields
	servers map[string]RegisteredServer // registered backend servers: by lower case names
}

// New returns a new initialized Proxy.
func New(config config.Config) (s *Proxy) {
	defer func() {
		s.connect = newConnect(s)
	}()
	return &Proxy{
		closed:           make(chan struct{}),
		config:           &config,
		event:            event.NewManager(),
		channelRegistrar: NewChannelRegistrar(),
		servers:          map[string]RegisteredServer{},
		authenticator:    auth.NewAuthenticator(),
	}
}

// Run runs the proxy and blocks until Shutdown is called or an error occurred.
// Run can only be called once per Proxy instance.
func (p *Proxy) Run() (err error) {
	select {
	default:
		// We can run the proxy
		p.wg.Add(1)
		defer p.wg.Done()
		return p.run()
	case <-p.closed:
		return errors.New("proxy was already run, create a new one")
	}
}

// Shutdown shuts down the Proxy and blocks until finished.
//
// It first stops listening for new connections, disconnects
// all existing connections with the given reason (if nil stands for no reason)
// and waits for all event subscribers to finish.
func (p *Proxy) Shutdown(reason component.Component) {
	p.closeOnce.Do(func() {
		zap.L().Info("Shutting down the proxy...")
		defer zap.L().Info("Finished shutdown.")
		close(p.closed)
		p.connect.DisconnectAll(reason)
		p.event.Wait()
		p.wg.Wait()
	})
}

func (p *Proxy) preInit() (err error) {
	c := p.config
	// Parse status motd
	if len(c.Status.Motd) != 0 {
		var motd component.Component
		if strings.HasPrefix(c.Status.Motd, "{") {
			motd, err = util.LatestJsonCodec().Unmarshal([]byte(c.Status.Motd))
		} else {
			motd, err = (&legacy.Legacy{}).Unmarshal([]byte(c.Status.Motd))
		}
		if err != nil {
			return err
		}
		t, ok := motd.(*component.Text)
		if !ok {
			return errors.New("specified motd is not a text component")
		}
		p.motd = t
	}
	// Load favicon
	if len(c.Status.Favicon) != 0 {
		if strings.HasPrefix(c.Status.Favicon, "data:image/") {
			p.favicon = favicon.Favicon(c.Status.Favicon)
			zap.L().Info("Using favicon from data uri")
		} else {
			p.favicon, err = favicon.FromFile(c.Status.Favicon)
			if err != nil {
				return fmt.Errorf("error reading favicon %q: %w", c.Status.Favicon, err)
			}
			zap.S().Infof("Using favicon file %s", c.Status.Favicon)
		}
	}

	// Register servers
	for name, addr := range c.Servers {
		p.Register(NewServerInfo(name, tcpAddr(addr)))
	}
	if len(c.Servers) != 0 {
		zap.S().Infof("Pre-registered %d servers", len(c.Servers))
	}
	return
}

func (p *Proxy) run() error {
	if err := p.preInit(); err != nil {
		return fmt.Errorf("pre-initialization error: %w", err)
	}

	errChan := make(chan error, 1)
	wg := new(sync.WaitGroup)
	defer wg.Wait()

	if p.config.Health.Enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errChan <- p.runHealthService(p.closed)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		errChan <- p.connect.listenAndServe(p.config.Bind, p.closed)
	}()

	return <-errChan
}

func (p *Proxy) runHealthService(stop <-chan struct{}) error {
	probe := p.config.Health
	run, err := health.New(probe.Bind)
	if err != nil {
		return fmt.Errorf("error creating health probe service: %w", err)
	}
	zap.S().Infof("Health probe service running at %s", probe.Bind)
	return run(stop, p.healthCheck)
}

// Event returns the Proxy's event manager.
func (p *Proxy) Event() *event.Manager {
	return p.event
}

// Config returns the config used by the Proxy.
func (p *Proxy) Config() config.Config {
	return *p.config
}

// Server gets a backend server registered with the proxy by name.
// Returns nil if not found.
func (p *Proxy) Server(name string) RegisteredServer {
	name = strings.ToLower(name)
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.servers[name] // may be nil
}

// Servers gets all registered servers.
func (p *Proxy) Servers() []RegisteredServer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	l := make([]RegisteredServer, 0, len(p.servers))
	for _, rs := range p.servers {
		l = append(l, rs)
	}
	return l
}

// Register registers a server with the proxy.
//
// Returns the new registered server and true on success.
//
// On failure either:
//  - if name already exists, returns the already registered server and false
//  - if the name or address specified in the info is invalid, returns nil and false.
func (p *Proxy) Register(info ServerInfo) (RegisteredServer, bool) {
	if !config.ValidServerName(info.Name()) ||
		config.ValidHostPort(info.Addr().String()) != nil {
		return nil, false
	}

	name := strings.ToLower(info.Name())

	p.mu.Lock()
	defer p.mu.Unlock()
	if exists, ok := p.servers[name]; ok {
		return exists, false
	}
	rs := newRegisteredServer(info)
	p.servers[name] = rs

	zap.S().Debugf("Registered server %q (%s)", info.Name(), info.Addr())
	return rs, true
}

// Unregister unregisters the server exactly matching the
// given ServerInfo and returns true if found.
func (p *Proxy) Unregister(info ServerInfo) bool {
	name := strings.ToLower(info.Name())
	p.mu.Lock()
	defer p.mu.Unlock()
	rs, ok := p.servers[name]
	if !ok || !rs.ServerInfo().Equals(info) {
		return false
	}
	delete(p.servers, name)

	zap.S().Debugf("Unregistered server %q (%s)", info.Name(), info.Addr())
	return true
}

func (p *Proxy) ChannelRegistrar() *ChannelRegistrar {
	return p.channelRegistrar
}

//
//
//
//
//
//

type ChannelRegistrar struct {
	mu          sync.RWMutex // Protects following fields
	identifiers map[string]message.ChannelIdentifier
}

func NewChannelRegistrar() *ChannelRegistrar {
	return &ChannelRegistrar{identifiers: map[string]message.ChannelIdentifier{}}
}

// ChannelsForProtocol returns all the channel names
// to register depending on the Minecraft protocol version.
func (r *ChannelRegistrar) ChannelsForProtocol(protocol proto.Protocol) sets.String {
	if protocol.GreaterEqual(proto.Minecraft_1_13) {
		return r.ModernChannelIds()
	}
	return r.LegacyChannelIds()
}

// ModernChannelIds returns all channel IDs (as strings)
// for use with Minecraft 1.13 and above.
func (r *ChannelRegistrar) ModernChannelIds() sets.String {
	r.mu.RLock()
	ids := r.identifiers
	r.mu.RUnlock()
	ss := sets.String{}
	for _, i := range ids {
		if _, ok := i.(*message.MinecraftChannelIdentifier); ok {
			ss.Insert(i.Id())
		} else {
			ss.Insert(plugin.TransformLegacyToModernChannel(i.Id()))
		}
	}
	return ss
}

// LegacyChannelIds returns all legacy channel IDs.
func (r *ChannelRegistrar) LegacyChannelIds() sets.String {
	r.mu.RLock()
	ids := r.identifiers
	r.mu.RUnlock()
	ss := sets.String{}
	for _, i := range ids {
		ss.Insert(i.Id())
	}
	return ss
}

func (r *ChannelRegistrar) FromId(channel string) (message.ChannelIdentifier, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.identifiers[channel]
	return id, ok
}

//
//
//
//
//

// pings the proxy to check health
func (p *Proxy) healthCheck(c context.Context) (*rpc.HealthCheckResponse, error) {
	ctx, cancel := context.WithTimeout(c, time.Second)
	defer cancel()

	var dialer net.Dialer
	client, err := dialer.DialContext(ctx, "tcp", p.config.Bind)
	if err != nil {
		return &rpc.HealthCheckResponse{Status: rpc.HealthCheckResponse_NOT_SERVING}, nil
	}
	defer client.Close()

	if err = client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		return &rpc.HealthCheckResponse{Status: rpc.HealthCheckResponse_NOT_SERVING}, nil
	}

	data, err := ioutil.ReadAll(client)
	if err != nil || len(data) == 0 {
		return &rpc.HealthCheckResponse{Status: rpc.HealthCheckResponse_NOT_SERVING}, nil
	}
	return &rpc.HealthCheckResponse{Status: rpc.HealthCheckResponse_SERVING}, nil
}
