// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

// Package evalplan implements lazy, dependency-driven evaluation of sets of
// HCL expressions whose variables may themselves be defined by other
// expressions.
//
// A [Plan] builds a directed acyclic graph (DAG) from the variable
// references ([hcl.Expression.Variables]) of the expressions registered on
// it. Each expression node is evaluated at most once, lazily, when its
// result is requested; its result is then reused by every other node that
// depends on it, including nodes contributed by different blocks.
//
// Unknown cty values propagate along dependency edges in the normal cty
// fashion but are never mistaken for null values: a node whose dependency
// is unknown simply evaluates to an unknown value rather than producing a
// cascade of "unknown variable" diagnostics. Nodes that are unrelated to a
// failing or unknown dependency still produce concrete, deliverable
// results.
//
// Reference cycles are detected during planning and reported as a complete
// cycle chain ordered by source range, alongside missing-variable and type
// diagnostics in the same [hcl.Diagnostics] collection.
//
// A plan is safe for concurrent reads: any number of goroutines may request
// values concurrently, and evaluation of independent nodes proceeds in
// parallel. Cancellation of the context passed to the evaluation methods
// stops any node that has not yet started, wakes every goroutine waiting on
// a dependency, and prevents partial results produced under the cancelled
// plan from being cached.
package evalplan

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// Plan is a lazy evaluation plan over a set of named expression nodes.
//
// A plan is built incrementally with [Plan.Add] and [Plan.SetVariable], and
// is safe for concurrent evaluation once built. Mutation methods reset all
// memoized results so a plan that is reused sequentially always reflects its
// latest definitions, but mutating a plan while an evaluation is in flight
// is not supported; the typical pattern is to build the whole plan first and
// only then share it for evaluation.
type Plan struct {
	mu sync.RWMutex

	// nodes holds every registered expression node keyed by its root
	// variable name.
	nodes map[string]*node

	// roots holds variable roots that were given concrete values directly
	// via SetVariable, rather than derived from an expression.
	roots map[string]cty.Value

	// funcs is the function table made available to every expression in
	// the plan. Functions are always provided by the calling application,
	// matching hcl.EvalContext semantics.
	funcs map[string]function.Function

	// cycles is populated by analyze and holds every reference cycle found
	// among expression nodes, each as an ordered list of node names.
	cycles [][]string

	// missing holds root names referenced by at least one expression but
	// never defined by either Add or SetVariable, along with a source range
	// where the reference occurs.
	missing map[string]hcl.Range

	analyzed bool
}

// node is a single expression in the plan's dependency graph.
type node struct {
	name string
	expr hcl.Expression

	// deps maps each root variable name this expression references to the
	// node (if any) that produces it. References to externally-provided
	// roots or missing variables have no entry here.
	deps map[string]*node

	// depNames records every referenced root name in the order returned by
	// expr.Variables, including external and missing roots. The graph edges
	// themselves (deps) are order-independent; this slice is retained only
	// for diagnostics.
	depNames []string

	rng hcl.Range

	// State used while evaluating. The node mutex guards the state
	// transition; waiters are signaled through per-call channels so a
	// cancelled caller never blocks inside the mutex.
	mu      sync.Mutex
	state   nodeState
	waiters []*waiter
	value   cty.Value
	diags   hcl.Diagnostics
}

// waiter represents one goroutine blocked on an in-flight node evaluation.
// reset is closed when the owning attempt was abandoned (cancellation) so
// the waiter should retry against its own context rather than inherit the
// owner's cancellation.
type waiter struct {
	done  chan struct{}
	reset chan struct{}
}

type nodeState int

const (
	nodePending nodeState = iota
	nodeRunning
	nodeDone
)

// NewPlan creates a new, empty plan. The given function table is used for
// every function call made while evaluating expressions in the plan; it may
// be nil if no expressions call functions. Functions remain provided by the
// caller, exactly as with [hcl.EvalContext.Functions].
func NewPlan(funcs map[string]function.Function) *Plan {
	return &Plan{
		nodes:   map[string]*node{},
		roots:   map[string]cty.Value{},
		missing: map[string]hcl.Range{},
		funcs:   funcs,
	}
}

