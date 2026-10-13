// Command run measures every library on the scoreboard and compares them
// with benchstat, one column per library. From the repository root:
//
//	go run -C bench/compare/run . [-count 10] [-bench .] [-out dir]
//
// Timings need a quiet machine: close other work first.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// libraries are the module directories beside this one, in column order.
var libraries = []string{"flexmcp", "mcpgo", "gosdk"}

func main() {
	count := flag.Int("count", 10, "go test -count for every library")
	bench := flag.String("bench", ".", "go test -bench pattern")
	out := flag.String("out", filepath.Join("..", "results", time.Now().UTC().Format("2006-01-02")), "directory for the raw and compared results")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	var labelled []string
	for _, lib := range libraries {
		fmt.Fprintf(os.Stderr, "measuring %s (-count %d)\n", lib, *count)
		cmd := exec.Command("go", "test", "-run", "^$", "-bench", *bench, "-benchmem", "-count", strconv.Itoa(*count))
		cmd.Dir = filepath.Join("..", lib)
		cmd.Stderr = os.Stderr
		raw, err := cmd.Output()
		if err != nil {
			fatal(fmt.Errorf("%s: %w", lib, err))
		}
		path := filepath.Join(*out, lib+".txt")
		if err := os.WriteFile(path, normalizePkg(raw), 0o644); err != nil {
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
	if err := os.WriteFile(filepath.Join(*out, "benchstat.txt"), table, 0o644); err != nil {
		fatal(err)
	}
	os.Stdout.Write(table)
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
