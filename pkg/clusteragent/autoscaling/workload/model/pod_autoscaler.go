// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	datadoghqcommon "github.com/DataDog/datadog-operator/api/datadoghq/common"
	datadoghq "github.com/DataDog/datadog-operator/api/datadoghq/v1alpha2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// longestScalingRulePeriodAllowed is the maximum period allowed for a scaling rule
	// increasing duration increase the number of events to keep in memory and to process for recommendations.
	longestScalingRulePeriodAllowed = 60 * time.Minute

	// statusRetainedActions is the number of horizontal actions kept in status
	statusRetainedActions = 10

	// CustomRecommenderAnnotationKey is the key used to store custom recommender configuration in annotations
	CustomRecommenderAnnotationKey = "autoscaling.datadoghq.com/custom-recommender"
)

// PodAutoscalerInternal holds the necessary data to work with the `DatadogPodAutoscaler` CRD.
type PodAutoscalerInternal struct {
	// namespace is the namespace of the PodAutoscaler
	namespace string

	// name is the name of the PodAutoscaler
	name string

	// creationTimestamp is the time when the kubernetes object was created
	// creationTimestamp is stored in .DatadogPodAutoscaler.CreationTimestamp
	creationTimestamp time.Time

	// generation is the received generation of the PodAutoscaler
	generation int64

	// keeping track of .Spec (configuration of the Autoscaling)
	spec *datadoghq.DatadogPodAutoscalerSpec

	// settingsTimestamp is the time when the settings were last updated
	// Version is stored in .Spec.RemoteVersion
	// (only if owner == remote)
	settingsTimestamp time.Time

	// scalingValues represents the active scaling values that should be used
	scalingValues ScalingValues

	// mainScalingValues represents the scaling values retrieved from the main recommender (product, optionally a custom endpoint)
	mainScalingValues ScalingValues

	// fallbackScalingValues represents the scaling values retrieved from the fallback
	fallbackScalingValues ScalingValues

	// horizontalLastActions is the last horizontal action successfully taken
	horizontalLastActions []datadoghqcommon.DatadogPodAutoscalerHorizontalAction

	// horizontalLastLimitReason is stored separately as we don't want to keep no-action events in `horizontalLastActions`
	// i.e. when targetReplicaCount after limits == currentReplicas but we want to surface the last limiting reason anyway.
	horizontalLastLimitReason string

	// horizontalLastActionError is the last error encountered on horizontal scaling
	horizontalLastActionError error

	// verticalLastAction is the last action taken by the Vertical Pod Autoscaler
	verticalLastAction *datadoghqcommon.DatadogPodAutoscalerVerticalAction

	// verticalLastActionError is the last error encountered on vertical scaling
	verticalLastActionError error

	// currentReplicas is the current number of PODs for the targetRef
	currentReplicas *int32

	// scaledReplicas is the current number of PODs for the targetRef matching the resources recommendations
	scaledReplicas *int32

	// error is the an error encountered by the controller not specific to a scaling action
	error error

	// deleted flags the PodAutoscaler as deleted (removal to be handled by the controller)
	// (only if owner == remote)
	deleted bool

	//
	// Computed fields
	//
	// targetGVK is the GroupVersionKind of the target resource
	// Parsed once from the .Spec.TargetRef
	targetGVK schema.GroupVersionKind

	// horizontalEventsRetention is the time to keep horizontal events in memory
	// based on scale policies
	horizontalEventsRetention time.Duration

	// customRecommenderConfiguration holds the configuration for custom recommenders,
	// Parsed from annotations on the autoscaler
	customRecommenderConfiguration *RecommenderConfiguration
}

