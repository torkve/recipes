package icloud

import (
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// Golden inflated top-level protos: a plain multi-paragraph create and the same note
// with the two ingredient lines as a checklist. The body layout matches real iCloud
// Notes web-app saves (paste-note.har); the title's first f5 entry additionally carries
// the Apple "Title" paragraph style ({style_type:0, f2:4}) so pushed notes render their
// first line as a title like device-authored notes (the paste capture left it unstyled).
// Our encoder must reproduce these byte-for-byte given the same content and object UUIDs.
const (
	goldenReplicaUUID = "308055bfc10dffcefd9def5f1801d15c"
	goldenItem1UUID   = "23b486b19d22db95b57273c49a6b9482"
	goldenItem2UUID   = "f9b62d26799932355cd4a997f357ea8f"

	goldenPlainProto = "080012d501080010001ace01125a5465737420526563697065205469746c650a4974656d206f6e650a4974656d2074776f0a466972737420706172616772617068206f662073746570732e0a5365636f6e6420706172616772617068206f662073746570732e0a0a1a100a040800100010001a040800100028011a100a0408011000105a1a040801100028021a160a08080010ffffffff0f10001a08080010ffffffff0f221c0a1a0a10308055bfc10dffcefd9def5f1801d15c1202085a120208012a0808121204080010042a0208472a080801120408001004"

	goldenChecklistProto = "080012b902080010001ab202125a5465737420526563697065205469746c650a4974656d206f6e650a4974656d2074776f0a466972737420706172616772617068206f662073746570732e0a5365636f6e6420706172616772617068206f662073746570732e0a0a1a100a040800100010001a040800100028011a100a040801100010121a040801100028021a100a040801101210121a040801100128031a100a040801102410361a040801100028041a160a08080010ffffffff0f10001a08080010ffffffff0f221c0a1a0a10308055bfc10dffcefd9def5f1801d15c1202085a120208022a0808121204080010042a1e0809121a086710042a140a1023b486b19d22db95b57273c49a6b948210002a1e0809121a086710042a140a10f9b62d26799932355cd4a997f357ea8f10002a0208352a080801120408001004"
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
			"Item one\nItem two\nFirst paragraph of steps.\nSecond paragraph of steps.", nil, nil)
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
			"First paragraph of steps.\nSecond paragraph of steps.", nil, nil)
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
			blob, err := encodeMergeableNoteBody(tc.title, tc.blocks, tc.steps, nil, nil)
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

