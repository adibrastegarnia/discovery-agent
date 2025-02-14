// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"github.com/onosproject/onos-net-lib/pkg/configtree"
	"github.com/onosproject/onos-net-lib/pkg/gnmiutils"
	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/spf13/viper"
	"path/filepath"
	"time"
)

var configFile = "/etc/discovery-agent/config.yaml" // not a constant for testing purposes

// Config contains configuration parameters for the link discovery
type Config struct {
	EmitFrequency               int64 `mapstructure:"emitFrequency" yaml:"emitFrequency"`
	MaxLinkAge                  int64 `mapstructure:"maxLinkAge" yaml:"maxLinkAge"`
	PipelineValidationFrequency int64 `mapstructure:"pipelineValidationFrequency" yaml:"pipelineValidationFrequency"`
	PortRediscoveryFrequency    int64 `mapstructure:"portRediscoveryFrequency" yaml:"portRediscoveryFrequency"`
	LinkPruneFrequency          int64 `mapstructure:"linkPruneFrequency" yaml:"linkPruneFrequency"`
}

type configWrapper struct {
	Config *Config `mapstructure:"config" yaml:"config"`
}

func loadConfig() *Config {
	wrapper := &configWrapper{
		Config: &Config{
			EmitFrequency:               5,
			MaxLinkAge:                  30,
			PipelineValidationFrequency: 60,
			PortRediscoveryFrequency:    60,
			LinkPruneFrequency:          2,
		},
	}

	cfg := viper.New()
	cfg.SetConfigType("yaml")
	cfg.SetConfigName(filepath.Base(configFile))
	cfg.AddConfigPath(filepath.Dir(configFile))
	if err := cfg.ReadInConfig(); err != nil {
		log.Warnf("Unable to load config file; using defaults: %+v", err)
	}

	if err := cfg.Unmarshal(wrapper); err != nil {
		log.Warnf("Unable to parse config file; using defaults: %+v", err)
	}
	return wrapper.Config
}

func saveConfig(config *Config) {
	cfg := viper.New()
	cfg.Set("config", config)
	if err := cfg.WriteConfigAs(configFile); err != nil {
		log.Warnf("Unable to save config file: %+v", err)
	}
}

// Creates a root config tree and populates its "config/" branch with the supplied configuration values.
func createConfigRoot(agentID string, config *Config) *configtree.Node {
	root := configtree.NewRoot()
	root.AddPath("state/agent-id",
		&gnmi.TypedValue{Value: &gnmi.TypedValue_StringVal{StringVal: agentID}})
	root.AddPath("config/emitFrequency",
		&gnmi.TypedValue{Value: &gnmi.TypedValue_IntVal{IntVal: config.EmitFrequency}})
	root.AddPath("config/maxLinkAge",
		&gnmi.TypedValue{Value: &gnmi.TypedValue_IntVal{IntVal: config.MaxLinkAge}})
	root.AddPath("config/pipelineValidationFrequency",
		&gnmi.TypedValue{Value: &gnmi.TypedValue_IntVal{IntVal: config.PipelineValidationFrequency}})
	root.AddPath("config/portRediscoveryFrequency",
		&gnmi.TypedValue{Value: &gnmi.TypedValue_IntVal{IntVal: config.PortRediscoveryFrequency}})
	root.AddPath("config/linkPruneFrequency",
		&gnmi.TypedValue{Value: &gnmi.TypedValue_IntVal{IntVal: config.LinkPruneFrequency}})
	root.Add("state/links", nil, nil)
	return root
}

// UpdateConfig should be called after the configuration tree has been updated to save the configuration and
// to reflect it back to the controller's Config structure for easy access.
func (c *Controller) UpdateConfig() {
	root := c.Root()
	c.config.EmitFrequency = root.GetPath("config/emitFrequency").Value().GetIntVal()
	c.config.MaxLinkAge = root.GetPath("config/maxLinkAge").Value().GetIntVal()
	c.config.PipelineValidationFrequency = root.GetPath("config/pipelineValidationFrequency").Value().GetIntVal()
	c.config.PortRediscoveryFrequency = root.GetPath("config/portRediscoveryFrequency").Value().GetIntVal()
	c.config.LinkPruneFrequency = root.GetPath("config/linkPruneFrequency").Value().GetIntVal()
	saveConfig(c.config)
	c.setStateIf(Configured, Reconfigured)
}