// NewPodAutoscalerInternal creates a new PodAutoscalerInternal from a Kubernetes CR
func NewPodAutoscalerInternal(podAutoscaler *datadoghq.DatadogPodAutoscaler) PodAutoscalerInternal {
	pai := PodAutoscalerInternal{
		namespace: podAutoscaler.Namespace,
		name:      podAutoscaler.Name,
	}
	pai.UpdateFromPodAutoscaler(podAutoscaler)
	pai.UpdateFromStatus(&podAutoscaler.Status)

	return pai
}

// NewPodAutoscalerFromSettings creates a new PodAutoscalerInternal from settings received through remote configuration
func NewPodAutoscalerFromSettings(ns, name string, podAutoscalerSpec *datadoghq.DatadogPodAutoscalerSpec, settingsTimestamp time.Time) PodAutoscalerInternal {
	pda := PodAutoscalerInternal{
		namespace: ns,
		name:      name,
	}
	pda.UpdateFromSettings(podAutoscalerSpec, settingsTimestamp)

	return pda
}

//
// Modifiers
//

// UpdateFromPodAutoscaler updates the PodAutoscalerInternal from a PodAutoscaler object inside K8S
func (p *PodAutoscalerInternal) UpdateFromPodAutoscaler(podAutoscaler *datadoghq.DatadogPodAutoscaler) {
	p.creationTimestamp = podAutoscaler.CreationTimestamp.Time
	p.generation = podAutoscaler.Generation
	p.spec = podAutoscaler.Spec.DeepCopy()
	// Reset the target GVK as it might have changed
	// Resolving the target GVK is done in the controller sync to ensure proper sync and error handling
	p.targetGVK = schema.GroupVersionKind{}
	// Compute the horizontal events retention again in case .Spec.ApplyPolicy has changed
	p.horizontalEventsRetention = getHorizontalEventsRetention(podAutoscaler.Spec.ApplyPolicy, longestScalingRulePeriodAllowed)
	// Compute recommender configuration again in case .Annotations has changed
	p.updateCustomRecommenderConfiguration(podAutoscaler.Annotations)
}

// UpdateFromSettings updates the PodAutoscalerInternal from a new settings
func (p *PodAutoscalerInternal) UpdateFromSettings(podAutoscalerSpec *datadoghq.DatadogPodAutoscalerSpec, settingsTimestamp time.Time) {
	if p.spec == nil || p.spec.RemoteVersion == nil || *p.spec.RemoteVersion != *podAutoscalerSpec.RemoteVersion {
		// Reset the target GVK as it might have changed
		// Resolving the target GVK is done in the controller sync to ensure proper sync and error handling
		p.targetGVK = schema.GroupVersionKind{}
		// Compute the horizontal events retention again in case .Spec.ApplyPolicy has changed
		p.horizontalEventsRetention = getHorizontalEventsRetention(podAutoscalerSpec.ApplyPolicy, longestScalingRulePeriodAllowed)
	}
	// From settings, we don't need to deep copy as the object is not stored anywhere else
	// We store spec all the time to avoid having duplicate memory in the retriever state and here
	p.spec = podAutoscalerSpec
	p.settingsTimestamp = settingsTimestamp
}

// MergeScalingValues updates the PodAutoscalerInternal scaling values based on the desired source of recommendations
func (p *PodAutoscalerInternal) MergeScalingValues(horizontalActiveSource, verticalActiveSource *datadoghqcommon.DatadogPodAutoscalerValueSource) {
	// Helper function to select scaling values based on the source
	selectScalingValues := func(source *datadoghqcommon.DatadogPodAutoscalerValueSource) ScalingValues {
		switch {
		case source == nil:
			return p.scalingValues
		case *source == datadoghqcommon.DatadogPodAutoscalerLocalValueSource:
			return p.fallbackScalingValues
		default:
			return p.mainScalingValues
		}
	}

	// Update scaling values
	p.scalingValues.Horizontal = selectScalingValues(horizontalActiveSource).Horizontal
	p.scalingValues.Vertical = selectScalingValues(verticalActiveSource).Vertical

	// Update error states based on main product recommendations
	p.scalingValues.HorizontalError = p.mainScalingValues.HorizontalError
	p.scalingValues.VerticalError = p.mainScalingValues.VerticalError
	p.scalingValues.Error = p.mainScalingValues.Error
}

