package notesync

// DiffOp is the role of a line in a unified line diff.
type DiffOp int

const (
	diffContext DiffOp = iota // unchanged: present on both sides
	diffAdd                   // present only on the new (local) side
	diffDel                   // present only on the old (remote) side
)

// DiffLine is one line of a unified line diff.
type DiffLine struct {
	Op   DiffOp
	Text string
}

// Class returns the CSS modifier for the line ("add"/"del"/"context"), so the
// template can style it without referencing the unexported DiffOp constants.
func (d DiffLine) Class() string {
	switch d.Op {
	case diffAdd:
		return "add"
	case diffDel:
		return "del"
	default:
		return "context"
	}
}

// Sign returns the leading marker for the line ("+", "-", or " "), mirroring a
// unified diff so the change is legible without relying on colour alone.
func (d DiffLine) Sign() string {
	switch d.Op {
	case diffAdd:
		return "+"
	case diffDel:
		return "-"
	default:
		return " "
	}
}

// LineDiff computes a minimal unified line diff between a (old/remote) and
// b (new/local) using the classic longest-common-subsequence table. Lines
// common to both are emitted as diffContext, lines only in a as diffDel, and
// lines only in b as diffAdd, ordered so the result reconstructs both inputs.
func LineDiff(a, b []string) []DiffLine {
	n, m := len(a), len(b)

	// lcs[i][j] = length of the LCS of a[i:] and b[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var out []DiffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{diffContext, a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, DiffLine{diffDel, a[i]})
			i++
		default:
			out = append(out, DiffLine{diffAdd, b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, DiffLine{diffDel, a[i]})
	}
	for ; j < m; j++ {
		out = append(out, DiffLine{diffAdd, b[j]})
	}
	return out
}
