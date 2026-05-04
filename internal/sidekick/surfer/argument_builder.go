// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package surfer

import (
	"fmt"
	"slices"
	"strings"

	"github.com/googleapis/librarian/internal/sidekick/api"
	"github.com/googleapis/librarian/internal/sidekick/surfer/provider"
	"github.com/iancoleman/strcase"
)

// ArgumentContext contains the context required to build a command-line argument.
type ArgumentContext struct {
	Method    *api.Method
	Overrides *provider.Config
	Model     *api.API
	Service   *api.Service
	Field     *api.Field
	APIField  []string
}

// buildArgument creates a single command-line argument (an `Argument` struct) from the context.
// It returns nil if the field should be ignored.
func buildArgument(ctx *ArgumentContext) (*Argument, error) {
	if isArgIgnored(ctx.Field, ctx.Method) {
		return nil, nil
	}

	// TODO(https://github.com/googleapis/librarian/issues/3414): Abstract away casing logic in the model.
	arg := &Argument{
		ArgName:   ctx.Field.Name,
		APIField:  ctx.APIField,
		Required:  ctx.Field.DocumentAsRequired(),
		Repeated:  repeated(ctx.Field),
		Clearable: clearable(ctx.Field, ctx.Method),
		HelpText:  argumentHelpText(ctx.Overrides, ctx.Field),
	}

	if ctx.Field.ResourceReference != nil {
		spec, err := resourceReferenceSpec(ctx)
		if err != nil {
			return nil, err
		}
		arg.ResourceSpec = spec
	} else if ctx.Field.Map {
		arg.Spec = mapSpec()
	} else if ctx.Field.EnumType != nil {
		arg.Choices = choices(ctx.Field)
	} else {
		arg.Type = provider.GetGcloudType(ctx.Field.Typez)
		if ctx.Field.Typez == api.TypezBool {
			if provider.IsUpdate(ctx.Method) {
				arg.Action = "store_true_false"
			} else {
				arg.Action = "store_true"
			}
		}
	}

	return arg, nil
}

func isArgIgnored(field *api.Field, method *api.Method) bool {
	if field.Name == "update_mask" {
		return true
	}
	if provider.IsList(method) {
		switch field.Name {
		case "page_size", "page_token", "filter", "order_by":
			return true
		case "return_partial_success":
			// Field is available in all APIs due to mixin but not all APIs actually
			// support it. Omitting for now.
			return provider.IsOperationsMethod(method)
		}
	}
	if slices.Contains(field.Behavior, api.FieldBehaviorOutputOnly) {
		return true
	}
	if provider.IsUpdate(method) && slices.Contains(field.Behavior, api.FieldBehaviorImmutable) {
		return true
	}
	return false
}

func repeated(field *api.Field) bool {
	return field.Repeated || field.Map
}

func clearable(field *api.Field, method *api.Method) bool {
	return provider.IsUpdate(method) && repeated(field)
}

func argumentHelpText(overrides *provider.Config, field *api.Field) string {
	return provider.GetFieldHelpText(overrides, field)
}

func choices(field *api.Field) []Choice {
	var choices []Choice
	for _, v := range field.EnumType.Values {
		// Skip the default "UNSPECIFIED" value.
		if !strings.HasSuffix(v.Name, "_UNSPECIFIED") {
			choices = append(choices, Choice{
				ArgValue:  strcase.ToKebab(v.Name),
				EnumValue: v.Name,
				HelpText:  fmt.Sprintf("Value for the `%s` field.", strcase.ToKebab(v.Name)),
			})
		}
	}
	return choices
}

func mapSpec() []ArgSpec {
	return []ArgSpec{{APIField: "key"}, {APIField: "value"}}
}

