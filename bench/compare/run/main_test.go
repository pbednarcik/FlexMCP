package main

import (
	"strings"
	"testing"
)

// Every library's results get one pkg line, so benchstat lines them up as
// columns of one table instead of printing a table per library.
func TestNormalizePkg(t *testing.T) {
	raw := "goos: darwin\npkg: github.com/pbednarcik/flexmcp/bench/compare/gosdk\nBenchmarkCompare/modern/small/serial-18 \t 1 \t 100 ns/op\n"
	want := "goos: darwin\npkg: compare\nBenchmarkCompare/modern/small/serial-18 \t 1 \t 100 ns/op\n"
	if got := string(normalizePkg([]byte(raw))); got != want {
		t.Errorf("normalizePkg:\n got %q\nwant %q", got, want)
	}
}

// The legs are read from a run's benchmark lines, without the GOMAXPROCS
// suffix, each once, in the order they ran.
func TestParseLegs(t *testing.T) {
	out := "goos: windows\npkg: x\n" +
		"BenchmarkCompare/modern/small/serial-16   \t1\t100 ns/op\n" +
		"BenchmarkCompare/modern/small/parallel-16 \t1\t100 ns/op\n" +
		"BenchmarkCompare/modern/small/serial-16   \t1\t100 ns/op\n" +
		"BenchmarkCompare/legacy/complex/serial    \t1\t100 ns/op\n" +
		"PASS\n"
	got := parseLegs([]byte(out))
	want := []string{"BenchmarkCompare/modern/small/serial", "BenchmarkCompare/modern/small/parallel", "BenchmarkCompare/legacy/complex/serial"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("parseLegs:\n got %q\nwant %q", got, want)
	}
}

// A leg's pattern matches that leg alone: go test splits -bench on '/' and
// matches each part against one level of the name, so every part is anchored.
func TestLegPattern(t *testing.T) {
	if got, want := legPattern("BenchmarkCompare/modern/small/serial"), "^BenchmarkCompare$/^modern$/^small$/^serial$"; got != want {
		t.Errorf("legPattern: got %q, want %q", got, want)
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
