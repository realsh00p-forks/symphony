/*
 * Copyright (c) Microsoft Corporation and others.
 * Licensed under the MIT license.
 * SPDX-License-Identifier: MIT
 */

package sovd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/eclipse-symphony/symphony/api/pkg/apis/v1alpha1/model"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/contexts"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/providers"
	"github.com/eclipse-symphony/symphony/coa/pkg/logger"
)

const (
	loggerName   = "providers.target.sovd"
	providerName = "P (SOVD Target)"
)

var sLog = logger.NewLogger(loggerName)

type SOVDTargetProviderConfig struct {
	Endpoint  string `json:"endpoint"`
	Component string `json:"component"`
	Timeout   int    `json:"timeout"`
}

type SOVDTargetProvider struct {
	Config  SOVDTargetProviderConfig
	Context *contexts.ManagerContext
	client  *http.Client
}

type sovdReadResponse struct {
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

type ankaiosState struct {
	DesiredState struct {
		Workloads map[string]map[string]interface{} `json:"workloads"`
	} `json:"desiredState"`
}

func (s *SOVDTargetProvider) SetContext(ctx *contexts.ManagerContext) {
	s.Context = ctx
}

func (s *SOVDTargetProvider) InitWithMap(properties map[string]string) error {
	config := SOVDTargetProviderConfig{
		Endpoint:  properties["endpoint"],
		Component: properties["component"],
	}
	if timeout, ok := properties["timeout"]; ok {
		fmt.Sscanf(timeout, "%d", &config.Timeout)
	}
	return s.Init(config)
}

func (s *SOVDTargetProvider) Init(config providers.IProviderConfig) error {
	cfg, err := toSOVDTargetProviderConfig(config)
	if err != nil {
		return err
	}
	if cfg.Endpoint == "" {
		return errors.New("SOVD endpoint is required")
	}
	if cfg.Component == "" {
		cfg.Component = "ankaios"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30
	}
	s.Config = cfg
	s.client = &http.Client{Timeout: time.Duration(cfg.Timeout) * time.Second}
	return nil
}

func toSOVDTargetProviderConfig(config providers.IProviderConfig) (SOVDTargetProviderConfig, error) {
	ret := SOVDTargetProviderConfig{}
	data, err := json.Marshal(config)
	if err != nil {
		return ret, err
	}
	err = json.Unmarshal(data, &ret)
	return ret, err
}

func (s *SOVDTargetProvider) GetValidationRule(ctx context.Context) model.ValidationRule {
	return model.ValidationRule{
		AllowSidecar: false,
		ComponentValidationRule: model.ComponentValidationRule{
			RequiredProperties: []string{
				"ankaios.agent",
				"ankaios.runtime",
				"ankaios.runtimeConfig",
			},
			OptionalProperties: []string{
				"ankaios.restartPolicy",
				"ankaios.dependencies",
			},
			RequiredComponentType: "",
			RequiredMetadata:      []string{},
			OptionalMetadata:      []string{},
		},
	}
}

func (s *SOVDTargetProvider) Get(ctx context.Context, deployment model.DeploymentSpec, references []model.ComponentStep) ([]model.ComponentSpec, error) {
	sLog.InfofCtx(ctx, "  P (SOVD Target): getting artifacts: %s - %s", deployment.Instance.Spec.Scope, deployment.Instance.ObjectMeta.Name)

	var state ankaiosState
	if err := s.readData(ctx, "ankaios.state", &state); err != nil {
		return nil, err
	}

	ret := make([]model.ComponentSpec, 0)
	for _, ref := range references {
		workload, ok := state.DesiredState.Workloads[ref.Component.Name]
		if !ok {
			continue
		}
		component := ref.Component
		props := component.Properties
		if props == nil {
			props = map[string]interface{}{}
		}
		copyStringProperty(props, workload, "ankaios.agent", "agent")
		copyStringProperty(props, workload, "ankaios.runtime", "runtime")
		copyStringProperty(props, workload, "ankaios.restartPolicy", "restartPolicy")
		copyStringProperty(props, workload, "ankaios.runtimeConfig", "runtimeConfig")
		component.Properties = props
		ret = append(ret, component)
	}
	return ret, nil
}

func (s *SOVDTargetProvider) Apply(ctx context.Context, deployment model.DeploymentSpec, step model.DeploymentStep, isDryRun bool) (map[string]model.ComponentResultSpec, error) {
	sLog.InfofCtx(ctx, "  P (SOVD Target): applying artifacts: %s - %s", deployment.Instance.Spec.Scope, deployment.Instance.ObjectMeta.Name)

	ret := step.PrepareResultMap()
	if err := s.GetValidationRule(ctx).Validate(step.GetUpdatedComponents()); err != nil {
		return ret, err
	}
	if isDryRun {
		return ret, nil
	}

	updates := map[string]interface{}{}
	deletes := map[string]interface{}{}
	for _, component := range step.Components {
		switch component.Action {
		case model.ComponentUpdate:
			updates[component.Component.Name] = workloadFromComponent(component.Component)
		case model.ComponentDelete:
			deletes[component.Component.Name] = workloadFromComponent(component.Component)
		}
	}

	if len(updates) > 0 {
		if err := s.writeData(ctx, "ankaios.applyState", manifest(updates)); err != nil {
			markResults(ret, step.Components, model.ComponentUpdate, v1alpha2.UpdateFailed, err.Error())
			return ret, err
		}
		markResults(ret, step.Components, model.ComponentUpdate, v1alpha2.OK, "Component applied successfully through SOVD")
	}
	if len(deletes) > 0 {
		if err := s.writeData(ctx, "ankaios.deleteState", manifest(deletes)); err != nil {
			markResults(ret, step.Components, model.ComponentDelete, v1alpha2.UpdateFailed, err.Error())
			return ret, err
		}
		markResults(ret, step.Components, model.ComponentDelete, v1alpha2.OK, "Component deleted successfully through SOVD")
	}
	return ret, nil
}

func (s *SOVDTargetProvider) Commit(ctx context.Context, deployment model.DeploymentSpec) error {
	sLog.InfofCtx(ctx, "  P (SOVD Target): committing artifacts: %s - %s", deployment.Instance.Spec.Scope, deployment.Instance.ObjectMeta.Name)
	return s.writeData(ctx, "ankaios.commitState", map[string]interface{}{"value": true})
}

func (s *SOVDTargetProvider) dataURL(dataID string) string {
	base := strings.TrimRight(s.Config.Endpoint, "/")
	return fmt.Sprintf("%s/components/%s/data/%s", base, s.Config.Component, dataID)
}

func (s *SOVDTargetProvider) readData(ctx context.Context, dataID string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.dataURL(dataID), nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("SOVD read %s failed with status %d: %s", dataID, resp.StatusCode, string(body))
	}
	var envelope sovdReadResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	return json.Unmarshal(envelope.Data, out)
}

