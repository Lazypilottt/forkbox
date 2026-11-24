//go:build !linux

package main

import "errors"

func pagesrvRun(string, []string, int, int, string, string, bool) error {
	return errors.New("the page server needs Linux (userfaultfd)")
}