// UpdateFromMainValues updates the PodAutoscalerInternal from new main scaling values
func (p *PodAutoscalerInternal) UpdateFromMainValues(mainScalingValues ScalingValues) {
	p.mainScalingValues = mainScalingValues
}

// UpdateFromLocalValues updates the PodAutoscalerInternal from new local scaling values
func (p *PodAutoscalerInternal) UpdateFromLocalValues(fallbackScalingValues ScalingValues) {
	p.fallbackScalingValues = fallbackScalingValues
}

// RemoveValues clears autoscaling values data from the PodAutoscalerInternal as we stopped autoscaling
func (p *PodAutoscalerInternal) RemoveValues() {
	p.scalingValues = ScalingValues{}
}

// RemoveMainValues clears main autoscaling values data from the PodAutoscalerInternal as we stopped autoscaling
func (p *PodAutoscalerInternal) RemoveMainValues() {
	p.mainScalingValues = ScalingValues{}
}

// RemoveLocalValues clears local autoscaling values data from the PodAutoscalerInternal as we stopped autoscaling
func (p *PodAutoscalerInternal) RemoveLocalValues() {
	p.fallbackScalingValues = ScalingValues{}
}

// UpdateFromHorizontalAction updates the PodAutoscalerInternal from a new horizontal action
func (p *PodAutoscalerInternal) UpdateFromHorizontalAction(action *datadoghqcommon.DatadogPodAutoscalerHorizontalAction, err error) {
	if err != nil {
		p.horizontalLastActionError = err
		p.horizontalLastLimitReason = ""
	} else if action != nil {
		p.horizontalLastActionError = nil
	}

	if action != nil {
		replicasChanged := false
		if action.ToReplicas != action.FromReplicas {
			p.horizontalLastActions = addHorizontalAction(action.Time.Time, p.horizontalEventsRetention, p.horizontalLastActions, action)
			replicasChanged = true
		}

		if action.LimitedReason != nil {
			p.horizontalLastLimitReason = *action.LimitedReason
		} else if replicasChanged {
			p.horizontalLastLimitReason = ""
		}
	}
}

// UpdateFromVerticalAction updates the PodAutoscalerInternal from a new vertical action
func (p *PodAutoscalerInternal) UpdateFromVerticalAction(action *datadoghqcommon.DatadogPodAutoscalerVerticalAction, err error) {
	if err != nil {
		p.verticalLastActionError = err
	} else if action != nil {
		p.verticalLastActionError = nil
	}

	if action != nil {
		p.verticalLastAction = action
	}
}

// SetGeneration sets the generation of the PodAutoscaler
func (p *PodAutoscalerInternal) SetGeneration(generation int64) {
	p.generation = generation
}

// SetScaledReplicas sets the current number of replicas for the targetRef matching the resources recommendations
func (p *PodAutoscalerInternal) SetScaledReplicas(replicas int32) {
	p.scaledReplicas = &replicas
}

// SetCurrentReplicas sets the current number of replicas for the targetRef
func (p *PodAutoscalerInternal) SetCurrentReplicas(replicas int32) {
	p.currentReplicas = &replicas
}

// SetError sets an error encountered by the controller not specific to a scaling action
func (p *PodAutoscalerInternal) SetError(err error) {
	p.error = err
}

// SetDeleted flags the PodAutoscaler as deleted
func (p *PodAutoscalerInternal) SetDeleted() {
	p.deleted = true
}

