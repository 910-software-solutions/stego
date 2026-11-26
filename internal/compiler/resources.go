package compiler

import "github.com/910-software-solutions/stego/internal/gen"

func constructorUsesResource(wiring *gen.Wiring, index int, want gen.Resource) bool {
	for _, resource := range wiring.ConstructorResources[index] {
		if resource == want {
			return true
		}
	}
	return false
}
