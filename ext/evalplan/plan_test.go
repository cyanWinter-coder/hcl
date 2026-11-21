// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package evalplan

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// parseExpr parses a single native-syntax expression for tests.
func parseExpr(t *testing.T, src string) hcl.Expression {
	t.Helper()
	expr, diags := hclsyntax.ParseExpression([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("unexpected parse diagnostics for %q: %s", src, diags)
	}
	return expr
}

func TestPlanSimpleDependency(t *testing.T) {
	p := NewPlan(nil)
	if diags := p.Add("a", parseExpr(t, "base + 1")); diags.HasErrors() {
		t.Fatal(diags)
	}
	if diags := p.Add("b", parseExpr(t, "a * 2")); diags.HasErrors() {
		t.Fatal(diags)
	}
	if diags := p.SetVariable("base", cty.NumberIntVal(10)); diags.HasErrors() {
		t.Fatal(diags)
	}

	val, diags := p.Value(context.Background(), "b")
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags)
	}
	if got := val.AsBigFloat().String(); got != "22" {
		t.Fatalf("b = %s; want 22", got)
	}

	// a must have been evaluated exactly once and be reusable.
	aval, _ := p.Value(context.Background(), "a")
	if got := aval.AsBigFloat().String(); got != "11" {
		t.Fatalf("a = %s; want 11", got)
	}
}

func TestPlanEvaluatesEachNodeOnce(t *testing.T) {
	p := NewPlan(nil)

	var calls int
	var mu sync.Mutex
	// Two blocks depend on "shared"; both must reuse its single result.
	shared := &countingExpr{
		inner: parseExpr(t, "base * 2"),
		after: func() {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	}
	p.SetVariable("base", cty.NumberIntVal(3))
	p.Add("shared", shared)
	p.Add("x", parseExpr(t, "shared + 1"))
	p.Add("y", parseExpr(t, "shared - 1"))

	if _, diags := p.Values(context.Background()); diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags)
	}
	if calls != 1 {
		t.Fatalf("shared evaluated %d times; want exactly 1", calls)
	}
}

// countingExpr wraps an expression and records evaluations.
type countingExpr struct {
	inner hcl.Expression
	after func()
}

func (e *countingExpr) Value(ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	val, diags := e.inner.Value(ctx)
	if e.after != nil {
		e.after()
	}
	return val, diags
}
func (e *countingExpr) Variables() []hcl.Traversal { return e.inner.Variables() }
func (e *countingExpr) Range() hcl.Range           { return e.inner.Range() }
func (e *countingExpr) StartRange() hcl.Range      { return e.inner.StartRange() }

func TestPlanUnknownPropagatesAsUnknownNotNull(t *testing.T) {
	p := NewPlan(nil)
	p.SetVariable("known", cty.NumberIntVal(1))
	p.SetVariable("mystery", cty.UnknownVal(cty.Number))
	p.Add("derived", parseExpr(t, "mystery + 10"))
	p.Add("indirect", parseExpr(t, "derived * 2"))
	p.Add("unrelated", parseExpr(t, "known + 41"))

	values, diags := p.Values(context.Background())
	if diags.HasErrors() {
		t.Fatalf("unknown values must not be errors: %s", diags)
	}
	if values["derived"].IsNull() || values["indirect"].IsNull() {
		t.Fatal("unknown dependency must not be treated as null")
	}
	if values["derived"].IsKnown() || values["indirect"].IsKnown() {
		t.Fatal("unknown must propagate along dependency edges")
	}
	if !values["unrelated"].IsKnown() || values["unrelated"].AsBigFloat().String() != "42" {
		t.Fatalf("unrelated node still deliverable; got %#v", values["unrelated"])
	}
}

func TestPlanMissingVariableReportsOnce(t *testing.T) {
	p := NewPlan(nil)
	// Both nodes reference the same missing root; the error must appear once
	// and only the directly affected outputs are unknown.
	p.SetVariable("safe", cty.StringVal("ok"))
	p.Add("a", parseExpr(t, "missing_root"))
	p.Add("b", parseExpr(t, "missing_root.id"))
	p.Add("c", parseExpr(t, "safe"))

	values, diags := p.Values(context.Background())
	if !diags.HasErrors() {
		t.Fatal("expected missing-variable diagnostics")
	}
	count := 0
	for _, d := range diags {
		if d.Summary == "Unknown variable" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("missing variable reported %d times; want 1", count)
	}
	if values["a"].IsKnown() || values["b"].IsKnown() {
		t.Fatal("affected outputs should be unknown")
	}
	if !values["c"].IsKnown() || values["c"].AsString() != "ok" {
		t.Fatalf("unrelated output must still be delivered; got %#v", values["c"])
	}
}

