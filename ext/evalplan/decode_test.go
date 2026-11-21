// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package evalplan

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
)

func parseFixture(t *testing.T, src string) hcl.Body {
	t.Helper()
	parser := hclparse.NewParser()
	f, diags := parser.ParseHCL([]byte(src), "fixture.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse failed: %s", diags)
	}
	return f.Body
}

func outputBlockSpec() hcldec.Spec {
	return &hcldec.BlockSpec{
		TypeName: "output",
		Nested: hcldec.ObjectSpec{
			"name":  &hcldec.BlockLabelSpec{Index: 0, Name: "name"},
			"value": &hcldec.AttrSpec{Name: "value", Type: cty.Number},
		},
	}
}

func TestDecodeBodyLazyGraph(t *testing.T) {
	// The label of each locals block is the root variable name, so
	// expressions reference the bare name rather than a "local.*" path.
	src := `
locals "base" {
  expression = 2
}
locals "doubled" {
  expression = base * 2
}
locals "combined" {
  expression = doubled + base
}
output "answer" {
  value = combined
}
`
	body := parseFixture(t, src)
	spec := hcldec.ObjectSpec{"answer": outputBlockSpec()}
	opts := &DecodeOptions{VariableBlockType: "locals"}

	result, values, _, diags := DecodeBody(context.Background(), body, spec, opts, false)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags)
	}

	if got := result.GetAttr("answer").GetAttr("value").AsBigFloat().String(); got != "6" {
		t.Fatalf("answer.value = %s; want 6", got)
	}
	for name, want := range map[string]string{
		"base":     "2",
		"doubled":  "4",
		"combined": "6",
	} {
		if got := values[name].AsBigFloat().String(); got != want {
			t.Errorf("values[%s] = %s; want %s", name, got, want)
		}
	}
}

func TestDecodeBodyPartialKeepsLeftovers(t *testing.T) {
	src := `
locals "base" {
  expression = 10
}
output "answer" {
  value = base + 5
}
other "x" {
  note = "kept"
}
`
	body := parseFixture(t, src)
	spec := hcldec.ObjectSpec{"answer": outputBlockSpec()}
	opts := &DecodeOptions{VariableBlockType: "locals"}

	result, leftover, diags := PartialDecode(context.Background(), body, spec, opts)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags)
	}
	if got := result.GetAttr("answer").GetAttr("value").AsBigFloat().String(); got != "15" {
		t.Fatalf("value = %s; want 15", got)
	}

	content, diags := leftover.Content(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "other", LabelNames: []string{"id"}}},
	})
	if diags.HasErrors() {
		t.Fatalf("leftover body lost content: %s", diags)
	}
	if len(content.Blocks) != 1 || content.Blocks[0].Type != "other" {
		t.Fatalf("leftover = %#v; want the other block", content.Blocks)
	}
}

func TestDecodeBodyMergesCycleMissingAndTypeDiags(t *testing.T) {
	src := `
locals "a" {
  expression = b
}
locals "b" {
  expression = a
}
locals "uses_missing" {
  expression = undefined_thing
}
output "answer" {
  value = a
}
badtype {
  value = "string where number expected"
}
`
	body := parseFixture(t, src)
	spec := hcldec.ObjectSpec{
		"answer": outputBlockSpec(),
		"badtype": &hcldec.BlockSpec{
			TypeName: "badtype",
			Nested: hcldec.ObjectSpec{
				"value": &hcldec.AttrSpec{Name: "value", Type: cty.Number},
			},
		},
	}
	opts := &DecodeOptions{VariableBlockType: "locals"}

	_, _, _, diags := DecodeBody(context.Background(), body, spec, opts, false)
	if !diags.HasErrors() {
		t.Fatal("expected merged diagnostics")
	}

	summaries := map[string]int{}
	for _, d := range diags {
		summaries[d.Summary]++
	}
	if summaries["Cyclic variable dependency"] == 0 {
		t.Error("cycle diagnostic missing")
	}
	if summaries["Unknown variable"] == 0 {
		t.Error("missing-variable diagnostic missing")
	}
	if summaries["Incorrect attribute value type"] == 0 {
		t.Error("hcldec type diagnostic missing")
	}

	// Subjects remain populated so the standard diagnostic coloring path
	// keeps working unchanged.
	for _, d := range diags {
		if d.Subject == nil {
			t.Errorf("diagnostic %q has no subject range", d.Summary)
		}
	}
}

func TestDecodeBodyExternalVariablesAndUnknowns(t *testing.T) {
	src := `
locals "derived" {
  expression = input + 1
}
output "v" {
  value = derived
}
`
	body := parseFixture(t, src)
	spec := hcldec.ObjectSpec{"v": outputBlockSpec()}
	opts := &DecodeOptions{
		VariableBlockType: "locals",
		Variables: map[string]cty.Value{
			"input": cty.UnknownVal(cty.Number),
		},
	}

	result, _, _, diags := DecodeBody(context.Background(), body, spec, opts, false)
	if diags.HasErrors() {
		t.Fatalf("unknown must not be an error: %s", diags)
	}
	val := result.GetAttr("v").GetAttr("value")
	if val.IsNull() {
		t.Fatal("unknown propagated as null; want unknown number")
	}
	if val.IsKnown() {
		t.Fatal("expected unknown value to propagate to hcldec output")
	}
}

func TestDecodeBodyCancellation(t *testing.T) {
	src := `
locals "a" {
  expression = 1
}
locals "b" {
  expression = a + 1
}
`
	body := parseFixture(t, src)
	opts := &DecodeOptions{VariableBlockType: "locals"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, _, diags := DecodeBody(ctx, body, hcldec.ObjectSpec{}, opts, true)
	found := false
	for _, d := range diags {
		if strings.Contains(d.Summary, "cancelled") {
			found = true
		}
	}
	if !found {
		t.Fatalf("cancellation diagnostic missing; got %v", diags)
	}
}

func TestDecodeBodyDiagnosticContentStable(t *testing.T) {
	// The same plan evaluated twice yields identical diagnostic counts and
	// equivalent values (deduplication is stable across runs).
	src := `
locals "a" {
  expression = missing
}
locals "b" {
  expression = missing
}
`
	body := parseFixture(t, src)
	opts := &DecodeOptions{VariableBlockType: "locals"}
	_, values1, _, d1 := DecodeBody(context.Background(), body, hcldec.ObjectSpec{}, opts, true)
	_, values2, _, d2 := DecodeBody(context.Background(), body, hcldec.ObjectSpec{}, opts, true)

	if len(d1) != len(d2) {
		t.Fatalf("diagnostic count unstable: %d vs %d", len(d1), len(d2))
	}
	if !values1["a"].RawEquals(values2["a"]) {
		t.Fatalf("values unstable across runs: %#v vs %#v", values1["a"], values2["a"])
	}
}
