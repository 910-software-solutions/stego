// Command notatest calls fill.UseTestModuleSources from a program that is not a test binary, which must panic
// (ST-STD-LOCAL-R03-T21). It is in testdata, so no build of the module reaches it; the test builds it by its path.
package main

import "github.com/910-software-solutions/stego/internal/fill"

func main() {
	_, _ = fill.UseTestModuleSources("file:///nonexistent")
}
