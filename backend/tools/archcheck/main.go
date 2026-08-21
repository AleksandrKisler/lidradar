// Command archcheck enforces the dependency boundaries of backend modules.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	root := flag.String("root", ".", "backend module root to inspect")
	flag.Parse()

	violations, err := checkTree(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archcheck: %v\n", err)
		os.Exit(2)
	}
	for _, violation := range violations {
		fmt.Fprintln(os.Stderr, violation)
	}
	if len(violations) != 0 {
		os.Exit(1)
	}
}