// buildPrimaryResourceArgument creates the main positional resource argument for a command.
// This is the argument that represents the resource being acted upon (e.g., the instance name).
func buildPrimaryResourceArgument(ctx *ArgumentContext, idField *api.Field) Argument {
	resource := provider.GetResourceForMethod(ctx.Method, ctx.Model)
	var segments []api.PathSegment
	// TODO(https://github.com/googleapis/librarian/issues/3415): Support multiple resource patterns and multitype resources.
	if resource != nil && len(resource.Patterns) > 0 {
		segments = resource.Patterns[0]
	}

	// Grab the parent if it is collection based method unless you have a resource id field.
	if provider.IsCollectionMethod(ctx.Method) && idField == nil {
		segments = provider.GetParentFromSegments(segments)
	}

	// resourceName should always be GetSingularFromSegments.
	resourceName := provider.GetSingularFromSegments(segments)

	// Help text should be documentation of builder.field name.
	// However, if you have resource id, then you actually want resource.name field.
	fieldHelpText := ctx.Field.Documentation
	if nameField := provider.FindNameField(resource); idField != nil && nameField != nil {
		fieldHelpText = nameField.Documentation
	}

	// documentation for LRO service is stripped. Provide fallback.
	if fieldHelpText == "" && provider.IsOperationsMethod(ctx.Method) {
		fieldHelpText = provider.OperationMethodDocumentation(ctx.Method.Name)
	}

	collectionPath := provider.GetCollectionPathFromSegments(segments)
	hostParts := strings.Split(ctx.Service.DefaultHost, ".")
	shortServiceName := hostParts[0]

	param := Argument{
		HelpText:          provider.CleanDocumentation(fieldHelpText),
		IsPositional:      !provider.IsList(ctx.Method),
		IsPrimaryResource: true,
		Required:          true,
		ResourceSpec: &ResourceSpec{
			Name:                  resourceName,
			PluralName:            provider.GetPluralFromSegments(segments),
			Collection:            fmt.Sprintf("%s.%s", shortServiceName, collectionPath),
			DisableAutoCompleters: provider.IsList(ctx.Method),
			Attributes:            newAttributesFromSegments(segments),
		},
	}

	if idField != nil {
		param.RequestIDField = strcase.ToLowerCamel(idField.Name)
	}

	return param
}

// resourceReferenceSpec creates a ResourceSpec for a field that references
// another resource type (e.g., a `--network` flag).
func resourceReferenceSpec(ctx *ArgumentContext) (*ResourceSpec, error) {
	for _, def := range ctx.Model.ResourceDefinitions {
		if def.Type == ctx.Field.ResourceReference.Type {
			if len(def.Patterns) == 0 {
				return nil, fmt.Errorf("resource definition for %q has no patterns", def.Type)
			}
			// TODO(https://github.com/googleapis/librarian/issues/3415): Support multiple resource patterns and multitype resources.
			segments := def.Patterns[0]

			pluralName := def.Plural
			if pluralName == "" {
				pluralName = provider.GetPluralFromSegments(segments)
			}

			name := provider.GetSingularFromSegments(segments)

			hostParts := strings.Split(ctx.Service.DefaultHost, ".")
			shortServiceName := hostParts[0]
			baseCollectionPath := provider.GetCollectionPathFromSegments(segments)
			fullCollectionPath := fmt.Sprintf("%s.%s", shortServiceName, baseCollectionPath)

			return &ResourceSpec{
				Name:       name,
				PluralName: pluralName,
				Collection: fullCollectionPath,
				// TODO(https://github.com/googleapis/librarian/issues/3416): Investigate and enable auto-completers for referenced resources.
				DisableAutoCompleters: true,
				Attributes:            newAttributesFromSegments(segments),
			}, nil
		}
	}
	return nil, fmt.Errorf("resource definition not found for type %q", ctx.Field.ResourceReference.Type)
}

// newAttributesFromSegments parses a structured resource pattern and extracts the attributes
// that make up the resource's name.
func newAttributesFromSegments(segments []api.PathSegment) []Attribute {
	var attributes []Attribute

	for i, part := range segments {
		if part.Variable == nil {
			continue
		}

		if len(part.Variable.FieldPath) == 0 {
			continue
		}
		name := part.Variable.FieldPath[len(part.Variable.FieldPath)-1]
		var parameterName string

		// The `parameter_name` is derived from the preceding literal segment
		// (e.g., "projects" -> "projectsId"). This is a gcloud convention.
		if i > 0 && segments[i-1].Literal != nil {
			parameterName = *segments[i-1].Literal + "Id"
		} else {
			parameterName = name + "sId"
		}

		attr := Attribute{
			AttributeName: name,
			ParameterName: parameterName,
			Help:          fmt.Sprintf("The %s id of the {resource} resource.", name),
		}

		// Standard gcloud property fallback so users don't need to specify --project
		// if it's already configured.
		if name == "project" {
			attr.Property = "core/project"
		}
		attributes = append(attributes, attr)
	}
	return attributes
}
