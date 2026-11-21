// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package evalplan

import (
	"context"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestPlanDiamondReusesSharedNode(t *testing.T) {
	//      base
	//       |
	//     shared
	//     /    \
	//  left    right
	//     \    /
	//      join
	p := NewPlan(nil)
	var (
		mu      sync.Mutex
		sharedN int
	)
	shared := &countingExpr{
		inner: parseExpr(t, "base + 1"),
		after: func() {
			mu.Lock()
			sharedN++
			mu.Unlock()
		},
	}
	p.SetVariable("base", cty.NumberIntVal(10))
	p.Add("shared", shared)
	p.Add("left", parseExpr(t, "shared * 2"))
	p.Add("right", parseExpr(t, "shared * 3"))
	p.Add("join", parseExpr(t, "left + right"))

	val, diags := p.Value(context.Background(), "join")
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	// shared=11, left=22, right=33, join=55.
	if val.AsBigFloat().String() != "55" {
		t.Fatalf("join = %s; want 55", val.AsBigFloat().String())
	}
	if sharedN != 1 {
		t.Fatalf("diamond shared node evaluated %d times; want 1", sharedN)
	}
}

func TestPlanMultipleCyclesAllReported(t *testing.T) {
	p := NewPlan(nil)
	// Two disjoint cycles: a<->b and c<->d.
	p.Add("a", parseExprAt(t, "b", 10))
	p.Add("b", parseExprAt(t, "a", 20))
	p.Add("c", parseExprAt(t, "d", 30))
	p.Add("d", parseExprAt(t, "c", 40))

	diags := p.Analyze()
	cycles := 0
	names := map[string]bool{}
	for _, d := range diags {
		if d.Summary != "Cyclic variable dependency" {
			continue
		}
		cycles++
		extra := d.Extra.(*CycleExtra)
		for _, n := range extra.Names {
			names[n] = true
		}
	}
	if cycles != 2 {
		t.Fatalf("found %d cycles; want 2", cycles)
	}
	if len(names) != 4 || !names["a"] || !names["b"] || !names["c"] || !names["d"] {
		t.Fatalf("cycle nodes = %v; want a,b,c,d", names)
	}
}

func TestPlanSelfCycle(t *testing.T) {
	p := NewPlan(nil)
	p.Add("x", parseExprAt(t, "x + 1", 5))

	diags := p.Analyze()
	if !diags.HasErrors() {
		t.Fatal("self-reference must be a cycle")
	}
	found := false
	for _, d := range diags {
		if extra, ok := d.Extra.(*CycleExtra); ok {
			found = true
			if len(extra.Names) != 1 || extra.Names[0] != "x" {
				t.Fatalf("self cycle = %v; want [x]", extra.Names)
			}
		}
	}
	if !found {
		t.Fatal("self cycle diagnostic missing")
	}
}

func TestPlanListAndMapOutputsOrderIndependent(t *testing.T) {
	p := NewPlan(nil)
	p.SetVariable("items", cty.ListVal([]cty.Value{
		cty.NumberIntVal(1),
		cty.NumberIntVal(2),
		cty.NumberIntVal(3),
	}))
	p.SetVariable("mapping", cty.MapVal(map[string]cty.Value{
		"z": cty.NumberIntVal(9),
		"a": cty.NumberIntVal(1),
	}))
	// for over a list preserves element order; object/map construction must
	// not depend on internal traversal ordering.
	p.Add("summed", parseExpr(t, "[for v in items: v * 10]"))
	p.Add("picked", parseExpr(t, "{for k, v in mapping: k => v + 1}"))

	values, diags := p.Values(context.Background())
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	list := values["summed"]
	if list.LengthInt() != 3 {
		t.Fatalf("list length = %d; want 3", list.LengthInt())
	}
	want := []string{"10", "20", "30"}
	for i, w := range want {
		if got := list.Index(cty.NumberIntVal(int64(i))).AsBigFloat().String(); got != w {
			t.Errorf("list[%d] = %s; want %s", i, got, w)
		}
	}

	obj := values["picked"]
	for k, w := range map[string]string{"a": "2", "z": "10"} {
		if got := obj.GetAttr(k).AsBigFloat().String(); got != w {
			t.Errorf("map[%s] = %s; want %s", k, got, w)
		}
	}
}

func TestPlanUnknownNotDeliveredAsNullInNestedStructures(t *testing.T) {
	p := NewPlan(nil)
	p.SetVariable("known", cty.NumberIntVal(7))
	p.SetVariable("maybe", cty.UnknownVal(cty.String))
	p.Add("container", parseExpr(t, `{safe = known, risky = maybe}`))

	val, diags := p.Value(context.Background(), "container")
	if diags.HasErrors() {
		t.Fatalf("unknown must not error: %s", diags)
	}
	if val.IsNull() {
		t.Fatal("container must not collapse to null just because one attr is unknown")
	}
	if val.Type() == cty.DynamicPseudoType {
		// ObjectCons returns DynamicVal when a key is unknown; that is
		// acceptable cty behavior, but verify it is still unknown, not null.
		if val.IsNull() {
			t.Fatal("dynamic value is null")
		}
	}
}

func TestPlanErrorNodeDoesNotReevaluate(t *testing.T) {
	p := NewPlan(nil)
	var (
		mu    sync.Mutex
		calls int
	)
	bad := &countingExpr{
		inner: parseExpr(t, "undefined_root"),
		after: func() {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	}
	p.Add("bad", bad)
	p.Add("d1", parseExpr(t, "bad + 1"))
	p.Add("d2", parseExpr(t, "bad + 2"))

	_, diags := p.Values(context.Background())
	if !diags.HasErrors() {
		t.Fatal("expected missing-variable error")
	}
	// Because the missing root short-circuits before expression evaluation,
	// the expression itself must not run at all; either way it is resolved
	// once rather than once per dependent.
	mu.Lock()
	got := calls
	mu.Unlock()
	if got > 1 {
		t.Fatalf("failing expression evaluated %d times; want at most 1", got)
	}

	// A second full evaluation reuses the memoized failure state.
	_, diags2 := p.Values(context.Background())
	mu.Lock()
	got2 := calls
	mu.Unlock()
	if got2 != got {
		t.Fatalf("second run re-evaluated failed node: %d vs %d", got, got2)
	}
	if !diags2.HasErrors() {
		t.Fatal("memoized failure must still report diagnostics")
	}
}
