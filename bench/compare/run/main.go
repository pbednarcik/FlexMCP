// Command run measures every library on the scoreboard and compares them
// with benchstat, one column per library. From the repository root:
//
//	go run -C bench/compare/run . [-count 10] [-bench .] [-out dir]
//
// Each library's test binary is built once; then each of count rounds runs
// every library once, in an order that rotates each round, so a machine that
// drifts during the run slows every library alike. Each leg runs in a process
// of its own: a leg run after a parallel one in the same process can pay for
// the garbage collector's stop on Windows, more for a library that collects
// more often, and so would measure the runtime rather than the library.
// Timings need a quiet machine: close other work first.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// libraries are the module directories beside this one, in column order.
var libraries = []string{"flexmcp", "mcpgo", "gosdk"}

func main() {
	count := flag.Int("count", 10, "rounds; each runs every library's benchmarks once")
	bench := flag.String("bench", ".", "go test -bench pattern")
	out := flag.String("out", filepath.Join("..", "results", time.Now().UTC().Format("2006-01-02")), "directory for the raw and compared results")
	flag.Parse()

	dir, err := filepath.Abs(*out)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal(err)
	}
	// The binaries go to a temporary directory, not beside the results.
	build, err := os.MkdirTemp("", "compare-")
	if err != nil {
		fatal(err)
	}
	binaries := make(map[string]string, len(libraries))
	for _, lib := range libraries {
		fmt.Fprintf(os.Stderr, "building %s\n", lib)
		binary := filepath.Join(build, lib+".test")
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		cmd := exec.Command("go", "test", "-c", "-o", binary)
		cmd.Dir = filepath.Join("..", lib)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			fatal(fmt.Errorf("%s: %w", lib, err))
		}
		binaries[lib] = binary
	}

	// The legs are listed by running each once; every library serves the
	// same harness, so the first library's list is every library's.
	listed, err := runLeg(binaries[libraries[0]], libraries[0], *bench, "1x")
	if err != nil {
		fatal(fmt.Errorf("listing the legs: %w", err))
	}
	legs := parseLegs(listed)
	if len(legs) == 0 {
		fatal(fmt.Errorf("no leg matches -bench %q", *bench))
	}

	raw := make(map[string][]byte, len(libraries))
	for round := range *count {
		for _, lib := range roundOrder(libraries, round) {
			fmt.Fprintf(os.Stderr, "round %d of %d: %s\n", round+1, *count, lib)
			for _, leg := range legs {
				results, err := runLeg(binaries[lib], lib, legPattern(leg), "")
				if err != nil {
					fatal(fmt.Errorf("%s, %s, round %d: %w", lib, leg, round+1, err))
				}
				raw[lib] = append(raw[lib], results...)
			}
		}
	}

	if err := os.RemoveAll(build); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
	}

	var labelled []string
	for _, lib := range libraries {
		path := filepath.Join(dir, lib+".txt")
		if err := os.WriteFile(path, normalizePkg(raw[lib]), 0o644); err != nil {
			fatal(err)
		}
		labelled = append(labelled, lib+"="+path)
	}

	cmd := exec.Command("go", append([]string{"tool", "benchstat"}, labelled...)...)
	cmd.Stderr = os.Stderr
	table, err := cmd.Output()
	if err != nil {
		fatal(fmt.Errorf("benchstat: %w", err))
	}
	if err := os.WriteFile(filepath.Join(dir, "benchstat.txt"), table, 0o644); err != nil {
		fatal(err)
	}
	os.Stdout.Write(table)
}

// runLeg runs the benchmarks of lib's binary that pattern matches, once, in
// a process of their own, and returns its output. benchtime, when set, is
// passed on.
func runLeg(binary, lib, pattern, benchtime string) ([]byte, error) {
	args := []string{"-test.run", "^$", "-test.bench", pattern, "-test.benchmem", "-test.count", "1"}
	if benchtime != "" {
		args = append(args, "-test.benchtime", benchtime)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = filepath.Join("..", lib)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// parseLegs returns the benchmark names in out, a go test run's output,
// without their GOMAXPROCS suffix, each once, in the order they ran.
func parseLegs(out []byte) []string {
	var legs []string
	seen := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "Benchmark") {
			continue
		}
		name := fields[0]
		if i := strings.LastIndexByte(name, '-'); i > strings.LastIndexByte(name, '/') {
			if _, err := strconv.Atoi(name[i+1:]); err == nil {
				name = name[:i]
			}
		}
		if !seen[name] {
			seen[name] = true
			legs = append(legs, name)
		}
	}
	return legs
}

// legPattern is the -test.bench pattern that matches leg alone: go test
// splits the pattern on '/' and matches each part against one level of a
// benchmark's name, so every part is quoted and anchored.
func legPattern(leg string) string {
	parts := strings.Split(leg, "/")
	for i, part := range parts {
		parts[i] = "^" + regexp.QuoteMeta(part) + "$"
	}
	return strings.Join(parts, "/")
}

// roundOrder is the order the libraries run in during round: libs rotated by
// round, so each library runs first, and last, equally often.
func roundOrder(libs []string, round int) []string {
	order := make([]string, len(libs))
	for i := range libs {
		order[i] = libs[(i+round)%len(libs)]
	}
	return order
}

// normalizePkg gives every library's results the same pkg line, so benchstat
// lines the libraries up as columns of one table.
func normalizePkg(raw []byte) []byte {
	lines := strings.SplitAfter(string(raw), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "pkg: ") {
			lines[i] = "pkg: compare\n"
		}
	}
	return []byte(strings.Join(lines, ""))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "run:", err)
	os.Exit(1)
}