// UpdateFromStatus updates the PodAutoscalerInternal from an existing status.
// It assumes the PodAutoscalerInternal is empty so it's not emptying existing data.
func (p *PodAutoscalerInternal) UpdateFromStatus(status *datadoghqcommon.DatadogPodAutoscalerStatus) {
	if status.Horizontal != nil {
		if status.Horizontal.Target != nil {
			p.scalingValues.Horizontal = &HorizontalScalingValues{
				Source:    status.Horizontal.Target.Source,
				Timestamp: status.Horizontal.Target.GeneratedAt.Time,
				Replicas:  status.Horizontal.Target.Replicas,
			}
		}

		if len(status.Horizontal.LastActions) > 0 {
			p.horizontalLastActions = status.Horizontal.LastActions
		}
	}

	if status.Vertical != nil {
		if status.Vertical.Target != nil {
			p.scalingValues.Vertical = &VerticalScalingValues{
				Source:             status.Vertical.Target.Source,
				Timestamp:          status.Vertical.Target.GeneratedAt.Time,
				ContainerResources: status.Vertical.Target.DesiredResources,
				ResourcesHash:      status.Vertical.Target.Version,
			}
		}

		p.verticalLastAction = status.Vertical.LastAction
	}

	if status.CurrentReplicas != nil {
		p.currentReplicas = status.CurrentReplicas
	}

	// Reading potential errors from conditions. Resetting internal errors first.
	// We're only keeping error string, loosing type, but it's not important for what we do.
	for _, cond := range status.Conditions {
		switch {
		case cond.Type == datadoghqcommon.DatadogPodAutoscalerErrorCondition && cond.Status == corev1.ConditionTrue:
			// Error condition could refer to a controller error or from a general Datadog error
			// We're restoring this to error as it's the most generic
			p.error = errors.New(cond.Reason)
		case cond.Type == datadoghqcommon.DatadogPodAutoscalerHorizontalAbleToRecommendCondition && cond.Status == corev1.ConditionFalse:
			p.scalingValues.HorizontalError = errors.New(cond.Reason)
		case cond.Type == datadoghqcommon.DatadogPodAutoscalerHorizontalAbleToScaleCondition && cond.Status == corev1.ConditionFalse:
			p.horizontalLastActionError = errors.New(cond.Reason)
		case cond.Type == datadoghqcommon.DatadogPodAutoscalerHorizontalScalingLimitedCondition && cond.Status == corev1.ConditionTrue:
			p.horizontalLastLimitReason = cond.Reason
		case cond.Type == datadoghqcommon.DatadogPodAutoscalerVerticalAbleToRecommendCondition && cond.Status == corev1.ConditionFalse:
			p.scalingValues.VerticalError = errors.New(cond.Reason)
		case cond.Type == datadoghqcommon.DatadogPodAutoscalerVerticalAbleToApply && cond.Status == corev1.ConditionFalse:
			p.verticalLastActionError = errors.New(cond.Reason)
		}
	}
}

// UpdateCreationTimestamp updates the timestamp the kubernetes object was created
func (p *PodAutoscalerInternal) UpdateCreationTimestamp(creationTimestamp time.Time) {
	p.creationTimestamp = creationTimestamp
}

//
// Getters
//

// Namespace returns the namespace of the PodAutoscaler
func (p *PodAutoscalerInternal) Namespace() string {
	return p.namespace
}

// Name returns the name of the PodAutoscaler
func (p *PodAutoscalerInternal) Name() string {
	return p.name
}

// ID returns the functional identifier of the PodAutoscaler
func (p *PodAutoscalerInternal) ID() string {
	return p.namespace + "/" + p.name
}

// Generation returns the generation of the PodAutoscaler
func (p *PodAutoscalerInternal) Generation() int64 {
	return p.generation
}

// Spec returns the spec of the PodAutoscaler
func (p *PodAutoscalerInternal) Spec() *datadoghq.DatadogPodAutoscalerSpec {
	return p.spec
}

// SettingsTimestamp returns the timestamp of the last settings update
func (p *PodAutoscalerInternal) SettingsTimestamp() time.Time {
	return p.settingsTimestamp
}