func TestPlanCycleReturnsOrderedChain(t *testing.T) {
	p := NewPlan(nil)
	// Source order deliberately differs from dependency order; the chain
	// must come back sorted by source range regardless.
	p.Add("first", parseExprAt(t, "third + 1", 30))
	p.Add("second", parseExprAt(t, "first + 1", 20))
	p.Add("third", parseExprAt(t, "second + 1", 10))

	diags := p.Analyze()
	if !diags.HasErrors() {
		t.Fatal("expected a cycle diagnostic")
	}
	var extra *CycleExtra
	for _, d := range diags {
		if d.Summary != "Cyclic variable dependency" {
			continue
		}
		var ok bool
		extra, ok = d.Extra.(*CycleExtra)
		if !ok {
			t.Fatalf("cycle diagnostic missing CycleExtra; got %T", d.Extra)
		}
	}
	if extra == nil {
		t.Fatal("no cycle diagnostic produced")
	}

	want := []string{"third", "second", "first"} // ranges 10, 20, 30
	if diff := cmp.Diff(want, extra.Names); diff != "" {
		t.Fatalf("cycle chain mismatch (-want +got):\n%s", diff)
	}

	// Evaluation of a cyclic node returns unknown, not recursion/timeout.
	val, evalDiags := p.Value(context.Background(), "first")
	if val.IsKnown() {
		t.Fatal("cyclic node must evaluate to unknown")
	}
	if !evalDiags.HasErrors() {
		t.Fatal("cycle must still surface through Value diagnostics")
	}
}

// parseExprAt parses src and shifts its source range start to the given
// byte offset, keeping distinct ordering for cycle tests.
func parseExprAt(t *testing.T, src string, offset int) hcl.Expression {
	t.Helper()
	expr := parseExpr(t, src)
	return &offsetExpr{Expression: expr, offset: offset}
}

type offsetExpr struct {
	hcl.Expression
	offset int
}

func (e *offsetExpr) Range() hcl.Range {
	r := e.Expression.Range()
	r.Start.Byte = e.offset
	r.End.Byte = e.offset + 1
	return r
}

func TestPlanMergesMissingAndTypeDiagnostics(t *testing.T) {
	// hcldec integration covers the type-error merge; at the plan layer a
	// function/argument type error and a missing variable must coexist.
	fn := function.New(&function.Spec{
		Params: []function.Parameter{{Name: "n", Type: cty.Number}},
		Type:   function.StaticReturnType(cty.Number),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			return args[0], nil
		},
	})
	p := NewPlan(map[string]function.Function{"neednum": fn})
	p.Add("bad", parseExpr(t, `neednum("not a number")`))
	p.Add("broken", parseExpr(t, "undefined_root"))

	_, diags := p.Values(context.Background())
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	summaries := map[string]bool{}
	for _, d := range diags {
		summaries[d.Summary] = true
	}
	if !summaries["Unknown variable"] {
		t.Error("missing-variable diagnostic absent")
	}
	if !summaries["Invalid function argument"] {
		t.Error("type-error diagnostic absent")
	}
}

