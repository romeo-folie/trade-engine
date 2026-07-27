package main

import "testing"

func TestRootCommandNamesAreUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for _, command := range rootCmd.Commands() {
		name := command.Name()
		if _, duplicate := seen[name]; duplicate {
			t.Fatalf("root command %q is registered more than once", name)
		}
		seen[name] = struct{}{}
	}
}
