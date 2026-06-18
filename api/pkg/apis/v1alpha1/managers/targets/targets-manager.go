/*
 * Copyright (c) Microsoft Corporation.
 * Licensed under the MIT license.
 * SPDX-License-Identifier: MIT
 */

package targets

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eclipse-symphony/symphony/api/constants"
	"github.com/eclipse-symphony/symphony/api/pkg/apis/v1alpha1/model"
	"github.com/eclipse-symphony/symphony/api/pkg/apis/v1alpha1/validation"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/contexts"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/managers"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/observability"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/providers"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/providers/registry"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/providers/states"
	"github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/utils"
	"github.com/eclipse-symphony/symphony/coa/pkg/logger"

	observ_utils "github.com/eclipse-symphony/symphony/coa/pkg/apis/v1alpha2/observability/utils"
)

var log = logger.NewLogger("coa.runtime")

type TargetsManager struct {
	managers.Manager
	StateProvider    states.IStateProvider
	RegistryProvider registry.IRegistryProvider
	needValidate     bool
	TargetValidator  validation.TargetValidator
}

func (s *TargetsManager) Init(context *contexts.VendorContext, config managers.ManagerConfig, providers map[string]providers.IProvider) error {
	err := s.Manager.Init(context, config, providers)
	if err != nil {
		return err
	}
	stateprovider, err := managers.GetPersistentStateProvider(config, providers)
	if err == nil {
		s.StateProvider = stateprovider
	} else {
		return err
	}
	s.needValidate = managers.NeedObjectValidate(config, providers)
	if s.needValidate {
		// Turn off validation of differnt types: https://github.com/eclipse-symphony/symphony/issues/445
		// s.TargetValidator = validation.NewTargetValidator(s.targetInstanceLookup, s.targetUniqueNameLookup)
		s.TargetValidator = validation.NewTargetValidator(nil, s.targetUniqueNameLookup)
	}
	if err = s.loadStaticTargets(stdctx.Background(), config.Properties); err != nil {
		return err
	}
	return nil
}

func (t *TargetsManager) loadStaticTargets(ctx stdctx.Context, properties map[string]string) error {
	targetFiles := make([]string, 0)
	if raw := strings.TrimSpace(properties["staticTargets"]); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if path := strings.TrimSpace(part); path != "" {
				targetFiles = append(targetFiles, path)
			}
		}
	}
	if raw := strings.TrimSpace(properties["staticTargetsDir"]); raw != "" {
		entries, err := os.ReadDir(raw)
		if err != nil {
			return fmt.Errorf("failed to read static targets directory %q: %w", raw, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			targetFiles = append(targetFiles, filepath.Join(raw, entry.Name()))
		}
	}
	for _, path := range targetFiles {
		if err := t.loadStaticTargetsFile(ctx, path); err != nil {
			return err
		}
	}
	return nil
}

func (t *TargetsManager) loadStaticTargetsFile(ctx stdctx.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read static target file %q: %w", path, err)
	}
	targets := make([]model.TargetState, 0)
	if err = json.Unmarshal(data, &targets); err != nil {
		var target model.TargetState
		if err = json.Unmarshal(data, &target); err != nil {
			return fmt.Errorf("failed to parse static target file %q: %w", path, err)
		}
		targets = append(targets, target)
	}
	for _, target := range targets {
		if target.ObjectMeta.Name == "" {
			return fmt.Errorf("static target file %q contains a target without metadata.name", path)
		}
		if err = t.UpsertState(ctx, target.ObjectMeta.Name, target); err != nil {
			return fmt.Errorf("failed to load static target %q from %q: %w", target.ObjectMeta.Name, path, err)
		}
		log.InfofCtx(ctx, " M (Targets): loaded static target %q from %s", target.ObjectMeta.Name, path)
	}
	return nil
}