func TestPlanCancellationWakesWaitersAndStopsNewNodes(t *testing.T) {
	p := NewPlan(nil)
	p.SetVariable("base", cty.NumberIntVal(1))

	started := make(chan struct{})
	release := make(chan struct{})
	blocking := &blockingExpr{
		inner:   parseExpr(t, "base"),
		started: started,
		release: release,
	}
	p.Add("slow", blocking)
	p.Add("dependent", parseExpr(t, "slow + 1"))
	// Registered up front (before any evaluation starts) so the
	// pre-cancellation check below never mutates a plan that has an
	// in-flight node.
	freshRan := make(chan struct{}, 1)
	p.Add("fresh", &signalExpr{
		inner: parseExpr(t, "base + 2"),
		ran:   freshRan,
	})

	// Goroutine A owns "slow" and parks inside the expression.
	go func() { p.Value(context.Background(), "slow") }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking expression never started")
	}

	// Goroutine B requests the dependent node. It blocks on the in-flight
	// "slow" node's waiter channel. Cancelling B's context must wake it
	// immediately without waiting for the blocked expression to finish.
	ctxB, cancelB := context.WithCancel(context.Background())
	resultB := make(chan struct {
		v cty.Value
		d hcl.Diagnostics
	}, 1)
	go func() {
		v, d := p.Value(ctxB, "dependent")
		resultB <- struct {
			v cty.Value
			d hcl.Diagnostics
		}{v, d}
	}()
	// Give B time to register as a waiter.
	time.Sleep(100 * time.Millisecond)
	cancelB()
	select {
	case res := <-resultB:
		if !res.d.HasErrors() {
			t.Fatal("cancelled waiter must return an error diagnostic")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("cancelled waiter was not woken")
	}

	// A separate, uncancelled node that has not started yet must refuse to
	// start under a cancelled context.
	ctxC, cancelC := context.WithCancel(context.Background())
	cancelC()
	_, diagsC := p.Value(ctxC, "fresh")
	if !diagsC.HasErrors() {
		t.Fatal("pre-cancelled context must stop a new node from starting")
	}
	select {
	case <-freshRan:
		t.Fatal("fresh node expression must not run under a cancelled context")
	default:
	}

	// Releasing A lets the in-flight node finish and cache normally: a later
	// uncancelled read of the dependent gets the real value without re-running
	// the blocked expression (which would panic by closing started twice).
	close(release)
	val, diags := p.Value(context.Background(), "dependent")
	if diags.HasErrors() {
		t.Fatalf("read after cancellation failed: %s", diags)
	}
	if val.AsBigFloat().String() != "2" {
		t.Fatalf("dependent = %s; want 2", val.AsBigFloat().String())
	}
	if got := blocking.runs(); got != 1 {
		t.Fatalf("blocked expression ran %d times; want exactly 1 (no re-evaluation)", got)
	}
}

