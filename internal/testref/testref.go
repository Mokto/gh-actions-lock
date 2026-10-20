// Package testref builds typed lockfile refs from literals in tests.
package testref

import parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"

// Pin parses s as a pin ref and panics if it is invalid.
func Pin(s string) parserlock.PinRef {
	r, err := parserlock.ParsePinRef(s)
	if err != nil {
		panic(err)
	}
	return r
}

// Name parses s as a tag or branch name and panics if it is invalid.
func Name(s string) parserlock.RefName {
	n, err := parserlock.ParseRefName(s)
	if err != nil {
		panic(err)
	}
	return n
}
