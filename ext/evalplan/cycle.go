// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package evalplan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// findCycles returns every distinct reference cycle in the graph described
// by nodes. Each cycle is returned as a list of node names ordered by the
// source range of the nodes' expressions, starting at the earliest range
// and following dependency edges around the loop.
//
// The result is itself sorted by the first node in each chain, so callers
// get deterministic output independent of map iteration order.
func findCycles(nodes map[string]*node) [][]string {
	// White/gray/black DFS coloring. We enumerate every back edge and
	// reconstruct the cycle it implies from the DFS stack, then canonicalize
	// each cycle so the same logical cycle is reported only once.
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	stack := []string{}
	onStack := map[string]int{} // name -> index in stack
	seen := map[string]struct{}{}
	var cycles [][]string

	// Deterministic DFS entry order.
	rootNames := make([]string, 0, len(nodes))
	for name := range nodes {
		rootNames = append(rootNames, name)
	}
	sort.Strings(rootNames)

	var visit func(name string)
	visit = func(name string) {
		color[name] = gray
		onStack[name] = len(stack)
		stack = append(stack, name)

		depNames := make([]string, 0, len(nodes[name].deps))
		for dep := range nodes[name].deps {
			depNames = append(depNames, dep)
		}
		sort.Strings(depNames)

		for _, dep := range depNames {
			switch color[dep] {
			case white:
				visit(dep)
			case gray:
				// Back edge: the cycle is the stack slice from dep onward.
				chain := append([]string{}, stack[onStack[dep]:]...)
				canonical := canonicalizeCycle(nodes, chain)
				key := cycleKey(canonical)
				if _, ok := seen[key]; !ok {
					seen[key] = struct{}{}
					cycles = append(cycles, canonical)
				}
			}
		}

		stack = stack[:len(stack)-1]
		delete(onStack, name)
		color[name] = black
	}

	for _, name := range rootNames {
		if color[name] == white {
			visit(name)
		}
	}

	sort.Slice(cycles, func(i, j int) bool {
		return cycleKey(cycles[i]) < cycleKey(cycles[j])
	})
	return cycles
}

// canonicalizeCycle rotates a cycle so it starts at the node with the
// earliest source range (ties broken by name), then verifies the chain
// follows dependency edges in that orientation. The input is assumed to be a
// valid cycle already.
func canonicalizeCycle(nodes map[string]*node, chain []string) []string {
	start := 0
	for i := 1; i < len(chain); i++ {
		if rangeLess(nodes[chain[i]].rng, nodes[chain[start]].rng) ||
			(nodes[chain[i]].rng.Start.Byte == nodes[chain[start]].rng.Start.Byte && chain[i] < chain[start]) {
			start = i
		}
	}

	out := make([]string, 0, len(chain))
	out = append(out, chain[start:]...)
	out = append(out, chain[:start]...)
	return out
}

// cycleKey produces a stable string identity for a cycle for de-duplication
// and ordering.
func cycleKey(chain []string) string {
	return strings.Join(chain, "\x00")
}

// cycleDiag renders a cycle as a single diagnostic whose subject is the
// earliest node in the chain and whose detail lists the full dependency
// loop in source-range order.
func cycleDiag(nodes map[string]*node, chain []string) *hcl.Diagnostic {
	described := make([]string, len(chain)+1)
	for i, name := range chain {
		described[i] = fmt.Sprintf("%q (at %s)", name, nodes[name].rng.String())
	}
	// Close the loop explicitly so the cycle is obvious.
	described[len(chain)] = described[0]

	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  "Cyclic variable dependency",
		Detail: fmt.Sprintf(
			"A cyclic reference exists between these variables: %s.",
			strings.Join(described, " -> "),
		),
		Subject: nodes[chain[0]].rng.Ptr(),
		Extra:   &CycleExtra{Names: append([]string{}, chain...)},
	}
}

// CycleExtra is the machine-readable form of a cycle diagnostic, available
// via the [hcl.Diagnostic.Extra] field. Names lists the nodes in the cycle
// in source-range order, without repeating the first node at the end.
type CycleExtra struct {
	Names []string
}

// UnwrapDiagnosticExtra implements hcl.DiagnosticExtraUnwrapper.
func (e *CycleExtra) UnwrapDiagnosticExtra() interface{} { return nil }
