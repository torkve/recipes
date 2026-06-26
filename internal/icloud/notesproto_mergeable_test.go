package icloud

import (
	"encoding/hex"
	"reflect"
	"testing"
)

// Golden inflated top-level protos captured from real iCloud Notes web-app saves
// (paste-note.har): a plain multi-paragraph create and the same note after the two
// ingredient lines were turned into a checklist. Our encoder must reproduce these
// byte-for-byte given the same content and object UUIDs.
const (
	goldenReplicaUUID = "308055bfc10dffcefd9def5f1801d15c"
	goldenItem1UUID   = "23b486b19d22db95b57273c49a6b9482"
	goldenItem2UUID   = "f9b62d26799932355cd4a997f357ea8f"

	goldenPlainProto = "080012cb01080010001ac401125a5465737420526563697065205469746c650a4974656d206f6e650a4974656d2074776f0a466972737420706172616772617068206f662073746570732e0a5365636f6e6420706172616772617068206f662073746570732e0a0a1a100a040800100010001a040800100028011a100a0408011000105a1a040801100028021a160a08080010ffffffff0f10001a08080010ffffffff0f221c0a1a0a10308055bfc10dffcefd9def5f1801d15c1202085a120208012a0208592a080801120408001004"

	goldenChecklistProto = "080012b302080010001aac02125a5465737420526563697065205469746c650a4974656d206f6e650a4974656d2074776f0a466972737420706172616772617068206f662073746570732e0a5365636f6e6420706172616772617068206f662073746570732e0a0a1a100a040800100010001a040800100028011a100a040801100010121a040801100028021a100a040801101210121a040801100128031a100a040801102410361a040801100028041a160a08080010ffffffff0f10001a08080010ffffffff0f221c0a1a0a10308055bfc10dffcefd9def5f1801d15c1202085a120208022a0208122a1e0809121a086710042a140a1023b486b19d22db95b57273c49a6b948210002a1e0809121a086710042a140a10f9b62d26799932355cd4a997f357ea8f10002a0208352a080801120408001004"
)

// withUUIDs swaps newNoteUUID for a deterministic sequence for the duration of fn.
func withUUIDs(t *testing.T, hexUUIDs []string, fn func()) {
	t.Helper()
	saved := newNoteUUID
	t.Cleanup(func() { newNoteUUID = saved })
	i := 0
	newNoteUUID = func() []byte {
		if i >= len(hexUUIDs) {
			t.Fatalf("encoder requested more UUIDs than provided (%d)", len(hexUUIDs))
		}
		b, err := hex.DecodeString(hexUUIDs[i])
		if err != nil {
			t.Fatal(err)
		}
		i++
		return b
	}
	fn()
}

func TestBuildMergeableNoteProtoGoldenPlain(t *testing.T) {
	// The plain create: title + 4 plain paragraphs, no checklists.
	withUUIDs(t, []string{goldenReplicaUUID}, func() {
		got := buildMergeableNoteProto(
			"Test Recipe Title", nil,
			"Item one\nItem two\nFirst paragraph of steps.\nSecond paragraph of steps.")
		if h := hex.EncodeToString(got); h != goldenPlainProto {
			t.Fatalf("plain proto mismatch:\n got=%s\nwant=%s", h, goldenPlainProto)
		}
	})
}

func TestBuildMergeableNoteProtoGoldenChecklist(t *testing.T) {
	// Two ingredient checklist items + two step paragraphs.
	withUUIDs(t, []string{goldenReplicaUUID, goldenItem1UUID, goldenItem2UUID}, func() {
		got := buildMergeableNoteProto(
			"Test Recipe Title",
			[][]string{{"Item one", "Item two"}},
			"First paragraph of steps.\nSecond paragraph of steps.")
		if h := hex.EncodeToString(got); h != goldenChecklistProto {
			t.Fatalf("checklist proto mismatch:\n got=%s\nwant=%s", h, goldenChecklistProto)
		}
	})
}

// encodeMergeableNoteBody must round-trip through parseMergeableNoteBody: pushed
// ingredient checklists come back as ingredient blocks and steps as steps.
func TestEncodeMergeableRoundTrip(t *testing.T) {
	cases := []struct {
		name       string
		title      string
		blocks     [][]string
		steps      string
		wantBlocks [][]string
		wantSteps  string
	}{
		{"ingredients+steps", "Брамбораки", [][]string{{"Картофель", "Мука"}},
			"Натереть\nСмешать всё", [][]string{{"Картофель", "Мука"}}, "Натереть\nСмешать всё"},
		{"subtitle", "Блинчики", [][]string{{"# Тесто", "Мука", "Молоко"}},
			"Готовим", [][]string{{"# Тесто", "Мука", "Молоко"}}, "Готовим"},
		{"steps only", "Чай", nil, "Вскипятить\nЗаварить", nil, "Вскипятить\nЗаварить"},
		{"ingredients only", "Салат", [][]string{{"Огурцы", "Помидоры"}}, "",
			[][]string{{"Огурцы", "Помидоры"}}, ""},
		{"two checklist blocks", "Торт", [][]string{{"# Корж", "Мука"}, {"# Крем", "Сахар"}},
			"Испечь", [][]string{{"# Корж", "Мука"}, {"# Крем", "Сахар"}}, "Испечь"},
		{"multibyte", "Cveće — zla", [][]string{{"prolećа"}}, "Pećenje — BSK",
			[][]string{{"prolećа"}}, "Pećenje — BSK"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blob, err := encodeMergeableNoteBody(tc.title, tc.blocks, tc.steps)
			if err != nil {
				t.Fatal(err)
			}
			blocks, steps, ok := parseMergeableNoteBody(blob)
			if !ok {
				t.Fatal("parseMergeableNoteBody failed")
			}
			if !reflect.DeepEqual(blocks, tc.wantBlocks) {
				t.Errorf("blocks = %v, want %v", blocks, tc.wantBlocks)
			}
			if steps != tc.wantSteps {
				t.Errorf("steps = %q, want %q", steps, tc.wantSteps)
			}
		})
	}
}

// The compressed body must be zlib (eJw header), matching the web app.
func TestEncodeMergeableIsZlib(t *testing.T) {
	blob, err := encodeMergeableNoteBody("T", nil, "step")
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) < 2 || blob[0] != 0x78 || blob[1] != 0x9c {
		t.Fatalf("expected zlib header, got % x", blob[:2])
	}
}