// CreationTimestamp returns the timestamp the kubernetes object was created
func (p *PodAutoscalerInternal) CreationTimestamp() time.Time {
	return p.creationTimestamp
}

// ScalingValues returns the scaling values of the PodAutoscaler
func (p *PodAutoscalerInternal) ScalingValues() ScalingValues {
	return p.scalingValues
}

// MainScalingValues returns the main scaling values of the PodAutoscaler
func (p *PodAutoscalerInternal) MainScalingValues() ScalingValues {
	return p.mainScalingValues
}

// FallbackScalingValues returns the fallback scaling values of the PodAutoscaler
func (p *PodAutoscalerInternal) FallbackScalingValues() ScalingValues {
	return p.fallbackScalingValues
}

// HorizontalLastActions returns the last horizontal actions taken
func (p *PodAutoscalerInternal) HorizontalLastActions() []datadoghqcommon.DatadogPodAutoscalerHorizontalAction {
	return p.horizontalLastActions
}

// HorizontalLastActionError returns the last error encountered on horizontal scaling
func (p *PodAutoscalerInternal) HorizontalLastActionError() error {
	return p.horizontalLastActionError
}

// VerticalLastAction returns the last action taken by the Vertical Pod Autoscaler
func (p *PodAutoscalerInternal) VerticalLastAction() *datadoghqcommon.DatadogPodAutoscalerVerticalAction {
	return p.verticalLastAction
}

// VerticalLastActionError returns the last error encountered on vertical scaling
func (p *PodAutoscalerInternal) VerticalLastActionError() error {
	return p.verticalLastActionError
}

// CurrentReplicas returns the current number of PODs for the targetRef
func (p *PodAutoscalerInternal) CurrentReplicas() *int32 {
	return p.currentReplicas
}

// ScaledReplicas returns the current number of PODs for the targetRef matching the resources recommendations
func (p *PodAutoscalerInternal) ScaledReplicas() *int32 {
	return p.scaledReplicas
}

// Error returns the an error encountered by the controller not specific to a scaling action
func (p *PodAutoscalerInternal) Error() error {
	return p.error
}

// Deleted returns the deletion status of the PodAutoscaler
func (p *PodAutoscalerInternal) Deleted() bool {
	return p.deleted
}

// TargetGVK resolves the GroupVersionKind if empty and returns it
func (p *PodAutoscalerInternal) TargetGVK() (schema.GroupVersionKind, error) {
	if !p.targetGVK.Empty() {
		return p.targetGVK, nil
	}

	gv, err := schema.ParseGroupVersion(p.spec.TargetRef.APIVersion)
	if err != nil || gv.Group == "" || gv.Version == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("failed to parse API version '%s', err: %w", p.spec.TargetRef.APIVersion, err)
	}

	p.targetGVK = schema.GroupVersionKind{
		Group:   gv.Group,
		Version: gv.Version,
		Kind:    p.spec.TargetRef.Kind,
	}
	return p.targetGVK, nil
}

// CustomRecommenderConfiguration returns the configuration set on the autoscaler for a customer recommender
func (p *PodAutoscalerInternal) CustomRecommenderConfiguration() *RecommenderConfiguration {
	return p.customRecommenderConfiguration
}

//
// Helpers
//

