/*
Copyright 2019 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"fmt"
	"strings"

	"github.com/tektoncd/pipeline/pkg/apis/validate"
	"github.com/tektoncd/pipeline/pkg/list"
	"github.com/tektoncd/pipeline/pkg/reconciler/pipeline/dag"
	"github.com/tektoncd/pipeline/pkg/substitution"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"
	"knative.dev/pkg/apis"
)

var _ apis.Validatable = (*Pipeline)(nil)

// Validate checks that the Pipeline structure is valid but does not validate
// that any references resources exist, that is done at run time.
func (p *Pipeline) Validate(ctx context.Context) *apis.FieldError {
	if err := validate.ObjectMetadata(p.GetObjectMeta()); err != nil {
		return err.ViaField("metadata")
	}
	return p.Spec.Validate(ctx)
}

func validateDeclaredResources(ps *PipelineSpec) error {
	required := []string{}
	for _, t := range ps.Tasks {
		if t.Resources != nil {
			for _, input := range t.Resources.Inputs {
				required = append(required, input.Resource)
			}
			for _, output := range t.Resources.Outputs {
				required = append(required, output.Resource)
			}
		}

		for _, condition := range t.Conditions {
			for _, cr := range condition.Resources {
				required = append(required, cr.Resource)
			}
		}
	}

	provided := make([]string, 0, len(ps.Resources))
	for _, resource := range ps.Resources {
		provided = append(provided, resource.Name)
	}
	missing := list.DiffLeft(required, provided)
	if len(missing) > 0 {
		return fmt.Errorf("pipeline declared resources didn't match usage in Tasks: Didn't provide required values: %s", missing)
	}

	return nil
}

func isOutput(outputs []PipelineTaskOutputResource, resource string) bool {
	for _, output := range outputs {
		if output.Resource == resource {
			return true
		}
	}
	return false
}

// validateFrom ensures that the `from` values make sense: that they rely on values from Tasks
// that ran previously, and that the PipelineResource is actually an output of the Task it should come from.
func validateFrom(tasks []PipelineTask) error {
	taskOutputs := map[string][]PipelineTaskOutputResource{}
	for _, task := range tasks {
		var to []PipelineTaskOutputResource
		if task.Resources != nil {
			to = make([]PipelineTaskOutputResource, len(task.Resources.Outputs))
			copy(to, task.Resources.Outputs)
		}
		taskOutputs[task.Name] = to
	}
	for _, t := range tasks {
		inputResources := []PipelineTaskInputResource{}
		if t.Resources != nil {
			inputResources = append(inputResources, t.Resources.Inputs...)
		}

		for _, c := range t.Conditions {
			inputResources = append(inputResources, c.Resources...)
		}

		for _, rd := range inputResources {
			for _, pt := range rd.From {
				outputs, found := taskOutputs[pt]
				if !found {
					return fmt.Errorf("expected resource %s to be from task %s, but task %s doesn't exist", rd.Resource, pt, pt)
				}
				if !isOutput(outputs, rd.Resource) {
					return fmt.Errorf("the resource %s from %s must be an output but is an input", rd.Resource, pt)
				}
			}
		}
	}
	return nil
}

// validateGraph ensures the Pipeline's dependency Graph (DAG) make sense: that there is no dependency
// cycle or that they rely on values from Tasks that ran previously, and that the PipelineResource
// is actually an output of the Task it should come from.
func validateGraph(tasks []PipelineTask) error {
	if _, err := dag.Build(PipelineTaskList(tasks)); err != nil {
		return err
	}
	return nil
}

// Validate checks that taskNames in the Pipeline are valid and that the graph
// of Tasks expressed in the Pipeline makes sense.
func (ps *PipelineSpec) Validate(ctx context.Context) *apis.FieldError {
	if equality.Semantic.DeepEqual(ps, &PipelineSpec{}) {
		return apis.ErrMissingField(apis.CurrentField)
	}

	// Names cannot be duplicated
	taskNames := map[string]struct{}{}
	for i, t := range ps.Tasks {
		// can't have both taskRef and taskSpec at the same time
		if (t.TaskRef != nil && t.TaskRef.Name != "") && t.TaskSpec != nil {
			return apis.ErrMultipleOneOf(fmt.Sprintf("spec.tasks[%d].taskRef", i), fmt.Sprintf("spec.tasks[%d].taskSpec", i))
		}
		// Check that one of TaskRef and TaskSpec is present
		if (t.TaskRef == nil || (t.TaskRef != nil && t.TaskRef.Name == "")) && t.TaskSpec == nil {
			return apis.ErrMissingOneOf(fmt.Sprintf("spec.tasks[%d].taskRef", i), fmt.Sprintf("spec.tasks[%d].taskSpec", i))
		}
		// Validate TaskSpec if it's present
		if t.TaskSpec != nil {
			if err := t.TaskSpec.Validate(ctx); err != nil {
				return err
			}
		}
		if t.TaskRef != nil && t.TaskRef.Name != "" {
			// Task names are appended to the container name, which must exist and
			// must be a valid k8s name
			if errSlice := validation.IsQualifiedName(t.Name); len(errSlice) != 0 {
				return apis.ErrInvalidValue(strings.Join(errSlice, ","), fmt.Sprintf("spec.tasks[%d].name", i))
			}
			// TaskRef name must be a valid k8s name
			if errSlice := validation.IsQualifiedName(t.TaskRef.Name); len(errSlice) != 0 {
				return apis.ErrInvalidValue(strings.Join(errSlice, ","), fmt.Sprintf("spec.tasks[%d].taskRef.name", i))
			}
			if _, ok := taskNames[t.Name]; ok {
				return apis.ErrMultipleOneOf(fmt.Sprintf("spec.tasks[%d].name", i))
			}
			taskNames[t.Name] = struct{}{}
		}
	}

	// All declared resources should be used, and the Pipeline shouldn't try to use any resources
	// that aren't declared
	if err := validateDeclaredResources(ps); err != nil {
		return apis.ErrInvalidValue(err.Error(), "spec.resources")
	}

	// The from values should make sense
	if err := validateFrom(ps.Tasks); err != nil {
		return apis.ErrInvalidValue(err.Error(), "spec.tasks.resources.inputs.from")
	}

	// Validate the pipeline task graph
	if err := validateGraph(ps.Tasks); err != nil {
		return apis.ErrInvalidValue(err.Error(), "spec.tasks")
	}

	// The parameter variables should be valid
	if err := validatePipelineParameterVariables(ps.Tasks, ps.Params); err != nil {
		return err
	}

	// Validate the pipeline's workspaces.
	if err := validatePipelineWorkspaces(ps.Workspaces, ps.Tasks); err != nil {
		return err
	}

	return nil
}

func validatePipelineWorkspaces(wss []WorkspacePipelineDeclaration, pts []PipelineTask) *apis.FieldError {
	// Workspace names must be non-empty and unique.
	wsTable := make(map[string]struct{})
	for i, ws := range wss {
		if ws.Name == "" {
			return apis.ErrInvalidValue(fmt.Sprintf("workspace %d has empty name", i), "spec.workspaces")
		}
		if _, ok := wsTable[ws.Name]; ok {
			return apis.ErrInvalidValue(fmt.Sprintf("workspace with name %q appears more than once", ws.Name), "spec.workspaces")
		}
		wsTable[ws.Name] = struct{}{}
	}

	// Any workspaces used in PipelineTasks should have their name declared in the Pipeline's
	// Workspaces list.
	for ptIdx, pt := range pts {
		for wsIdx, ws := range pt.Workspaces {
			if _, ok := wsTable[ws.Workspace]; !ok {
				return apis.ErrInvalidValue(
					fmt.Sprintf("pipeline task %q expects workspace with name %q but none exists in pipeline spec", pt.Name, ws.Workspace),
					fmt.Sprintf("spec.tasks[%d].workspaces[%d]", ptIdx, wsIdx),
				)
			}
		}
	}
	return nil
}

func validatePipelineParameterVariables(tasks []PipelineTask, params []ParamSpec) *apis.FieldError {
	parameterNames := map[string]struct{}{}
	arrayParameterNames := map[string]struct{}{}

	for _, p := range params {
		// Verify that p is a valid type.
		validType := false
		for _, allowedType := range AllParamTypes {
			if p.Type == allowedType {
				validType = true
			}
		}
		if !validType {
			return apis.ErrInvalidValue(string(p.Type), fmt.Sprintf("spec.params.%s.type", p.Name))
		}

		// If a default value is provided, ensure its type matches param's declared type.
		if (p.Default != nil) && (p.Default.Type != p.Type) {
			return &apis.FieldError{
				Message: fmt.Sprintf(
					"\"%v\" type does not match default value's type: \"%v\"", p.Type, p.Default.Type),
				Paths: []string{
					fmt.Sprintf("spec.params.%s.type", p.Name),
					fmt.Sprintf("spec.params.%s.default.type", p.Name),
				},
			}
		}

		// Add parameter name to parameterNames, and to arrayParameterNames if type is array.
		parameterNames[p.Name] = struct{}{}
		if p.Type == ParamTypeArray {
			arrayParameterNames[p.Name] = struct{}{}
		}
	}

	return validatePipelineVariables(tasks, "params", parameterNames, arrayParameterNames)
}

func validatePipelineVariables(tasks []PipelineTask, prefix string, paramNames map[string]struct{}, arrayParamNames map[string]struct{}) *apis.FieldError {
	for _, task := range tasks {
		for _, param := range task.Params {
			if param.Value.Type == ParamTypeString {
				if err := validatePipelineVariable(fmt.Sprintf("param[%s]", param.Name), param.Value.StringVal, prefix, paramNames); err != nil {
					return err
				}
				if err := validatePipelineNoArrayReferenced(fmt.Sprintf("param[%s]", param.Name), param.Value.StringVal, prefix, arrayParamNames); err != nil {
					return err
				}
			} else {
				for _, arrayElement := range param.Value.ArrayVal {
					if err := validatePipelineVariable(fmt.Sprintf("param[%s]", param.Name), arrayElement, prefix, paramNames); err != nil {
						return err
					}
					if err := validatePipelineArraysIsolated(fmt.Sprintf("param[%s]", param.Name), arrayElement, prefix, arrayParamNames); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func validatePipelineVariable(name, value, prefix string, vars map[string]struct{}) *apis.FieldError {
	return substitution.ValidateVariable(name, value, prefix, "", "task parameter", "pipelinespec.params", vars)
}

func validatePipelineNoArrayReferenced(name, value, prefix string, vars map[string]struct{}) *apis.FieldError {
	return substitution.ValidateVariableProhibited(name, value, prefix, "", "task parameter", "pipelinespec.params", vars)
}

func validatePipelineArraysIsolated(name, value, prefix string, vars map[string]struct{}) *apis.FieldError {
	return substitution.ValidateVariableIsolated(name, value, prefix, "", "task parameter", "pipelinespec.params", vars)
}