// RefreshConfig refreshes the config tree state from any relevant external source state
func (c *Controller) RefreshConfig() {
	// no-op here
}

func (c *Controller) addLinkToTree(ingressPort uint32, egressPort uint32, egressDeviceID string) {
	portPath := fmt.Sprintf("state/link[port=%d]/egress-port", ingressPort)
	portVal := &gnmi.TypedValue{Value: &gnmi.TypedValue_IntVal{IntVal: int64(egressPort)}}
	devicePath := fmt.Sprintf("state/link[port=%d]/egress-device", ingressPort)
	deviceVal := &gnmi.TypedValue{Value: &gnmi.TypedValue_StringVal{StringVal: egressDeviceID}}
	createTimePath := fmt.Sprintf("state/link[port=%d]/create-time", ingressPort)
	createTimeVal := &gnmi.TypedValue{Value: &gnmi.TypedValue_UintVal{UintVal: uint64(time.Now().UnixNano())}}

	c.Root().AddPath(portPath, portVal)
	c.Root().AddPath(devicePath, deviceVal)
	c.Root().AddPath(createTimePath, createTimeVal)

	// Forward the add notification to any subscribe responders
	c.SendToAllResponders(&gnmi.SubscribeResponse{Response: &gnmi.SubscribeResponse_Update{
		Update: &gnmi.Notification{
			Timestamp: time.Now().UnixNano(),
			Update: []*gnmi.Update{
				{Path: gnmiutils.ToPath(portPath), Val: portVal},
				{Path: gnmiutils.ToPath(devicePath), Val: deviceVal},
				{Path: gnmiutils.ToPath(createTimePath), Val: createTimeVal},
			},
		},
	}})
}

func (c *Controller) removeLinkFromTree(ingressPort uint32) {
	path := fmt.Sprintf("state/link[port=%d]", ingressPort)
	_ = c.Root().DeletePath(path)

	// Forward the delete notification to any subscribe responders
	c.SendToAllResponders(&gnmi.SubscribeResponse{Response: &gnmi.SubscribeResponse_Update{
		Update: &gnmi.Notification{
			Timestamp: time.Now().UnixNano(),
			Delete:    []*gnmi.Path{gnmiutils.ToPath(path)},
		},
	}})
}

func (c *Controller) addHostToTree(macString string, ipString string, port uint32) {
	portPath := fmt.Sprintf("state/host[mac=%s]/port", macString)
	portVal := &gnmi.TypedValue{Value: &gnmi.TypedValue_IntVal{IntVal: int64(port)}}
	ipPath := fmt.Sprintf("state/link[mac=%s]/ip-address", macString)
	ipVal := &gnmi.TypedValue{Value: &gnmi.TypedValue_StringVal{StringVal: ipString}}
	createTimePath := fmt.Sprintf("state/link[mac=%s]/create-time", macString)
	createTimeVal := &gnmi.TypedValue{Value: &gnmi.TypedValue_UintVal{UintVal: uint64(time.Now().UnixNano())}}

	c.Root().AddPath(portPath, portVal)
	c.Root().AddPath(ipPath, ipVal)
	c.Root().AddPath(createTimePath, createTimeVal)

	// Forward the add notification to any subscribe responders
	c.SendToAllResponders(&gnmi.SubscribeResponse{Response: &gnmi.SubscribeResponse_Update{
		Update: &gnmi.Notification{
			Timestamp: time.Now().UnixNano(),
			Update: []*gnmi.Update{
				{Path: gnmiutils.ToPath(portPath), Val: portVal},
				{Path: gnmiutils.ToPath(ipPath), Val: ipVal},
				{Path: gnmiutils.ToPath(createTimePath), Val: createTimeVal},
			},
		},
	}})
}

func (c *Controller) removeHostFromTree(macString string) {
	path := fmt.Sprintf("state/host[mac=%s]", macString)
	_ = c.Root().DeletePath(path)

	// Forward the delete notification to any subscribe responders
	c.SendToAllResponders(&gnmi.SubscribeResponse{Response: &gnmi.SubscribeResponse_Update{
		Update: &gnmi.Notification{
			Timestamp: time.Now().UnixNano(),
			Delete:    []*gnmi.Path{gnmiutils.ToPath(path)},
		},
	}})
}
