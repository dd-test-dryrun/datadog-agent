// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package patch

import (
	"github.com/DataDog/datadog-agent/pkg/clusteragent/telemetry"
	rcclient "github.com/DataDog/datadog-agent/pkg/config/remote/client"
	"github.com/DataDog/datadog-agent/pkg/util/log"

	"k8s.io/client-go/kubernetes"
)

// ControllerContext holds necessary context for the patch controller
type ControllerContext struct {
	LeadershipStateSubscribeFunc func() (notifChan <-chan struct{}, isLeader func() bool)
	K8sClient                    kubernetes.Interface
	RcClient                     *rcclient.Client
	ClusterName                  string
	ClusterID                    string
	StopCh                       chan struct{}
}

// StartControllers starts the patch controllers
func StartControllers(ctx ControllerContext) error {
	log.Info("Starting patch controllers")
	telemetryCollector := telemetry.NewNoopCollector()
	if ctx.RcClient != nil {
		telemetryCollector = telemetry.NewCollector(ctx.RcClient.ID, ctx.ClusterID)
	}
	leadershipStateNotif, isLeaderFunc := ctx.LeadershipStateSubscribeFunc()
	provider, err := newPatchProvider(ctx.RcClient, isLeaderFunc, leadershipStateNotif, telemetryCollector, ctx.ClusterName)
	if err != nil {
		return err
	}
	patcher := newPatcher(ctx.K8sClient, isLeaderFunc, telemetryCollector, provider)
	go provider.start(ctx.StopCh)
	go patcher.start(ctx.StopCh)
	return nil
}
