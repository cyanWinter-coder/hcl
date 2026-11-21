# evalplan: lazy, dependency-driven HCL evaluation

`ext/evalplan` evaluates a set of HCL expressions whose variables may
themselves be defined by other expressions, using a lazily evaluated
dependency graph. It is an opt-in extension: the core `hcl`, `hclsyntax`
and `hcldec` packages are unchanged, and existing `Decode`,
`PartialDecode` and diagnostic-rendering behavior keeps working.

## Why

In infrastructure configuration, a large number of blocks reference
variables defined by other blocks. Evaluating block by block means that a
single unknown or missing variable fans out into many repeated errors,
while a reference cycle shows up only as recursion or a timeout with no
hint of the real dependency chain. `evalplan` addresses this by planning
the evaluation first.

## Building a plan

Register derived expressions with `Add`, seed externally known values with
`SetVariable`, and pass the caller-provided function table to `NewPlan`:

```go
plan := evalplan.NewPlan(functions)
plan.SetVariable("base", cty.NumberIntVal(10))
plan.Add("doubled", expr1)  // e.g. base * 2
plan.Add("combined", expr2) // e.g. doubled + base

val, diags := plan.Value(ctx, "combined")
```

Edges are derived from each expression's `Variables()` traversals, so the
graph is constructed from real references rather than declaration order.

## Guarantees

1. **DAG with single evaluation.** Each node is evaluated at most once; a
   successful result is reused by every dependent node across blocks.
   Diamond and fan-in dependencies share the single cached result.
2. **Unknowns propagate as unknowns, never null.** A node depending on an
   unknown cty value evaluates to an unknown value. Nodes unrelated to the
   unknown or failing dependency still produce concrete, deliverable
   results, and only the directly affected outputs are reported.
3. **Cycles and other diagnostics together.** Reference cycles are detected
   statically and returned as a complete chain ordered by source range
   (`CycleExtra` in the diagnostic's `Extra`). Missing variables and type
   errors appear in the same `hcl.Diagnostics` collection.
4. **Order-independent results.** Graph edges use keyed maps, output maps
   are keyed by name, and list/tuple construction follows expression
   semantics; internal traversal and map iteration order cannot change
   results. Functions are always supplied by the caller, exactly as with
   `hcl.EvalContext`.
5. **Cancellation-aware.** Cancelling the context stops any node that has
   not yet started, immediately wakes goroutines waiting on a dependency,
   and never caches a half-finished result. Cancellation of one caller does
   not leak to other readers: live waiters retry the node under their own
   context.
6. **Safe concurrent reads and hcldec compatible.** Any number of goroutines
   may call `Value`/`Values` concurrently (independent nodes evaluate in
   parallel). `Decode`, `PartialDecode` and the standard diagnostic
   coloring path continue to work through the hcldec integration.

## hcldec integration

`DecodeBody` extracts variable-defining blocks (for example labeled
`locals` blocks whose `expression` attribute defines the named variable),
registers them as plan nodes, evaluates the DAG, and then decodes the
remaining body with `hcldec` using an `hcl.EvalContext` populated from the
evaluated values:

```go
result, values, leftovers, diags := evalplan.DecodeBody(
    ctx, body, spec,
    &evalplan.DecodeOptions{
        VariableBlockType:  "locals",
        ExpressionAttrName: "expression",
        Variables:          externalVars,
        Functions:          functions,
    },
    partial,
)
```

`Decode` and `PartialDecode` are convenience wrappers mirroring the
`hcldec` entry points. Structural plan diagnostics (cycles, missing
variables) and hcldec diagnostics (including type errors) are merged into
one set, and every diagnostic keeps its source range for the existing
coloring behavior.
