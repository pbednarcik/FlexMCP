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

// Over as many rounds as there are libraries, each library runs in every
// position once, so none is always first while the machine is still cool.
func TestRoundOrderRotates(t *testing.T) {
	libs := []string{"a", "b", "c"}
	positions := map[string]map[int]bool{}
	for round := range len(libs) {
		order := roundOrder(libs, round)
		if len(order) != len(libs) {
			t.Fatalf("round %d: %v", round, order)
		}
		for i, lib := range order {
			if positions[lib] == nil {
				positions[lib] = map[int]bool{}
			}
			positions[lib][i] = true
		}
	}
	for _, lib := range libs {
		if len(positions[lib]) != len(libs) {
			t.Errorf("%s ran in positions %v, want every one of %d", lib, positions[lib], len(libs))
		}
	}
}
