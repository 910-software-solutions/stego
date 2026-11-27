// Command notatest calls appbuild.UseTestModuleSources from a program that is not a test binary, which must panic
// (ST-STD-LOCAL-R03-T07). It is in testdata, so no build of the module reaches it; the test builds it by its path.
package main

import "github.com/910-software-solutions/stego/internal/appbuild"

func main() {
	_, _ = appbuild.UseTestModuleSources("http://127.0.0.1:1", "stego-notatest+00000000+AAAA", "http://127.0.0.1:2")
}
