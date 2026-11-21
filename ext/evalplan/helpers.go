// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package evalplan

import (
	"context"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// rangeLess imposes a total, deterministic order over source ranges so that
// cycle chains and other diagnostics are independent of map iteration
// order. It compares filename first, then the start byte offset, then the
// end byte offset.
func rangeLess(a, b hcl.Range) bool {
	if a.Filename != b.Filename {
		return a.Filename < b.Filename
	}
	if a.Start.Byte != b.Start.Byte {
		return a.Start.Byte < b.Start.Byte
	}
	return a.End.Byte < b.End.Byte
}

// mergeDiags concatenates diagnostic lists, skipping nil lists. It does not
// de-duplicate: each evaluation path may legitimately surface the same
// underlying node diagnostic once, and [dedupeDiags] is applied only at the
// boundaries where a full plan result is assembled.
func mergeDiags(lists ...hcl.Diagnostics) hcl.Diagnostics {
	var out hcl.Diagnostics
	for _, list := range lists {
		out = append(out, list...)
	}
	return out
}

// dedupeDiags removes duplicate diagnostic pointers and structurally equal
// diagnostics, preserving the order of first appearance. Memoization means
// the same node diagnostic can be merged in via several dependency paths;
// this keeps the final set tidy without relying on callers to track what
// they already saw.
func dedupeDiags(diags hcl.Diagnostics) hcl.Diagnostics {
	seen := map[string]struct{}{}
	out := make(hcl.Diagnostics, 0, len(diags))
	for _, diag := range diags {
		if diag == nil {
			continue
		}
		key := diagKey(diag)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, diag)
	}
	return out
}

func diagKey(d *hcl.Diagnostic) string {
	subject := ""
	if d.Subject != nil {
		subject = d.Subject.String()
	}
	return fmt.Sprintf("%d|%s|%s|%s", d.Severity, d.Summary, d.Detail, subject)
}

// duplicateDeclDiag builds the diagnostic for a name registered twice.
func duplicateDeclDiag(name string, newRange, existingRange hcl.Range) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  "Duplicate variable definition",
		Detail: fmt.Sprintf(
			"The name %q is already defined in this evaluation plan; the earlier definition is at %s.",
			name, existingRange.String(),
		),
		Subject: newRange.Ptr(),
	}
}

// valueRange returns a best-effort source range for an externally provided
// value that has no source location of its own. It is used only in
// duplicate-definition diagnostics, where a distinct placeholder keeps the
// message honest about the earlier definition not being an expression.
func valueRange(val cty.Value) hcl.Range {
	return hcl.Range{Filename: "<provided value>"}
}

// cancelDiag translates a context cancellation into an HCL diagnostic.
func cancelDiag(ctx context.Context) *hcl.Diagnostic {
	err := ctx.Err()
	if err == nil {
		err = context.Canceled
	}
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  "Evaluation cancelled",
		Detail:   fmt.Sprintf("Evaluation was stopped before completion: %s.", err),
	}
}