func (t *TargetsManager) DeleteSpec(ctx stdctx.Context, name string, namespace string) error {
	ctx, span := observability.StartSpan("Targets Manager", ctx, &map[string]string{
		"method": "DeleteSpec",
	})
	var err error = nil
	defer observ_utils.CloseSpanWithError(span, &err)
	defer observ_utils.EmitUserDiagnosticsLogs(ctx, &err)

	if t.needValidate {
		if err = t.ValidateDelete(ctx, name, namespace); err != nil {
			return err
		}
	}

	err = t.StateProvider.Delete(ctx, states.DeleteRequest{
		ID: name,
		Metadata: map[string]interface{}{
			"namespace": namespace,
			"group":     model.FabricGroup,
			"version":   "v1",
			"resource":  "targets",
			"kind":      "Target",
		},
	})
	return err
}

func (t *TargetsManager) UpsertState(ctx stdctx.Context, name string, state model.TargetState) error {
	ctx, span := observability.StartSpan("Targets Manager", ctx, &map[string]string{
		"method": "UpsertSpec",
	})
	var err error = nil
	defer observ_utils.CloseSpanWithError(span, &err)
	defer observ_utils.EmitUserDiagnosticsLogs(ctx, &err)

	if state.ObjectMeta.Name != "" && state.ObjectMeta.Name != name {
		return v1alpha2.NewCOAError(nil, fmt.Sprintf("Name in metadata (%s) does not match name in request (%s)", state.ObjectMeta.Name, name), v1alpha2.BadRequest)
	}
	state.ObjectMeta.FixNames(name)

	oldState, getStateErr := t.GetState(ctx, state.ObjectMeta.Name, state.ObjectMeta.Namespace)
	if getStateErr == nil {
		state.ObjectMeta.PreserveSystemMetadata(oldState.ObjectMeta)
	}

	if t.needValidate {
		if state.ObjectMeta.Labels == nil {
			state.ObjectMeta.Labels = make(map[string]string)
		}
		if state.Spec != nil {
			state.ObjectMeta.Labels[constants.DisplayName] = utils.ConvertStringToValidLabel(state.Spec.DisplayName)
		}
		if err = validation.ValidateCreateOrUpdateWrapper(ctx, &t.TargetValidator, state, oldState, getStateErr); err != nil {
			return err
		}
	}

	body := map[string]interface{}{
		"apiVersion": model.FabricGroup + "/v1",
		"kind":       "Target",
		"metadata":   state.ObjectMeta,
		"spec":       state.Spec,
	}

	upsertRequest := states.UpsertRequest{
		Value: states.StateEntry{
			ID:   name,
			Body: body,
			ETag: state.ObjectMeta.ETag,
		},
		Metadata: map[string]interface{}{
			"namespace": state.ObjectMeta.Namespace,
			"group":     model.FabricGroup,
			"version":   "v1",
			"resource":  "targets",
			"kind":      "Target",
		},
	}

	_, err = t.StateProvider.Upsert(ctx, upsertRequest)
	return err
}