// An inline image becomes a U+FFFC paragraph + an f5 attachment entry that
// parseNoteBody reads back as an @@IMG:<attachmentID>@@ marker with the right id.
func TestEncodeMergeableInlineImage(t *testing.T) {
	att := "CF8663E7-8ED6-44DA-8B6A-B508AAFBC808"
	blob, err := encodeMergeableNoteBody(
		"Торт",
		[][]string{{"Мука"}},
		"Шаг один\n@@IMG:photo.jpg@@\nШаг два",
		map[string]mImage{"photo.jpg": {attachmentID: att, uti: "public.jpeg"}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	blocks, steps, imageIDs, ok := parseNoteBody(blob)
	if !ok {
		t.Fatal("parseNoteBody failed on body with inline image")
	}
	if len(blocks) != 1 || len(blocks[0]) != 1 || blocks[0][0] != "Мука" {
		t.Fatalf("blocks = %v, want [[Мука]]", blocks)
	}
	if !reflect.DeepEqual(imageIDs, []string{att}) {
		t.Fatalf("imageIDs = %v, want [%s]", imageIDs, att)
	}
	if !strings.Contains(steps, "@@IMG:"+att+"@@") {
		t.Fatalf("steps missing image marker: %q", steps)
	}
	if !strings.Contains(steps, "Шаг один") || !strings.Contains(steps, "Шаг два") {
		t.Fatalf("steps missing text around image: %q", steps)
	}
}

// The compressed body must be zlib (eJw header), matching the web app.
func TestEncodeMergeableIsZlib(t *testing.T) {
	blob, err := encodeMergeableNoteBody("T", nil, "step", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) < 2 || blob[0] != 0x78 || blob[1] != 0x9c {
		t.Fatalf("expected zlib header, got % x", blob[:2])
	}
}

// Real f4 object-table fixtures from image-note.har (a 2- and a 5-replica note).
// Only replica UUIDs and integer counters — no recipe content. They validate the
// raw-f4 accessors against real Apple data.
const (
	// note fc07639b "Пельмени": replicas [1198,40] and [1198,39].
	goldenF4Pelmeni = "0a1b0a1068cc87f9207f62445defc2eaab6caffa120308ae09120208280a1b0a10e9acd937a10538b9c60b2451e0421aa0120308ae0912020827"
	// note b5b1602f "Ajo blanco": 5 replicas.
	goldenF4Ajo = "0a1b0a1016c5d09159694c7c4fe4528070eae61c120308ea081202084c0a1b0a10334a1a4bf37e8f503eda14e594ba94cb120308c2061202083f0a1b0a10485b26e4ebc14c6c9538c5d0a3a25244120308e9081202084e0a1b0a1079563a41ea2d1f5584d36124ac7f91ff120308c106120208460a1b0a10929e4a8661e5467cace6040778bbfaa8120308c10612020845"
)

// objectTableEntries splits a raw f4 table (the bytes of Note.object_table) into its
// per-replica entries, mirroring what parseObjectTable does on a full inflated body.
func objectTableEntries(t *testing.T, tableHex string) [][]byte {
	t.Helper()
	tbl, err := hex.DecodeString(tableHex)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	pbEachBytes(tbl, 1, func(e []byte) { out = append(out, append([]byte(nil), e...)) })
	return out
}

// The raw-f4 accessors must read real Apple object tables: entry count, the 16-byte
// replica UUIDs, and the two counters per replica.
func TestObjectTableRealFixtures(t *testing.T) {
	pel := objectTableEntries(t, goldenF4Pelmeni)
	if len(pel) != 2 {
		t.Fatalf("Пельмени: got %d replicas, want 2", len(pel))
	}
	if got := hex.EncodeToString(objEntryUUID(pel[0])); got != "68cc87f9207f62445defc2eaab6caffa" {
		t.Fatalf("Пельмени replica0 uuid = %s", got)
	}
	if c1, c2 := objEntryCounters(pel[0]); c1 != 1198 || c2 != 40 {
		t.Fatalf("Пельмени replica0 counters = [%d,%d], want [1198,40]", c1, c2)
	}
	if c1, c2 := objEntryCounters(pel[1]); c1 != 1198 || c2 != 39 {
		t.Fatalf("Пельмени replica1 counters = [%d,%d], want [1198,39]", c1, c2)
	}
	if ajo := objectTableEntries(t, goldenF4Ajo); len(ajo) != 5 {
		t.Fatalf("Ajo blanco: got %d replicas, want 5", len(ajo))
	}
}

// parseObjectTable on an encoded in-place-update body must return every foreign
// replica entry verbatim plus our advanced one, in order.
func TestParseObjectTableRoundTrip(t *testing.T) {
	foreign := objectTableEntries(t, goldenF4Pelmeni) // two real foreign replicas
	ourUUID, _ := hex.DecodeString("308055bfc10dffcefd9def5f1801d15c")
	const baseC1, baseC2 = 1000, 5
	rep := &replicaState{foreign: foreign, uuid: ourUUID, baseC1: baseC1, baseC2: baseC2}

	blob, err := encodeMergeableNoteBody("Торт", [][]string{{"Мука"}}, "Шаг", nil, rep)
	if err != nil {
		t.Fatal(err)
	}
	entries, ok := parseObjectTable(blob)
	if !ok {
		t.Fatal("parseObjectTable failed on an encoded update body")
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (2 foreign + ours)", len(entries))
	}
	// Foreign entries preserved byte-for-byte and in order.
	for i := range foreign {
		if !reflect.DeepEqual(entries[i], foreign[i]) {
			t.Fatalf("foreign entry %d not preserved verbatim", i)
		}
	}
	// Our entry carries our UUID and counters advanced strictly past the base, so the
	// vector dominates the device's last-seen state.
	if got := hex.EncodeToString(objEntryUUID(entries[2])); got != "308055bfc10dffcefd9def5f1801d15c" {
		t.Fatalf("our replica uuid = %s", got)
	}
	if c1, c2 := objEntryCounters(entries[2]); c1 <= baseC1 || c2 <= baseC2 {
		t.Fatalf("our counters = [%d,%d], want both strictly > base [%d,%d]", c1, c2, baseC1, baseC2)
	}
}

// A nil replicaState (create path) yields exactly one replica entry — guarding the
// no-behavior-change property for creates.
func TestParseObjectTableCreateSingleReplica(t *testing.T) {
	blob, err := encodeMergeableNoteBody("T", nil, "step", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, ok := parseObjectTable(blob)
	if !ok || len(entries) != 1 {
		t.Fatalf("create body: ok=%v entries=%d, want ok=true entries=1", ok, len(entries))
	}
}
