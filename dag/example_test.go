package dag_test

import (
	"errors"
	"fmt"

	"github.com/richinsley/goomba/dag"
)

// Link order for a few conda packages: dimension 0 carries an edge from
// each dependency to its dependent; an edge that would close a cycle there
// moves to dimension 1.
func Example() {
	deps := [][2]string{ // dependency, dependent
		{"libzlib", "openssl"},
		{"openssl", "python"},
		{"libzlib", "python"},
		{"python", "pip"},
		{"pip", "python"}, // python requires pip too
	}
	g := dag.New[string](2)
	for _, k := range []string{"python", "pip", "openssl", "libzlib"} {
		g.AddNode(k)
	}
	for _, e := range deps {
		err := g.Connect(0, e[0], e[1])
		var ce *dag.CycleError[string]
		if errors.As(err, &ce) {
			fmt.Println("moved to dimension 1:", ce.From, "->", ce.To, "closes", ce.Path)
			err = g.Connect(1, e[0], e[1])
		}
		if err != nil {
			panic(err)
		}
	}
	for i, layer := range g.Layers(0) {
		fmt.Println("layer", i, layer)
	}
	fmt.Println("impact of libzlib:", g.DescendentsAcross([]int{0, 1}, "libzlib"))
	// Output:
	// moved to dimension 1: pip -> python closes [python pip]
	// layer 0 [libzlib]
	// layer 1 [openssl]
	// layer 2 [python]
	// layer 3 [pip]
	// impact of libzlib: [openssl python pip]
}
