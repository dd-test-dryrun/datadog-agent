// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build orchestrator

// Package deploymenttagprovider implements a provider that supplies deployment tags for cluster checks
package deploymenttagprovider

import (
	"github.com/DataDog/datadog-agent/comp/core/config"
	tagger "github.com/DataDog/datadog-agent/comp/core/tagger/def"
	taggertypes "github.com/DataDog/datadog-agent/comp/core/tagger/types"
	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	// pkgconfigsetup "github.com/DataDog/datadog-agent/pkg/config/setup"

	appsv1 "k8s.io/api/apps/v1"
)

type TagProvider interface {
	GetTags(*appsv1.Deployment, taggertypes.TagCardinality) ([]string, error)
}

type tagProviderFunc func(*appsv1.Deployment, taggertypes.TagCardinality) ([]string, error)

func NewTagProvider(_ config.Component, store workloadmeta.Component, tagger tagger.Component) TagProvider {
	/*
		if pkgconfigsetup.IsCLCRunner(pkgconfigsetup.Datadog()) {
			// Running in a CLC Runner
			return newCLCTagProvider(cfg, store)
		}*/

	return newNodeTagProvider(tagger)
}

func newNodeTagProvider(tagger tagger.Component) TagProvider {
}