func (s *SOVDTargetProvider) writeData(ctx context.Context, dataID string, value interface{}) error {
	body, err := json.Marshal(map[string]interface{}{"data": value})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.dataURL(dataID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("SOVD write %s failed with status %d: %s", dataID, resp.StatusCode, string(respBody))
	}
	return nil
}

func manifest(workloads map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"workloads":  workloads,
	}
}

func workloadFromComponent(component model.ComponentSpec) map[string]interface{} {
	props := component.Properties
	workload := map[string]interface{}{
		"agent":         model.ReadPropertyCompat(props, "ankaios.agent", nil),
		"runtime":       model.ReadPropertyCompat(props, "ankaios.runtime", nil),
		"runtimeConfig": model.ReadPropertyCompat(props, "ankaios.runtimeConfig", nil),
	}
	restartPolicy := model.ReadPropertyCompat(props, "ankaios.restartPolicy", nil)
	if restartPolicy == "" {
		restartPolicy = "NEVER"
	}
	workload["restartPolicy"] = restartPolicy
	if dependencies := parseDependencies(model.ReadPropertyCompat(props, "ankaios.dependencies", nil)); len(dependencies) > 0 {
		workload["dependencies"] = dependencies
	}
	return workload
}

func parseDependencies(value string) map[string]string {
	ret := map[string]string{}
	for _, line := range strings.FieldsFunc(value, func(r rune) bool { return r == '\n' || r == ',' }) {
		name, condition, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		condition = strings.TrimSpace(condition)
		if name != "" && condition != "" {
			ret[name] = condition
		}
	}
	return ret
}

func copyStringProperty(props map[string]interface{}, workload map[string]interface{}, propName string, workloadName string) {
	if value, ok := workload[workloadName].(string); ok {
		props[propName] = value
	}
}

func markResults(results map[string]model.ComponentResultSpec, components []model.ComponentStep, action model.ComponentAction, status v1alpha2.State, message string) {
	for _, component := range components {
		if component.Action == action {
			results[component.Component.Name] = model.ComponentResultSpec{
				Status:  status,
				Message: message,
			}
		}
	}
}
