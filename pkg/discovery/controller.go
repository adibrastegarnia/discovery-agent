// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

// Package discovery implements the link and host discovery control logic
package discovery

import (
	"context"
	"github.com/onosproject/onos-lib-go/pkg/logging"
	"github.com/onosproject/onos-net-lib/pkg/configtree"
	"github.com/onosproject/onos-net-lib/pkg/p4utils"
	"github.com/openconfig/gnmi/proto/gnmi"
	p4info "github.com/p4lang/p4runtime/go/p4/config/v1"
	p4api "github.com/p4lang/p4runtime/go/p4/v1"
	"google.golang.org/grpc"
	"sort"
	"sync"
	"time"
)

var log = logging.GetLogger("linkdiscovery")

// State represents the various states of controller lifecycle
type State int

const (
	// Disconnected represents the default/initial state
	Disconnected State = iota
	// Connected represents state where Stratum connection(s) have been established
	Connected
	// PipelineConfigAvailable represents state where P4Info has been obtained
	PipelineConfigAvailable
	// Elected represents state where the link agent established mastership for its role
	Elected
	// PortsDiscovered represents state where the link agent discovered all Stratum ports
	PortsDiscovered
	// Configured represents state where the link agent has been fully configured and can discover links
	Configured
	// Reconfigured represents state where new configuration has been received
	Reconfigured
	// Stopped represents state where the link agent has been issued a stop command
	Stopped
)

// Controller represents the link discovery control
type Controller struct {
	configtree.Configurable
	configtree.GNMIConfigurable

	TargetAddress   string
	IngressDeviceID string

	state  State
	lock   sync.RWMutex
	config *Config
	ports  map[string]*Port
	links  map[uint32]*Link
	hosts  map[string]*Host

	conn       *grpc.ClientConn
	p4Client   p4api.P4RuntimeClient
	gnmiClient gnmi.GNMIClient

	ctx       context.Context
	ctxCancel context.CancelFunc

	chassisID  uint64
	info       *p4info.P4Info
	codec      *p4utils.ControllerMetadataCodec
	stream     p4api.P4Runtime_StreamChannelClient
	electionID *p4api.Uint128
	cookie     uint64
	role       *p4api.Role

	monitor *portMonitor
}

// Port holds data about each discovered switch ports
type Port struct {
	ID         string
	Number     uint32
	Status     string
	LastChange uint64
}

// Link holds data about each discovered ingress links
type Link struct {
	EgressPort     uint32
	EgressDeviceID string
	IngressPort    uint32
	LastUpdate     time.Time
}

// Host is a simple representation of a host network interface discovered by the ONOS lite
type Host struct {
	MAC        string
	IP         string
	Port       uint32
	LastUpdate time.Time
}

// NewController creates a new link discovery controller
func NewController(targetAddress string, agentID string) *Controller {
	config := loadConfig()
	ctrl := &Controller{
		GNMIConfigurable: *configtree.NewGNMIConfigurable(createConfigRoot(agentID, config)),
		TargetAddress:    targetAddress,
		IngressDeviceID:  agentID,
		config:           config,
		ports:            make(map[string]*Port),
		links:            make(map[uint32]*Link),
		monitor:          &portMonitor{},
	}
	ctrl.GNMIConfigurable.Configurable = ctrl
	return ctrl
}

// Start starts the controller
func (c *Controller) Start() {
	log.Infof("Starting...")
	go c.run()
}

// Stop stops the controller
func (c *Controller) Stop() {
	log.Infof("Stopping...")
	c.setState(Stopped)
	if c.ctxCancel != nil {
		c.ctxCancel()
	}
}

// GetLinks returns a list of currently discovered links, sorted by ingress port
func (c *Controller) GetLinks() []*Link {
	c.lock.RLock()
	defer c.lock.RUnlock()

	links := make([]*Link, 0, len(c.links))
	for _, link := range c.links {
		links = append(links, link)
	}

	sort.SliceStable(links, func(i, j int) bool { return links[i].IngressPort < links[j].IngressPort })
	return links
}

func (c *Controller) updateIngressLink(ingressPort uint32, egressPort uint32, egressDeviceID string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	link, ok := c.links[ingressPort]
	if !ok || link.EgressPort != egressPort || link.EgressDeviceID != egressDeviceID {
		link = &Link{
			EgressPort:     egressPort,
			EgressDeviceID: egressDeviceID,
			IngressPort:    ingressPort,
		}

		// Add the link to our internal structure and to the config tree
		c.links[ingressPort] = link
		log.Infof("Added a new link: %d <- %s/%d", ingressPort, egressDeviceID, egressPort)
		c.addLinkToTree(ingressPort, egressPort, egressDeviceID)
	}
	link.LastUpdate = time.Now()
}

