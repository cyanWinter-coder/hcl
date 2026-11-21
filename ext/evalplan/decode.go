// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package evalplan

import (
	"context"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// DecodeOptions controls how [DecodeBody] builds and evaluates its plan.
//
// The zero value is usable: with no pre-existing variables the plan treats
// every referenced root as a missing variable unless the body itself defines
// it through the variable blocks.
type DecodeOptions struct {
	// Variables are externally known values seeded into the plan before
	// evaluation, mirroring hcl.EvalContext.Variables.
	Variables map[string]cty.Value

	// Functions is the function table exposed to every expression, mirroring
	// hcl.EvalContext.Functions. As elsewhere in HCL, functions are provided
	// by the calling application; the plan never invents any.
	Functions map[string]function.Function

	// VariableBlockType is the block type whose blocks declare derived
	// variables, for example "variable" or "locals". Each block of this type
	// must contain an "expression" attribute (configurable via
	// ExpressionAttrName); the block label, when present, names the variable.
	//
	// If empty, the body is expected to define variables only through
	// Variables above, and DecodeBody behaves like a lazily scheduled version
	// of hcldec.Decode.
	VariableBlockType string

	// BlockLabelIndex selects which block label is the variable name when
	// blocks have multiple labels. Defaults to 0.
	BlockLabelIndex int

	// ExpressionAttrName is the attribute within a variable block holding
	// the defining expression. Defaults to "expression".
	ExpressionAttrName string
}

// DecodeBody interprets body against spec using a lazy [Plan].
//
// Variable-defining blocks (per opts.VariableBlockType) are registered as
// plan nodes; their cross-references form the DAG and are each evaluated at
// most once, with results shared across every block that references them.
// The remaining body content is then decoded with hcldec using an
// [hcl.EvalContext] populated from the evaluated variables, so all existing
// hcldec specs and diagnostic coloring behavior continue to work unchanged.
//
// Structural problems (cycles, missing variables) and hcldec's own
// diagnostics (including type errors) are returned together in a single
// [hcl.Diagnostics] collection. Nodes unrelated to any failure still
// evaluate and their values remain available via the returned map.
//
// When partial is true the behavior matches [hcldec.PartialDecode]: content
// not described by spec is returned as the leftover body.
func DecodeBody(ctx context.Context, body hcl.Body, spec hcldec.Spec, opts *DecodeOptions, partial bool) (cty.Value, map[string]cty.Value, hcl.Body, hcl.Diagnostics) {
	if opts == nil {
		opts = &DecodeOptions{}
	}
	var diags hcl.Diagnostics

	plan := NewPlan(opts.Functions)
	for name, val := range opts.Variables {
		diags = append(diags, plan.SetVariable(name, val)...)
	}

	// First collect the variable-defining blocks, if any, without consuming
	// anything else from the body. We use a permissive schema so body content
	// that does not match is left for hcldec below.
	var varBlocks hcl.Blocks
	var leftovers hcl.Body = body

	if opts.VariableBlockType != "" {
		// Variable blocks carry exactly one label naming the variable they
		// define. We always extract with PartialContent so the remaining body
		// is untouched for hcldec: this works for both partial and full
		// decodes, and hcldec below still enforces completeness against the
		// real spec.
		varSchema := &hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{
				{
					Type:       opts.VariableBlockType,
					LabelNames: []string{"name"},
				},
			},
		}
		var content *hcl.BodyContent
		content, leftovers, _ = body.PartialContent(varSchema)
		varBlocks = content.Blocks
	}

	labelIdx := opts.BlockLabelIndex
	exprAttr := opts.ExpressionAttrName
	if exprAttr == "" {
		exprAttr = "expression"
	}

	for _, block := range varBlocks {
		if labelIdx >= len(block.Labels) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid variable block",
				Detail:   fmt.Sprintf("Block of type %q requires a label naming the variable.", opts.VariableBlockType),
				Subject:  block.DefRange.Ptr(),
			})
			continue
		}
		name := block.Labels[labelIdx]

		attr, attrDiags := block.Body.Content(&hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{
				{Name: exprAttr, Required: true},
			},
		})
		diags = append(diags, attrDiags...)
		if attrDiags.HasErrors() {
			continue
		}
		diags = append(diags, plan.Add(name, attr.Attributes[exprAttr].Expr)...)
	}

	// Structural analysis (cycles and missing variables) up front so its
	// diagnostics can be merged with hcldec's rather than discovered lazily
	// one node at a time.
	diags = append(diags, plan.Analyze()...)

	values, valDiags := plan.Values(ctx)
	diags = append(diags, valDiags...)

	// Build the EvalContext hcldec expects. Functions and all known variable
	// values are supplied; failed or unknown nodes surface as cty.DynamicVal
	// and therefore propagate as unknown rather than null.
	evalCtx := &hcl.EvalContext{
		Variables: values,
		Functions: opts.Functions,
	}

	var (
		result      cty.Value
		decodedLeft hcl.Body
		decodeDiags hcl.Diagnostics
	)
	if partial {
		result, decodedLeft, decodeDiags = hcldec.PartialDecode(leftovers, spec, evalCtx)
	} else {
		result, decodeDiags = hcldec.Decode(leftovers, spec, evalCtx)
	}
	diags = append(diags, decodeDiags...)

	return result, values, decodedLeft, diags
}

// Decode is a convenience wrapper around [DecodeBody] that mirrors
// [hcldec.Decode] and discards the leftover body and evaluated-variable map.
func Decode(ctx context.Context, body hcl.Body, spec hcldec.Spec, opts *DecodeOptions) (cty.Value, hcl.Diagnostics) {
	val, _, _, diags := DecodeBody(ctx, body, spec, opts, false)
	return val, diags
}

// PartialDecode is a convenience wrapper around [DecodeBody] that mirrors
// [hcldec.PartialDecode] and returns the leftover body.
func PartialDecode(ctx context.Context, body hcl.Body, spec hcldec.Spec, opts *DecodeOptions) (cty.Value, hcl.Body, hcl.Diagnostics) {
	val, _, leftovers, diags := DecodeBody(ctx, body, spec, opts, true)
	return val, leftovers, diags
}