// Caller need to explicitly set namespace in current.Metadata!
func (t *TargetsManager) ReportState(ctx stdctx.Context, current model.TargetState) (model.TargetState, error) {
	ctx, span := observability.StartSpan("Targets Manager", ctx, &map[string]string{
		"method": "ReportState",
	})
	var err error = nil
	defer observ_utils.CloseSpanWithError(span, &err)
	defer observ_utils.EmitUserDiagnosticsLogs(ctx, &err)

	getRequest := states.GetRequest{
		ID: current.ObjectMeta.Name,
		Metadata: map[string]interface{}{
			"version":   "v1",
			"group":     model.FabricGroup,
			"resource":  "targets",
			"namespace": current.ObjectMeta.Namespace,
		},
	}

	var target states.StateEntry
	target, err = t.StateProvider.Get(ctx, getRequest)
	if err != nil {
		return model.TargetState{}, err
	}

	var targetState model.TargetState
	bytes, _ := json.Marshal(target.Body)
	err = json.Unmarshal(bytes, &targetState)
	if err != nil {
		return model.TargetState{}, err
	}

	for k, v := range current.Status.Properties {
		if targetState.Status.Properties == nil {
			targetState.Status.Properties = make(map[string]string)
		}
		targetState.Status.Properties[k] = v
	}
	targetState.Status.LastModified = current.Status.LastModified

	target.Body = targetState

	updateRequest := states.UpsertRequest{
		Value: target,
		Metadata: map[string]interface{}{
			"version":   "v1",
			"group":     model.FabricGroup,
			"resource":  "targets",
			"namespace": current.ObjectMeta.Namespace,
			"kind":      "Target",
		},
		Options: states.UpsertOption{
			UpdateStatusOnly: true,
		},
	}

	_, err = t.StateProvider.Upsert(ctx, updateRequest)
	if err != nil {
		return model.TargetState{}, err
	}
	return targetState, nil
}
func (t *TargetsManager) ListState(ctx stdctx.Context, namespace string) ([]model.TargetState, error) {
	ctx, span := observability.StartSpan("Targets Manager", ctx, &map[string]string{
		"method": "ListSpec",
	})
	var err error = nil
	defer observ_utils.CloseSpanWithError(span, &err)
	defer observ_utils.EmitUserDiagnosticsLogs(ctx, &err)

	listRequest := states.ListRequest{
		Metadata: map[string]interface{}{
			"version":   "v1",
			"group":     model.FabricGroup,
			"resource":  "targets",
			"namespace": namespace,
			"kind":      "Target",
		},
	}
	var targets []states.StateEntry
	targets, _, err = t.StateProvider.List(ctx, listRequest)
	if err != nil {
		return nil, err
	}
	ret := make([]model.TargetState, 0)
	for _, t := range targets {
		var rt model.TargetState
		rt, err = getTargetState(t.Body)
		if err != nil {
			return nil, err
		}
		rt.ObjectMeta.UpdateEtag(t.ETag)
		ret = append(ret, rt)
	}
	return ret, nil
}

func getTargetState(body interface{}) (model.TargetState, error) {
	var targetState model.TargetState
	bytes, _ := json.Marshal(body)
	err := json.Unmarshal(bytes, &targetState)
	if err != nil {
		return model.TargetState{}, err
	}
	if targetState.Spec == nil {
		targetState.Spec = &model.TargetSpec{}
	}
	return targetState, nil
}

func (t *TargetsManager) GetState(ctx stdctx.Context, id string, namespace string) (model.TargetState, error) {
	ctx, span := observability.StartSpan("Targets Manager", ctx, &map[string]string{
		"method": "GetSpec",
	})
	var err error = nil
	defer observ_utils.CloseSpanWithError(span, &err)
	defer observ_utils.EmitUserDiagnosticsLogs(ctx, &err)

	getRequest := states.GetRequest{
		ID: id,
		Metadata: map[string]interface{}{
			"version":   "v1",
			"group":     model.FabricGroup,
			"resource":  "targets",
			"namespace": namespace,
			"kind":      "Target",
		},
	}
	var target states.StateEntry
	target, err = t.StateProvider.Get(ctx, getRequest)
	if err != nil {
		return model.TargetState{}, err
	}

	var ret model.TargetState
	ret, err = getTargetState(target.Body)
	if err != nil {
		return model.TargetState{}, err
	}
	ret.ObjectMeta.UpdateEtag(target.ETag)
	return ret, nil
}

func (t *TargetsManager) ValidateDelete(ctx stdctx.Context, name string, namespace string) error {
	state, err := t.GetState(ctx, name, namespace)
	return validation.ValidateDeleteWrapper(ctx, &t.TargetValidator, state, err)
}

func (t *TargetsManager) targetUniqueNameLookup(ctx stdctx.Context, displayName string, namespace string) (interface{}, error) {
	return states.GetObjectStateWithUniqueName(ctx, t.StateProvider, validation.Target, displayName, namespace)
}

func (t *TargetsManager) targetInstanceLookup(ctx stdctx.Context, name string, namespace string) (bool, error) {
	instanceList, err := states.ListObjectStateWithLabels(ctx, t.StateProvider, validation.Instance, namespace, map[string]string{constants.Target: name}, 1)
	if err != nil {
		return false, err
	}
	return len(instanceList) > 0, nil
}