func TestPlanOwnerCancellationDoesNotLeakToLiveWaiters(t *testing.T) {
	p := NewPlan(nil)
	p.SetVariable("base", cty.NumberIntVal(1))

	started := make(chan struct{})
	release := make(chan struct{})
	// The blocked expression returns immediately once released; on a second
	// invocation (after the cancelled owner resets the node) it still works.
	blocking := &blockingExpr{
		inner:   parseExpr(t, "base + 41"),
		started: started,
		release: release,
	}
	p.Add("slow", blocking)

	// Owner A runs under a cancellable context and parks inside the node.
	ctxA, cancelA := context.WithCancel(context.Background())
	go func() { p.Value(ctxA, "slow") }()
	<-started

	// Live waiter B requests the same node with a background context and
	// blocks on the in-flight attempt.
	resB := make(chan struct {
		v cty.Value
		d hcl.Diagnostics
	}, 1)
	go func() {
		v, d := p.Value(context.Background(), "slow")
		resB <- struct {
			v cty.Value
			d hcl.Diagnostics
		}{v, d}
	}()
	time.Sleep(100 * time.Millisecond)

	// Cancelling A must not make B fail: B should be told to reset and retry.
	cancelA()
	select {
	case <-resB:
		t.Fatal("live waiter returned before the node could be retried")
	case <-time.After(100 * time.Millisecond):
		// Correctly still waiting for the retried attempt to run.
	}

	close(release)
	select {
	case res := <-resB:
		if res.d.HasErrors() {
			t.Fatalf("live waiter must succeed after retry: %s", res.d)
		}
		if res.v.AsBigFloat().String() != "42" {
			t.Fatalf("live waiter value = %s; want 42", res.v.AsBigFloat().String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live waiter was not woken after the node became runnable")
	}
}

type blockingExpr struct {
	inner   hcl.Expression
	started chan struct{}
	release chan struct{}
	once    sync.Once
	runMu   sync.Mutex
	runCnt  int
}

func (e *blockingExpr) Value(ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	e.runMu.Lock()
	e.runCnt++
	e.runMu.Unlock()
	e.once.Do(func() { close(e.started) })
	<-e.release
	return e.inner.Value(ctx)
}
func (e *blockingExpr) runs() int {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	return e.runCnt
}
func (e *blockingExpr) Variables() []hcl.Traversal { return e.inner.Variables() }
func (e *blockingExpr) Range() hcl.Range           { return e.inner.Range() }
func (e *blockingExpr) StartRange() hcl.Range      { return e.inner.StartRange() }

func TestPlanCancellationBeforeStart(t *testing.T) {
	p := NewPlan(nil)
	p.SetVariable("base", cty.NumberIntVal(5))

	ran := make(chan struct{}, 1)
	expr := &signalExpr{inner: parseExpr(t, "base + 1"), ran: ran}
	p.Add("n", expr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, diags := p.Value(ctx, "n"); !diags.HasErrors() {
		t.Fatal("pre-cancelled context must stop the node from starting")
	}
	select {
	case <-ran:
		t.Fatal("node expression must not run under an already-cancelled context")
	default:
	}

	// Retry works and evaluates normally.
	val, diags := p.Value(context.Background(), "n")
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	if val.AsBigFloat().String() != "6" {
		t.Fatalf("got %s; want 6", val.AsBigFloat().String())
	}
}

type signalExpr struct {
	inner hcl.Expression
	ran   chan struct{}
}

func (e *signalExpr) Value(ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	e.ran <- struct{}{}
	return e.inner.Value(ctx)
}
func (e *signalExpr) Variables() []hcl.Traversal { return e.inner.Variables() }
func (e *signalExpr) Range() hcl.Range           { return e.inner.Range() }
func (e *signalExpr) StartRange() hcl.Range      { return e.inner.StartRange() }

func TestPlanConcurrentReads(t *testing.T) {
	p := NewPlan(nil)
	p.SetVariable("base", cty.NumberIntVal(10))
	for i := 0; i < 20; i++ {
		p.Add(fmt.Sprintf("n%d", i), parseExpr(t, fmt.Sprintf("base + %d", i)))
	}

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				val, diags := p.Value(context.Background(), fmt.Sprintf("n%d", i))
				if diags.HasErrors() {
					errs <- errors.New(diags.Error())
					return
				}
				want := fmt.Sprintf("%d", 10+i)
				if val.AsBigFloat().String() != want {
					errs <- fmt.Errorf("n%d = %s; want %s", i, val.AsBigFloat().String(), want)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestPlanFunctionsFromCaller(t *testing.T) {
	fn := function.New(&function.Spec{
		Params: []function.Parameter{{Name: "s", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			return cty.StringVal("hello " + args[0].AsString()), nil
		},
	})
	p := NewPlan(map[string]function.Function{"greet": fn})
	p.Add("msg", parseExpr(t, `greet("world")`))

	val, diags := p.Value(context.Background(), "msg")
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	if val.AsString() != "hello world" {
		t.Fatalf("got %q", val.AsString())
	}
}

func TestPlanDuplicateRegistration(t *testing.T) {
	p := NewPlan(nil)
	if diags := p.Add("x", parseExpr(t, "1")); diags.HasErrors() {
		t.Fatal(diags)
	}
	if diags := p.Add("x", parseExpr(t, "2")); !diags.HasErrors() {
		t.Fatal("duplicate Add should error")
	}
	if diags := p.SetVariable("x", cty.NumberIntVal(3)); !diags.HasErrors() {
		t.Fatal("SetVariable conflicting with Add should error")
	}
	if diags := p.SetVariable("y", cty.NumberIntVal(1)); diags.HasErrors() {
		t.Fatal(diags)
	}
	if diags := p.SetVariable("y", cty.NumberIntVal(2)); !diags.HasErrors() {
		t.Fatal("duplicate SetVariable should error")
	}
}

func TestPlanNamesAndOrderIndependence(t *testing.T) {
	p := NewPlan(nil)
	p.SetVariable("zeta", cty.NumberIntVal(1))
	p.Add("alpha", parseExpr(t, "zeta"))
	p.Add("mid", parseExpr(t, "alpha + zeta"))

	got := p.Names()
	want := []string{"alpha", "mid", "zeta"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Names mismatch (-want +got):\n%s", diff)
	}

	values, diags := p.Values(context.Background())
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	// Map output content is keyed and unaffected by traversal order.
	if values["mid"].AsBigFloat().String() != "2" {
		t.Fatalf("mid = %s; want 2", values["mid"].AsBigFloat().String())
	}
}

func TestPlanMutationInvalidatesMemoizedResults(t *testing.T) {
	p := NewPlan(nil)
	p.Add("n", parseExpr(t, "base + 1"))

	// First evaluation cannot resolve "base".
	val, diags := p.Value(context.Background(), "n")
	if !diags.HasErrors() {
		t.Fatal("expected missing-variable error before seeding")
	}
	if val.IsKnown() {
		t.Fatal("unresolved node must be unknown")
	}

	// Supplying the missing variable afterwards must invalidate the cached
	// failure and re-evaluate against the new graph.
	if d := p.SetVariable("base", cty.NumberIntVal(41)); d.HasErrors() {
		t.Fatal(d)
	}
	val, diags = p.Value(context.Background(), "n")
	if diags.HasErrors() {
		t.Fatalf("re-evaluation after seeding failed: %s", diags)
	}
	if val.AsBigFloat().String() != "42" {
		t.Fatalf("n = %s; want 42", val.AsBigFloat().String())
	}
}
