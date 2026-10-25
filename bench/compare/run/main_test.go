package main

import "testing"

// Every library's results get one pkg line, so benchstat lines them up as
// columns of one table instead of printing a table per library.
func TestNormalizePkg(t *testing.T) {
	raw := "goos: darwin\npkg: github.com/pbednarcik/flexmcp/bench/compare/gosdk\nBenchmarkCompare/modern/small/serial-18 \t 1 \t 100 ns/op\n"
	want := "goos: darwin\npkg: compare\nBenchmarkCompare/modern/small/serial-18 \t 1 \t 100 ns/op\n"
	if got := string(normalizePkg([]byte(raw))); got != want {
		t.Errorf("normalizePkg:\n got %q\nwant %q", got, want)
	}
}
