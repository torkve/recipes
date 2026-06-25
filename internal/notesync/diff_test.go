package notesync

import (
	"strings"
	"testing"
)

// render flattens a diff to a compact string like "=a -b +c" so cases read
// clearly (= context, - deletion, + addition).
func render(lines []DiffLine) string {
	var parts []string
	for _, l := range lines {
		var sign byte
		switch l.Op {
		case diffContext:
			sign = '='
		case diffAdd:
			sign = '+'
		case diffDel:
			sign = '-'
		}
		parts = append(parts, string(sign)+l.Text)
	}
	return strings.Join(parts, " ")
}

func TestLineDiff(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want string
	}{
		{"both empty", nil, nil, ""},
		{"identical", []string{"x", "y"}, []string{"x", "y"}, "=x =y"},
		{"all add (empty old)", nil, []string{"a", "b"}, "+a +b"},
		{"all del (empty new)", []string{"a", "b"}, nil, "-a -b"},
		{"replace one", []string{"a", "b", "c"}, []string{"a", "B", "c"}, "=a -b +B =c"},
		{"insert middle", []string{"a", "c"}, []string{"a", "b", "c"}, "=a +b =c"},
		{"delete middle", []string{"a", "b", "c"}, []string{"a", "c"}, "=a -b =c"},
		{"append tail", []string{"a"}, []string{"a", "b", "c"}, "=a +b +c"},
		{"disjoint", []string{"a", "b"}, []string{"c", "d"}, "-a -b +c +d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := render(LineDiff(tc.a, tc.b))
			if got != tc.want {
				t.Fatalf("LineDiff(%v, %v) = %q, want %q", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestDiffLineClassAndSign(t *testing.T) {
	for _, tc := range []struct {
		op          DiffOp
		class, sign string
	}{
		{diffContext, "context", " "},
		{diffAdd, "add", "+"},
		{diffDel, "del", "-"},
	} {
		l := DiffLine{Op: tc.op, Text: "t"}
		if l.Class() != tc.class || l.Sign() != tc.sign {
			t.Fatalf("op %d: class=%q sign=%q, want %q/%q", tc.op, l.Class(), l.Sign(), tc.class, tc.sign)
		}
	}
}