// Add registers an expression under the given root variable name. The same
// name may later be referenced by other registered expressions to create a
// dependency edge.
//
// Add returns an error diagnostic (wrapped in hcl.Diagnostics) if the name
// was already registered as an expression or set as a variable. The
// returned diagnostics describe only the registration conflict; graph
// diagnostics such as cycles are reported by [Plan.Analyze] and the
// evaluation methods.
func (p *Plan) Add(name string, expr hcl.Expression) hcl.Diagnostics {
	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, exists := p.nodes[name]; exists {
		return hcl.Diagnostics{duplicateDeclDiag(name, expr.Range(), existing.expr.Range())}
	}
	if val, exists := p.roots[name]; exists {
		return hcl.Diagnostics{duplicateDeclDiag(name, expr.Range(), valueRange(val))}
	}

	n := &node{
		name: name,
		expr: expr,
		deps: map[string]*node{},
		rng:  expr.Range(),
	}
	p.nodes[name] = n
	p.invalidateLocked()

	return nil
}

// SetVariable provides a concrete value for a root variable name. This is
// how externally-known inputs (caller-provided variables) enter the plan.
//
// It returns an error diagnostic if the name is already produced by a
// registered expression or was previously set.
func (p *Plan) SetVariable(name string, val cty.Value) hcl.Diagnostics {
	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, exists := p.nodes[name]; exists {
		return hcl.Diagnostics{duplicateDeclDiag(name, existing.expr.Range(), valueRange(val))}
	}
	if _, exists := p.roots[name]; exists {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Duplicate variable definition",
			Detail:   fmt.Sprintf("The variable %q was already assigned a value in this evaluation plan.", name),
		}}
	}

	p.roots[name] = val
	p.invalidateLocked()
	return nil
}

// invalidateLocked marks the graph as needing re-analysis and clears every
// memoized node result, since adding or redefining a name can change the
// inputs of existing nodes. The plan write lock must be held; callers must
// not mutate a plan while evaluations are in flight.
func (p *Plan) invalidateLocked() {
	p.analyzed = false
	p.cycles = nil
	p.missing = map[string]hcl.Range{}
	for _, n := range p.nodes {
		n.mu.Lock()
		n.state = nodePending
		n.value = cty.NilVal
		n.diags = nil
		for _, w := range n.waiters {
			close(w.reset)
		}
		n.waiters = nil
		n.mu.Unlock()
	}
}

// Analyze resolves the dependency edges between the registered nodes and
// checks the graph for reference cycles and missing variables. It is called
// automatically by the evaluation methods, but may be called explicitly to
// obtain structural diagnostics without evaluating anything.
//
// Repeated calls are cheap: the graph is analyzed only once after the last
// mutation.
func (p *Plan) Analyze() hcl.Diagnostics {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.analyzeLocked()
}

// analyzeLocked builds the dependency edges, the missing-variable set and
// the cycle list. The plan write lock must be held.
func (p *Plan) analyzeLocked() hcl.Diagnostics {
	if p.analyzed {
		return p.structuralDiagsLocked()
	}

	p.missing = map[string]hcl.Range{}
	p.cycles = nil

	// Resolve edges from each expression's referenced root names. Edges are
	// keyed by root name in a map, so the resulting graph is independent of
	// the order in which Variables() happened to report references.
	for _, n := range p.nodes {
		n.deps = map[string]*node{}
		n.depNames = nil
		seen := map[string]struct{}{}

		for _, trav := range n.expr.Variables() {
			root := trav.RootName()
			n.depNames = append(n.depNames, root)
			if _, dup := seen[root]; dup {
				continue
			}
			seen[root] = struct{}{}

			if dep, ok := p.nodes[root]; ok {
				n.deps[root] = dep
			} else if _, ok := p.roots[root]; !ok {
				if rng, ok := p.missing[root]; !ok || rangeLess(trav.SourceRange(), rng) {
					p.missing[root] = trav.SourceRange()
				}
			}
		}
	}

	p.cycles = findCycles(p.nodes)
	p.analyzed = true

	return p.structuralDiagsLocked()
}

