package skills

import (
	"fmt"
	"strings"
)

const (
	diffContext  = 3
	maxDiffCells = 4_000_000
	maxDiffBytes = 64 << 10
)

// UnifiedDiff is a unified diff of two text files; binary or very large files get a one-line summary instead.
func UnifiedDiff(name string, a, b []byte) string {
	if !isText(a) || !isText(b) {
		return fmt.Sprintf("Binary file %s changed (%d -> %d bytes)\n", name, len(a), len(b))
	}

	x, y := splitLines(string(a)), splitLines(string(b))

	if len(x)*len(y) > maxDiffCells {
		return fmt.Sprintf("%s changed (%d -> %d lines; too large to show here)\n", name, len(x), len(y))
	}

	ops := lineOps(x, y)

	var out strings.Builder

	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", name, name)

	for start := 0; start < len(ops); {
		for start < len(ops) && ops[start].kind == ' ' {
			start++
		}

		if start == len(ops) {
			break
		}

		lo := max(0, start-diffContext)
		end, quiet := start, 0

		for end < len(ops) && quiet <= 2*diffContext {
			if ops[end].kind == ' ' {
				quiet++
			} else {
				quiet = 0
			}

			end++
		}

		hi := min(len(ops), end-max(0, quiet-diffContext))
		ai, bi, an, bn := ops[lo].a, ops[lo].b, 0, 0

		for _, op := range ops[lo:hi] {
			if op.kind != '+' {
				an++
			}

			if op.kind != '-' {
				bn++
			}
		}

		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", ai+1, an, bi+1, bn)

		for _, op := range ops[lo:hi] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			out.WriteByte('\n')
		}

		if out.Len() > maxDiffBytes {
			out.WriteString("... (diff truncated)\n")

			break
		}

		start = hi
	}

	return out.String()
}

type lineOp struct {
	kind byte
	text string
	a, b int
}

// lineOps aligns two line lists by their longest common subsequence.
func lineOps(x, y []string) []lineOp {
	n, m := len(x), len(y)
	lcs := make([][]int32, n+1)

	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}

	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	var ops []lineOp

	i, j := 0, 0

	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			ops = append(ops, lineOp{' ', x[i], i, j})
			i, j = i+1, j+1
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, lineOp{'-', x[i], i, j})
			i++
		default:
			ops = append(ops, lineOp{'+', y[j], i, j})
			j++
		}
	}

	return ops
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}

	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