func (c *Controller) pruneLinks() {
	c.lock.Lock()
	defer c.lock.Unlock()
	limit := time.Now().Add(-30 * time.Second)
	for ingressPort, link := range c.links {
		if link.LastUpdate.Before(limit) {
			c.deleteLink(ingressPort)
			log.Infof("Pruned stale link: %d <- %s/%d", link.IngressPort, link.EgressDeviceID, link.EgressPort)
		}
	}
}

func (c *Controller) deleteLink(ingressPort uint32) {
	// Delete the link from our internal structure and from the config tree
	delete(c.links, ingressPort)
	c.removeLinkFromTree(ingressPort)
}

func (c *Controller) deleteHost(macString string) {
	// Delete the link from our internal structure and from the config tree
	delete(c.hosts, macString)
	c.removeHostFromTree(macString)
}

// Get the current operational state
func (c *Controller) getState() State {
	c.lock.RLock()
	defer c.lock.RUnlock()
	state := c.state
	return state
}

// Change state to the new state
func (c *Controller) setState(state State) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.state = state
}

// Change state to the new state, but only if in the given condition state
func (c *Controller) setStateIf(condition State, state State) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.state == condition {
		c.state = state
	}
}

func (c *Controller) updateHost(macString string, ipString string, port uint32) {
	c.lock.Lock()
	defer c.lock.Unlock()
	host, ok := c.hosts[macString]
	if !ok || host.MAC != macString || host.IP != ipString || host.Port != port {
		host = &Host{
			MAC:  macString,
			IP:   ipString,
			Port: port,
		}
		c.hosts[macString] = host
		log.Infof("Added a new host: %s <- %s/%d", macString, ipString, port)
		c.addHostToTree(macString, ipString, port)
	}
	host.LastUpdate = time.Now()
}

func (c *Controller) pruneHosts() {
	c.lock.Lock()
	defer c.lock.Unlock()
	limit := time.Now().Add(-30 * time.Minute) // this is to discuss
	for mac, host := range c.hosts {
		if host.LastUpdate.Before(limit) {
			c.deleteHost(mac)
			log.Infof("Pruned stale host: %s <- %s/%s", host.MAC, host.IP, host.Port)
		}
	}
}

func (c *Controller) run() {
	log.Infof("Started")
	for state := c.getState(); state != Stopped; state = c.getState() {
		switch state {
		case Disconnected:
			c.waitForDeviceConnection()
		case Connected:
			c.waitForPipelineConfiguration()
		case PipelineConfigAvailable:
			c.waitForMastershipArbitration()
		case Elected:
			c.discoverPorts()
		case PortsDiscovered:
			c.setupForDiscovery()
		case Configured:
			c.enterDiscovery()
		case Reconfigured:
			c.reenterDiscovery()
		}
	}
	log.Infof("Stopped")
}

// Pause for the specified duration, but only if in the given condition state
func (c *Controller) pauseIf(condition State, pause time.Duration) {
	if c.getState() == condition {
		time.Sleep(pause)
	}
}

func (c *Controller) setupForDiscovery() {
	// Program intercept rule(s)
	c.programPacketInterceptRules()
	c.setState(Configured)

	// Setup packet-in handler
	go c.handlePackets()
}

func (c *Controller) enterDiscovery() {
	tLinks := time.NewTicker(time.Duration(c.config.EmitFrequency) * time.Second)
	tConf := time.NewTicker(time.Duration(c.config.PipelineValidationFrequency) * time.Second)
	tPorts := time.NewTicker(time.Duration(c.config.PortRediscoveryFrequency) * time.Second)
	tPrune := time.NewTicker(time.Duration(c.config.LinkPruneFrequency) * time.Second)

	// Do I have to emit ARP packets here? I guess so...
	for c.getState() == Configured {
		select {
		// Periodically emit LLDP packets
		case <-tLinks.C:
			c.emitLLDPPackets()

		// Periodically re-discover ports
		case <-tPorts.C:
			c.discoverPorts()

		// Periodically validate pipeline config
		case <-tConf.C:
			c.validatePipelineConfiguration()

		// Periodically prune links
		case <-tPrune.C:
			c.pruneLinks()
			c.pruneHosts()
		}
	}
}

func (c *Controller) reenterDiscovery() {
	c.setState(Configured)
}