// structuralDiagsLocked renders the cached cycle and missing-variable
// information as diagnostics. The plan write lock must be held.
func (p *Plan) structuralDiagsLocked() hcl.Diagnostics {
	var diags hcl.Diagnostics

	// Report cycles deterministically: each cycle chain is already ordered by
	// source range, and the set of cycles is sorted on its first node so the
	// overall output does not depend on map iteration order.
	for _, cyc := range p.cycles {
		diags = append(diags, cycleDiag(p.nodes, cyc))
	}

	missingNames := make([]string, 0, len(p.missing))
	for name := range p.missing {
		missingNames = append(missingNames, name)
	}
	sort.Strings(missingNames)
	for _, name := range missingNames {
		rng := p.missing[name]
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unknown variable",
			Detail:   fmt.Sprintf("There is no variable named %q in the evaluation plan.", name),
			Subject:  rng.Ptr(),
		})
	}

	return diags
}

// Value returns the evaluated value of the expression or variable
// registered under name, evaluating its dependencies lazily as needed.
//
// If name is part of a reference cycle, depends on a missing variable, or
// any of its dependencies fail to evaluate, the returned value is an
// unknown value (cty.DynamicVal unless a more precise type cannot be
// inferred) and error diagnostics explain the reason. Diagnostics from
// dependencies are not duplicated for every dependent: each node's
// diagnostics are collected once and merged into the returned set.
//
// The given context governs cancellation: cancelling it aborts evaluation of
// any node that has not yet started, wakes all waiting goroutines, and
// causes Value to return the context's error as a diagnostic. Partial
// results computed under a cancelled context are discarded rather than
// cached, so a subsequent evaluation with a live context re-runs them.
func (p *Plan) Value(ctx context.Context, name string) (cty.Value, hcl.Diagnostics) {
	p.mu.RLock()
	if !p.analyzed {
		// Upgrade to a write lock for the one-time analysis.
		p.mu.RUnlock()
		p.mu.Lock()
		structDiags := p.analyzeLocked()
		p.mu.Unlock()
		return p.valueAfterAnalyze(ctx, name, structDiags)
	}
	structDiags := p.structuralDiagsLocked()
	p.mu.RUnlock()
	return p.valueAfterAnalyze(ctx, name, structDiags)
}

func (p *Plan) valueAfterAnalyze(ctx context.Context, name string, structDiags hcl.Diagnostics) (cty.Value, hcl.Diagnostics) {
	p.mu.RLock()
	n, isNode := p.nodes[name]
	rootVal, isRoot := p.roots[name]
	_, isMissing := p.missing[name]
	p.mu.RUnlock()

	switch {
	case isNode:
		return p.evalNode(ctx, n, map[string]struct{}{}, structDiags)
	case isRoot:
		return rootVal, structDiags
	case isMissing:
		// The missing-variable diagnostic for this name is already present
		// in the structural diagnostics.
		return cty.DynamicVal, append(hcl.Diagnostics{}, structDiags...)
	default:
		diags := append(hcl.Diagnostics{}, structDiags...)
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unknown plan node",
			Detail:   fmt.Sprintf("The evaluation plan contains no node or variable named %q.", name),
		})
		return cty.DynamicVal, diags
	}
}