// BuildStatus builds the status of the PodAutoscaler from the internal state
func (p *PodAutoscalerInternal) BuildStatus(currentTime metav1.Time, currentStatus *datadoghqcommon.DatadogPodAutoscalerStatus) datadoghqcommon.DatadogPodAutoscalerStatus {
	status := datadoghqcommon.DatadogPodAutoscalerStatus{}

	// Syncing current replicas
	if p.currentReplicas != nil {
		status.CurrentReplicas = p.currentReplicas
	}

	// Produce Horizontal status only if we have a desired number of replicas
	if p.scalingValues.Horizontal != nil {
		status.Horizontal = &datadoghqcommon.DatadogPodAutoscalerHorizontalStatus{
			Target: &datadoghqcommon.DatadogPodAutoscalerHorizontalTargetStatus{
				Source:      p.scalingValues.Horizontal.Source,
				GeneratedAt: metav1.NewTime(p.scalingValues.Horizontal.Timestamp),
				Replicas:    p.scalingValues.Horizontal.Replicas,
			},
		}

		if lenActions := len(p.horizontalLastActions); lenActions > 0 {
			firstIndex := max(lenActions-statusRetainedActions, 0)

			status.Horizontal.LastActions = slices.Clone(p.horizontalLastActions[firstIndex:lenActions])
		}
	}

	// Produce Vertical status only if we have a desired container resources
	if p.scalingValues.Vertical != nil {
		cpuReqSum, memReqSum := p.scalingValues.Vertical.SumCPUMemoryRequests()

		status.Vertical = &datadoghqcommon.DatadogPodAutoscalerVerticalStatus{
			Target: &datadoghqcommon.DatadogPodAutoscalerVerticalTargetStatus{
				Source:           p.scalingValues.Vertical.Source,
				GeneratedAt:      metav1.NewTime(p.scalingValues.Vertical.Timestamp),
				Version:          p.scalingValues.Vertical.ResourcesHash,
				DesiredResources: p.scalingValues.Vertical.ContainerResources,
				Scaled:           p.scaledReplicas,
				PodCPURequest:    cpuReqSum,
				PodMemoryRequest: memReqSum,
			},
			LastAction: p.verticalLastAction,
		}
	}

	// Building conditions
	existingConditions := map[datadoghqcommon.DatadogPodAutoscalerConditionType]*datadoghqcommon.DatadogPodAutoscalerCondition{
		datadoghqcommon.DatadogPodAutoscalerErrorCondition:                     nil,
		datadoghqcommon.DatadogPodAutoscalerActiveCondition:                    nil,
		datadoghqcommon.DatadogPodAutoscalerHorizontalAbleToRecommendCondition: nil,
		datadoghqcommon.DatadogPodAutoscalerHorizontalAbleToScaleCondition:     nil,
		datadoghqcommon.DatadogPodAutoscalerHorizontalScalingLimitedCondition:  nil,
		datadoghqcommon.DatadogPodAutoscalerVerticalAbleToRecommendCondition:   nil,
		datadoghqcommon.DatadogPodAutoscalerVerticalAbleToApply:                nil,
	}

	if currentStatus != nil {
		for i := range currentStatus.Conditions {
			condition := &currentStatus.Conditions[i]
			if _, ok := existingConditions[condition.Type]; ok {
				existingConditions[condition.Type] = condition
			}
		}
	}

	// Building global error condition
	globalError := p.error
	if p.error == nil {
		globalError = p.scalingValues.Error
	}
	status.Conditions = append(status.Conditions, newConditionFromError(true, currentTime, globalError, datadoghqcommon.DatadogPodAutoscalerErrorCondition, existingConditions))

	// Building active condition, should handle multiple reasons, currently only disabled if target replicas = 0
	if p.currentReplicas != nil && *p.currentReplicas == 0 {
		status.Conditions = append(status.Conditions, newCondition(corev1.ConditionFalse, "Target has been scaled to 0 replicas", currentTime, datadoghqcommon.DatadogPodAutoscalerActiveCondition, existingConditions))
	} else {
		status.Conditions = append(status.Conditions, newCondition(corev1.ConditionTrue, "", currentTime, datadoghqcommon.DatadogPodAutoscalerActiveCondition, existingConditions))
	}

	// Building errors related to compute recommendations
	var horizontalAbleToRecommend datadoghqcommon.DatadogPodAutoscalerCondition
	if p.scalingValues.HorizontalError != nil || p.scalingValues.Horizontal != nil {
		horizontalAbleToRecommend = newConditionFromError(false, currentTime, p.scalingValues.HorizontalError, datadoghqcommon.DatadogPodAutoscalerHorizontalAbleToRecommendCondition, existingConditions)
	} else {
		horizontalAbleToRecommend = newCondition(corev1.ConditionUnknown, "", currentTime, datadoghqcommon.DatadogPodAutoscalerHorizontalAbleToRecommendCondition, existingConditions)
	}
	status.Conditions = append(status.Conditions, horizontalAbleToRecommend)

	var verticalAbleToRecommend datadoghqcommon.DatadogPodAutoscalerCondition
	if p.scalingValues.VerticalError != nil || p.scalingValues.Vertical != nil {
		verticalAbleToRecommend = newConditionFromError(false, currentTime, p.scalingValues.VerticalError, datadoghqcommon.DatadogPodAutoscalerVerticalAbleToRecommendCondition, existingConditions)
	} else {
		verticalAbleToRecommend = newCondition(corev1.ConditionUnknown, "", currentTime, datadoghqcommon.DatadogPodAutoscalerVerticalAbleToRecommendCondition, existingConditions)
	}
	status.Conditions = append(status.Conditions, verticalAbleToRecommend)

	// Horizontal: handle scaling limited condition
	if p.horizontalLastLimitReason != "" {
		status.Conditions = append(status.Conditions, newCondition(corev1.ConditionTrue, p.horizontalLastLimitReason, currentTime, datadoghqcommon.DatadogPodAutoscalerHorizontalScalingLimitedCondition, existingConditions))
	} else {
		status.Conditions = append(status.Conditions, newCondition(corev1.ConditionFalse, "", currentTime, datadoghqcommon.DatadogPodAutoscalerHorizontalScalingLimitedCondition, existingConditions))
	}

	// Building rollout errors
	var horizontalReason string
	horizontalStatus := corev1.ConditionUnknown
	if p.horizontalLastActionError != nil {
		horizontalStatus = corev1.ConditionFalse
		horizontalReason = p.horizontalLastActionError.Error()
	} else if len(p.horizontalLastActions) > 0 {
		horizontalStatus = corev1.ConditionTrue
	}
	status.Conditions = append(status.Conditions, newCondition(horizontalStatus, horizontalReason, currentTime, datadoghqcommon.DatadogPodAutoscalerHorizontalAbleToScaleCondition, existingConditions))

	var verticalReason string
	rolloutStatus := corev1.ConditionUnknown
	if p.verticalLastActionError != nil {
		rolloutStatus = corev1.ConditionFalse
		verticalReason = p.verticalLastActionError.Error()
	} else if p.verticalLastAction != nil {
		rolloutStatus = corev1.ConditionTrue
	}
	status.Conditions = append(status.Conditions, newCondition(rolloutStatus, verticalReason, currentTime, datadoghqcommon.DatadogPodAutoscalerVerticalAbleToApply, existingConditions))

	return status
}

