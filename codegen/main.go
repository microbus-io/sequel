/*
Copyright (c) 2025-2026 Microbus LLC and various contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/microbus-io/sequel/codegen/generator"
)

// main is executed when "go generate" is run in the current working directory.
func main() {
	// Load flags from environment variable because can't pass arguments to code-generator
	var flagForce bool
	var flagVerbose bool
	envVal := os.Getenv("MICROBUS_CODEGEN")
	if envVal == "" {
		envVal = os.Getenv("CODEGEN")
	}
	flags := flag.NewFlagSet("", flag.ContinueOnError)
	flags.BoolVar(&flagForce, "f", false, "Force processing even if no change detected")
	flags.BoolVar(&flagVerbose, "v", false, "Verbose output")
	_ = flags.Parse(strings.Split(envVal, " "))

	// Run generator
	gen, err := generator.NewGenerator()
	if err == nil {
		gen.Force = flagForce
		gen.Verbose = flagVerbose
		fmt.Fprintf(os.Stdout, "[sequel] %s\n", gen.PackagePath)
		gen.Indent()
		err = gen.Run()
		gen.Unindent()
	}
	if err != nil {
		if flagVerbose {
			fmt.Fprintf(os.Stderr, "%+v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
		os.Exit(-1)
	}
}