// evalNode returns the cached result of n, evaluating it on first request.
// visiting is the set of nodes on the current request's dependency stack,
// used to fail fast if a cycle is reached at evaluation time despite the
// static check (for example, if the plan was mutated without re-analysis).
func (p *Plan) evalNode(ctx context.Context, n *node, visiting map[string]struct{}, structDiags hcl.Diagnostics) (cty.Value, hcl.Diagnostics) {
	// If this node is already on the request's dependency stack the path has
	// closed a cycle. Short-circuit before touching node state so concurrent
	// requests can never deadlock waiting on themselves; the cycle diagnostic
	// itself was produced during analysis.
	if _, cyclic := visiting[n.name]; cyclic {
		return cty.DynamicVal, nil
	}

	// Evaluate every dependency first. Their results are reused across all
	// dependents thanks to the per-node memoization below.
	depVals, depDiags, depsOK := p.evalDeps(ctx, n, visiting, structDiags)

	n.mu.Lock()
	if n.state == nodeDone {
		val, diags := n.value, n.diags
		n.mu.Unlock()
		// Merge dependency diagnostics for this request path even when the
		// node itself was already memoized. A failed node supplies an unknown
		// placeholder rather than being re-evaluated.
		return val, mergeDiags(structDiags, depDiags, diags)
	}
	if n.state == nodeRunning {
		// Another goroutine owns the evaluation. Subscribe to its outcome
		// and release the lock before blocking so the owner can finish.
		w := &waiter{done: make(chan struct{}), reset: make(chan struct{})}
		n.waiters = append(n.waiters, w)
		n.mu.Unlock()

		select {
		case <-w.done:
			// The owner produced a final result.
			n.mu.Lock()
			val, diags := n.value, n.diags
			n.mu.Unlock()
			return val, mergeDiags(structDiags, depDiags, diags)
		case <-w.reset:
			// The owner's attempt was abandoned (its own context was
			// cancelled). Do not inherit that cancellation: retry the node
			// under this caller's own context.
			return p.evalNode(ctx, n, visiting, structDiags)
		case <-ctx.Done():
			// This caller gave up while the owner keeps running. Its result,
			// if it completes, is simply ignored here and remains valid for
			// other readers.
			return cty.DynamicVal, mergeDiags(structDiags, depDiags, hcl.Diagnostics{cancelDiag(ctx)})
		}
	}

	// nodePending: this goroutine owns the evaluation.
	n.state = nodeRunning
	n.mu.Unlock()

	// Evaluate without holding any locks so independent nodes can run in
	// parallel. If the context is already cancelled, do not start at all.
	if err := ctx.Err(); err != nil {
		p.resetNode(n)
		return cty.DynamicVal, mergeDiags(structDiags, depDiags, hcl.Diagnostics{cancelDiag(ctx)})
	}

	if !depsOK {
		// A dependency failed: propagate an unknown value without re-running
		// the expression, and record only the dependency diagnostics rather
		// than generating a fresh error per dependent. The failed state is
		// memoized so every dependent shares this single resolution.
		p.failNode(n, cty.DynamicVal, nil)
		return cty.DynamicVal, mergeDiags(structDiags, depDiags)
	}

	evalCtx := p.evalContext(depVals)
	val, exprDiags := n.expr.Value(evalCtx)

	// Never cache a result produced after the context was cancelled. The
	// node returns to pending so a later live context can retry cleanly.
	if err := ctx.Err(); err != nil {
		p.resetNode(n)
		return cty.DynamicVal, mergeDiags(structDiags, depDiags, hcl.Diagnostics{cancelDiag(ctx)})
	}

	if exprDiags.HasErrors() {
		// Keep the expression's value (often already cty.DynamicVal) but mark
		// the node as failed so dependents do not treat it as a usable input
		// or trigger a second evaluation.
		p.failNode(n, val, exprDiags)
	} else {
		p.completeNode(n, val, exprDiags)
	}
	return val, mergeDiags(structDiags, depDiags, exprDiags)
}

// evalDeps evaluates every dependency of n and returns a variable map
// containing all external roots and dependency values, the merged
// diagnostics, and whether the dependencies evaluated without errors.
// Unknown dependency values are not errors and depsOK remains true.
func (p *Plan) evalDeps(ctx context.Context, n *node, visiting map[string]struct{}, structDiags hcl.Diagnostics) (map[string]cty.Value, hcl.Diagnostics, bool) {
	vars := map[string]cty.Value{}
	var diags hcl.Diagnostics
	ok := true

	// External roots are visible to every expression.
	p.mu.RLock()
	for name, val := range p.roots {
		vars[name] = val
	}
	depOrder := make([]string, 0, len(n.deps))
	for name := range n.deps {
		depOrder = append(depOrder, name)
	}
	p.mu.RUnlock()

	// Stable evaluation order within a single request for reproducible
	// diagnostics; the graph itself and all cached results are order-free.
	sort.Strings(depOrder)

	nextVisiting := map[string]struct{}{}
	for name := range visiting {
		nextVisiting[name] = struct{}{}
	}
	nextVisiting[n.name] = struct{}{}

	// Missing roots have no producer at all. They are reported once by
	// Analyze; supply an unknown placeholder here and stop the expression
	// from re-running so the same missing variable is not diagnosed again
	// from every expression that mentions it.
	p.mu.RLock()
	for _, depName := range n.depNames {
		if _, isMissing := p.missing[depName]; isMissing {
			ok = false
			vars[depName] = cty.DynamicVal
		}
	}
	p.mu.RUnlock()

	for _, depName := range depOrder {
		p.mu.RLock()
		dep := n.deps[depName]
		p.mu.RUnlock()

		if _, cyclic := nextVisiting[depName]; cyclic {
			// Reaching a node on the current stack means this request path
			// traverses a cycle. The cycle itself is reported by Analyze;
			// supply an unknown placeholder so the expression still has a
			// value for every referenced root, without recursing forever.
			ok = false
			vars[depName] = cty.DynamicVal
			continue
		}

		depVal, depDiags := p.evalNode(ctx, dep, nextVisiting, structDiags)
		diags = mergeDiags(diags, depDiags)
		if depDiags.HasErrors() {
			ok = false
		}
		vars[depName] = depVal
	}

	return vars, diags, ok
}