// Private helpers
func (p *PodAutoscalerInternal) updateCustomRecommenderConfiguration(annotations map[string]string) {
	annotation, err := parseCustomConfigurationAnnotation(annotations)
	if err != nil {
		p.error = err
		return
	}
	p.customRecommenderConfiguration = annotation
}

func addHorizontalAction(currentTime time.Time, retention time.Duration, actions []datadoghqcommon.DatadogPodAutoscalerHorizontalAction, action *datadoghqcommon.DatadogPodAutoscalerHorizontalAction) []datadoghqcommon.DatadogPodAutoscalerHorizontalAction {
	if retention == 0 {
		actions = actions[:0]
		actions = append(actions, *action)
		return actions
	}

	// Find oldest event index to keep
	cutoffTime := currentTime.Add(-retention)
	cutoffIndex := 0
	for i, action := range actions {
		// The first event after the cutoff time is the oldest event to keep
		if action.Time.Time.After(cutoffTime) {
			cutoffIndex = i
			break
		}
	}

	// We are basically removing space from the array until we reallocate
	actions = actions[cutoffIndex:]
	actions = append(actions, *action)
	return actions
}

func newConditionFromError(trueOnError bool, currentTime metav1.Time, err error, conditionType datadoghqcommon.DatadogPodAutoscalerConditionType, existingConditions map[datadoghqcommon.DatadogPodAutoscalerConditionType]*datadoghqcommon.DatadogPodAutoscalerCondition) datadoghqcommon.DatadogPodAutoscalerCondition {
	var condition corev1.ConditionStatus

	var reason string
	if err != nil {
		reason = err.Error()
		if trueOnError {
			condition = corev1.ConditionTrue
		} else {
			condition = corev1.ConditionFalse
		}
	} else {
		if trueOnError {
			condition = corev1.ConditionFalse
		} else {
			condition = corev1.ConditionTrue
		}
	}

	return newCondition(condition, reason, currentTime, conditionType, existingConditions)
}

