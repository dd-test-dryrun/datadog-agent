// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025-present Datadog, Inc.

//go:build linux

package service

import (
	"context"
	"time"

	"go.uber.org/fx"

	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	"github.com/DataDog/datadog-agent/pkg/collector/corechecks/servicediscovery/model"
	pkgconfigsetup "github.com/DataDog/datadog-agent/pkg/config/setup"
	sysprobeclient "github.com/DataDog/datadog-agent/pkg/system-probe/api/client"
	sysconfig "github.com/DataDog/datadog-agent/pkg/system-probe/config"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const (
	collectorID = "service"
)

type collector struct {
	id      string
	catalog workloadmeta.AgentType
	store   workloadmeta.Component

	sysProbeClient *sysprobeclient.CheckClient
}

func NewCollector() (workloadmeta.CollectorProvider, error) {
	return workloadmeta.CollectorProvider{
		Collector: &collector{
			id:      collectorID,
			catalog: workloadmeta.NodeAgent,
			sysProbeClient: sysprobeclient.GetCheckClient(pkgconfigsetup.SystemProbe().GetString("system_probe_config.sysprobe_socket")),
		},
	}, nil
}

func GetFxOptions() fx.Option {
	return fx.Provide(NewCollector)
}

func (c *collector) Start(_ context.Context, store workloadmeta.Component) error {
	log.Debugf("initializing Service Discovery collector")
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		for range ticker.C {
			c.getDiscoveryServices()
		}
	}()

	return nil
}

func (c *collector) Pull(_ context.Context) error {
	return nil
}

func (c *collector) GetID() string {
	return c.id
}

func (c *collector) GetTargetCatalog() workloadmeta.AgentType {
	return c.catalog
}

func (c *collector) getDiscoveryServices() (*model.ServicesResponse, error) {
	resp, err := sysprobeclient.GetCheck[model.ServicesResponse](c.sysProbeClient, sysconfig.DiscoveryModule)
	if err != nil {
		return nil, err
	}
	log.Debugf("service collector: running services count: %d", resp.RunningServicesCount)

	return &resp, nil
}