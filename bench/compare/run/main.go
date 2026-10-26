// Command run measures every library on the scoreboard and compares them
// with benchstat, one column per library. From the repository root:
//
//	go run -C bench/compare/run . [-count 10] [-bench .] [-out dir]
//
// Each library's test binary is built once; then each of count rounds runs
// every library once, in an order that rotates each round, so a machine that
// drifts during the run slows every library alike. Timings need a quiet
// machine: close other work first.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	raw := make(map[string][]byte, len(libraries))
	for round := range *count {
		for _, lib := range roundOrder(libraries, round) {
			fmt.Fprintf(os.Stderr, "round %d of %d: %s\n", round+1, *count, lib)
			cmd := exec.Command(binaries[lib], "-test.run", "^$", "-test.bench", *bench, "-test.benchmem", "-test.count", "1")
			cmd.Dir = filepath.Join("..", lib)
			cmd.Stderr = os.Stderr
			results, err := cmd.Output()
			if err != nil {
				fatal(fmt.Errorf("%s, round %d: %w", lib, round+1, err))
			}
			raw[lib] = append(raw[lib], results...)
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