// evalContext builds a fresh EvalContext for one expression evaluation.
// Functions come from the plan; variables from the already-evaluated
// dependency and external-root values.
func (p *Plan) evalContext(vars map[string]cty.Value) *hcl.EvalContext {
	return &hcl.EvalContext{
		Variables: vars,
		Functions: p.funcs,
	}
}

// completeNode marks n as finished with a usable value and wakes every
// goroutine waiting on it.
func (p *Plan) completeNode(n *node, val cty.Value, diags hcl.Diagnostics) {
	n.mu.Lock()
	n.state = nodeDone
	n.value = val
	n.diags = diags
	waiters := n.waiters
	n.waiters = nil
	n.mu.Unlock()

	for _, w := range waiters {
		close(w.done)
	}
}

// failNode marks n as finished with an unusable (unknown) value. Dependents
// see the unknown placeholder and the recorded diagnostics, and the node is
// never evaluated again within the plan.
func (p *Plan) failNode(n *node, val cty.Value, diags hcl.Diagnostics) {
	n.mu.Lock()
	n.state = nodeDone
	n.value = val
	n.diags = diags
	waiters := n.waiters
	n.waiters = nil
	n.mu.Unlock()

	for _, w := range waiters {
		close(w.done)
	}
}

// resetNode returns a node that was interrupted by cancellation to its
// pending state and signals waiters to retry under their own contexts, so no
// half-computed result is cached and the owner's cancellation never leaks to
// unrelated callers.
func (p *Plan) resetNode(n *node) {
	n.mu.Lock()
	n.state = nodePending
	n.value = cty.NilVal
	n.diags = nil
	waiters := n.waiters
	n.waiters = nil
	n.mu.Unlock()

	for _, w := range waiters {
		close(w.reset)
	}
}

// Values evaluates every registered node and variable and returns their
// values keyed by name, along with the merged diagnostics for the whole
// plan. Independent nodes are evaluated concurrently.
//
// The returned map is always fully populated: nodes that could not be
// evaluated appear with cty.DynamicVal. As with [Plan.Value], cancellation
// aborts remaining nodes and is reported via diagnostics.
func (p *Plan) Values(ctx context.Context) (map[string]cty.Value, hcl.Diagnostics) {
	p.mu.Lock()
	structDiags := p.analyzeLocked()
	names := make([]string, 0, len(p.nodes)+len(p.roots))
	for name := range p.nodes {
		names = append(names, name)
	}
	for name := range p.roots {
		names = append(names, name)
	}
	p.mu.Unlock()

	// Sort names so iteration order never affects the result.
	sort.Strings(names)

	var (
		wg     sync.WaitGroup
		resMu  sync.Mutex
		values = make(map[string]cty.Value, len(names))
		diags  = append(hcl.Diagnostics{}, structDiags...)
	)

	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			val, nodeDiags := p.Value(ctx, name)
			resMu.Lock()
			values[name] = val
			diags = mergeDiags(diags, nodeDiags)
			resMu.Unlock()
		}(name)
	}
	wg.Wait()

	return values, dedupeDiags(diags)
}

// Names returns the sorted list of every node and variable name in the plan.
func (p *Plan) Names() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	names := make([]string, 0, len(p.nodes)+len(p.roots))
	for name := range p.nodes {
		names = append(names, name)
	}
	for name := range p.roots {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