func newCondition(status corev1.ConditionStatus, reason string, currentTime metav1.Time, conditionType datadoghqcommon.DatadogPodAutoscalerConditionType, existingConditions map[datadoghqcommon.DatadogPodAutoscalerConditionType]*datadoghqcommon.DatadogPodAutoscalerCondition) datadoghqcommon.DatadogPodAutoscalerCondition {
	condition := datadoghqcommon.DatadogPodAutoscalerCondition{
		Type:   conditionType,
		Status: status,
		Reason: reason,
	}

	prevCondition := existingConditions[conditionType]
	if prevCondition == nil || (prevCondition.Status != condition.Status) {
		condition.LastTransitionTime = currentTime
	} else {
		condition.LastTransitionTime = prevCondition.LastTransitionTime
	}

	return condition
}

func getHorizontalEventsRetention(policy *datadoghq.DatadogPodAutoscalerApplyPolicy, longestLookbackAllowed time.Duration) time.Duration {
	var longestRetention time.Duration
	if policy == nil {
		return 0
	}

	if policy.ScaleUp != nil {
		scaleUpRetention := getLongestScalingRulesPeriod(policy.ScaleUp.Rules)
		scaleUpStabilizationWindow := time.Second * time.Duration(policy.ScaleUp.StabilizationWindowSeconds)
		longestRetention = max(longestRetention, scaleUpRetention, scaleUpStabilizationWindow)
	}

	if policy.ScaleDown != nil {
		scaleDownRetention := getLongestScalingRulesPeriod(policy.ScaleDown.Rules)
		scaleDownStabilizationWindow := time.Second * time.Duration(policy.ScaleDown.StabilizationWindowSeconds)
		longestRetention = max(longestRetention, scaleDownRetention, scaleDownStabilizationWindow)
	}

	if longestRetention > longestLookbackAllowed {
		return longestLookbackAllowed
	}
	return longestRetention
}

func getLongestScalingRulesPeriod(rules []datadoghqcommon.DatadogPodAutoscalerScalingRule) time.Duration {
	var longest time.Duration
	for _, rule := range rules {
		periodDuration := time.Second * time.Duration(rule.PeriodSeconds)
		if periodDuration > longest {
			longest = periodDuration
		}
	}

	return longest
}

func parseCustomConfigurationAnnotation(annotations map[string]string) (*RecommenderConfiguration, error) {
	annotation, ok := annotations[CustomRecommenderAnnotationKey]

	if !ok { // No annotation set
		return nil, nil
	}

	customConfiguration := RecommenderConfiguration{}

	if err := json.Unmarshal([]byte(annotation), &customConfiguration); err != nil {
		return nil, fmt.Errorf("Failed to parse annotations for custom recommender configuration: %v", err)
	}

	return &customConfiguration, nil
}
