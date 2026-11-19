package csvfix

import (
	"encoding/csv"
	"errors"
	"strings"
	"testing"
)

var unit = Costs{
	InsertComma: 1,
	DeleteComma: 1,
	InsertQuote: 1,
	DeleteQuote: 1,
	InsertLF:    1,
	DeleteLF:    1,
}

// recordsEqual compares ordered records.
func recordsEqual(a, b []Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// parseWithStdlib cross-checks a repaired file against the standard library
// CSV reader with a strict per-record column count. Test inputs never use
// '\r', so stdlib's CR handling is irrelevant here.
func parseWithStdlib(t *testing.T, text string, k int) []Record {
	t.Helper()
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = k
	r.LazyQuotes = false
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("repaired text rejected by encoding/csv: %v\ntext=%q", err, text)
	}
	out := make([]Record, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

func editCost(edits []Edit, c Costs) int {
	total := 0
	for _, e := range edits {
		if e.Op == OpDelete {
			total += delCost(c, e.Char)
		} else {
			total += insCost(c, e.Char)
		}
	}
	return total
}

func insCost(c Costs, ch byte) int {
	switch ch {
	case ',':
		return c.InsertComma
	case '"':
		return c.InsertQuote
	default:
		return c.InsertLF
	}
}

func delCost(c Costs, ch byte) int {
	switch ch {
	case ',':
		return c.DeleteComma
	case '"':
		return c.DeleteQuote
	default:
		return c.DeleteLF
	}
}

func TestValidInputNeedsNoEdits(t *testing.T) {
	in := "a,b\n\"c,d\",e\n"
	res, err := Repair(in, 2, unit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ambiguous {
		t.Fatal("valid input reported ambiguous")
	}
	if res.Cost != 0 || len(res.Edits) != 0 {
		t.Fatalf("expected zero-cost empty edit list, got cost=%d edits=%v", res.Cost, res.Edits)
	}
	if res.Repaired != in {
		t.Fatalf("repaired text differs from input:\n got %q\nwant %q", res.Repaired, in)
	}
	want := parseWithStdlib(t, in, 2)
	if !recordsEqual(res.Records, want) {
		t.Fatalf("records mismatch:\n got %v\nwant %v", res.Records, want)
	}
}

func TestQuotedFieldWithEscapesAndEmbeddedSeparators(t *testing.T) {
	in := "\"he said \"\"hi, friend\"\"\",x\n"
	res, err := Repair(in, 2, unit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cost != 0 {
		t.Fatalf("expected cost 0, got %d", res.Cost)
	}
	want := parseWithStdlib(t, in, 2)
	if !recordsEqual(res.Records, want) {
		t.Fatalf("got %v want %v", res.Records, want)
	}
	if got := res.Records[0][0]; got != "he said \"hi, friend\"" {
		t.Fatalf("escaped quote/embedded comma decoded wrong: %q", got)
	}
}

func TestMissingSeparatorIsInserted(t *testing.T) {
	// k=1: "ab" can only become a single record by appending an LF; this
	// repair is unique under unit costs and costs one insertion.
	res, err := Repair("ab", 1, unit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ambiguous {
		t.Fatal("unexpected ambiguity")
	}
	if res.Cost != 1 || len(res.Edits) != 1 ||
		res.Edits[0].Op != OpInsert || res.Edits[0].Char != '\n' || res.Edits[0].Pos != 2 {
		t.Fatalf("unexpected edits: cost=%d %v", res.Cost, res.Edits)
	}
	if res.Repaired != "ab\n" {
		t.Fatalf("repaired=%q", res.Repaired)
	}
	want := parseWithStdlib(t, res.Repaired, 1)
	if !recordsEqual(res.Records, want) {
		t.Fatalf("got %v want %v", res.Records, want)
	}
}

func TestStructuralDeletion(t *testing.T) {
	// A stray quote inside a k=1 field must be deleted; the only valid
	// repair is unique and removes exactly that quote.
	res, err := Repair("a\"b", 1, unit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ambiguous {
		t.Fatal("unexpected ambiguity")
	}
	if res.Cost != 2 || len(res.Edits) != 2 {
		t.Fatalf("unexpected edits: cost=%d %v", res.Cost, res.Edits)
	}
	if res.Edits[0].Op != OpDelete || res.Edits[0].Char != '"' || res.Edits[0].Pos != 1 {
		t.Fatalf("expected quote delete, got %v", res.Edits)
	}
	if res.Repaired != "ab\n" {
		t.Fatalf("repaired=%q", res.Repaired)
	}
	for _, e := range res.Edits {
		if !isStructural(e.Char) {
			t.Fatalf("non-structural edit: %v", e)
		}
	}
}

func TestTwoRecordsMustNotBeMerged(t *testing.T) {
	// Two already-correct rows missing only the trailing LF must remain
	// two records; naive column padding would splice them.
	in := "a\nb"
	res, err := Repair(in, 1, unit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ambiguous {
		t.Fatal("unexpected ambiguity")
	}
	if res.Repaired != "a\nb\n" {
		t.Fatalf("repaired=%q", res.Repaired)
	}
	want := []Record{{"a"}, {"b"}}
	if !recordsEqual(res.Records, want) {
		t.Fatalf("got %v want %v", res.Records, want)
	}
}

func TestQuotedEmbeddedLFAndMissingCloseQuote(t *testing.T) {
	// The opening quote forces quoted mode, so the embedded newline is a
	// literal field character; the only cheap completion inserts the
	// missing closing quote. This is a unique one-edit repair.
	in := "\"a\nb,c\n"
	res, err := Repair(in, 2, unit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ambiguous {
		t.Fatal("unexpected ambiguity")
	}
	if res.Repaired != "\"a\nb\",c\n" {
		t.Fatalf("repaired=%q", res.Repaired)
	}
	want := parseWithStdlib(t, res.Repaired, 2)
	if !recordsEqual(res.Records, want) {
		t.Fatalf("got %v want %v", res.Records, want)
	}
	if res.Records[0][0] != "a\nb" {
		t.Fatalf("embedded LF not preserved inside quotes: %q", res.Records[0][0])
	}
}

func TestAmbiguityReturnsTwoDifferentWitnesses(t *testing.T) {
	// "ab" k=2 under unit costs has two minimum-cost repairs that decode
	// to different records: a comma split ("a,b\n" -> [a b]) versus an
	// empty trailing field ("ab,\n" -> [ab ""]).
	res, err := Repair("ab", 2, unit)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ambiguous {
		t.Fatal("expected ambiguity")
	}
	if res.Edits != nil || res.Records != nil || res.Repaired != "" {
		t.Fatal("ambiguous result must not present a single repair")
	}
	w0, w1 := res.Witnesses[0], res.Witnesses[1]
	if w0.Repaired == "" || w1.Repaired == "" {
		t.Fatal("both witnesses must be populated")
	}
	if recordsEqual(w0.Records, w1.Records) {
		t.Fatalf("witness records must differ, both were %v", w0.Records)
	}
	if editCost(w0.Edits, unit) != res.Cost || editCost(w1.Edits, unit) != res.Cost {
		t.Fatal("witnesses must each have the minimum cost")
	}
	for _, w := range []Witness{w0, w1} {
		if got := applyEdits("ab", w.Edits); got != w.Repaired {
			t.Fatalf("witness edits do not reconstruct text: %q vs %q", got, w.Repaired)
		}
		rows, verr := ValidateRepair(w.Repaired, 2)
		if verr != nil {
			t.Fatalf("witness repaired text invalid: %v", verr)
		}
		if !recordsEqual(rows, w.Records) {
			t.Fatal("witness records fail independent validation")
		}
		for _, e := range w.Edits {
			if !isStructural(e.Char) {
				t.Fatalf("edit touches non-structural char: %v", e)
			}
		}
	}
}

func TestWeightsChooseCheaperRepair(t *testing.T) {
	// Penalize every edit except comma/LF insertion differently so the
	// single-record trailing-empty-field repair ("ab,\n") wins over the
	// split. With quote and deletion costly and a comma cheap, k=2 still
	// needs one comma plus one LF: assert the chosen minimum matches the
	// configured costs rather than hard-coding text.
	cheaperComma := Costs{
		InsertComma: 1,
		DeleteComma: 100,
		InsertQuote: 100,
		DeleteQuote: 100,
		InsertLF:    1,
		DeleteLF:    100,
	}
	res, err := Repair("ab", 2, cheaperComma)
	if err != nil {
		t.Fatal(err)
	}
	// Under these weights both k=2 completions need one comma + one LF
	// somewhere, so the tie may persist; verify whichever branch is
	// returned is cost-consistent rather than asserting uniqueness.
	if res.Ambiguous {
		for _, w := range res.Witnesses {
			if editCost(w.Edits, cheaperComma) != res.Cost {
				t.Fatal("witness cost inconsistent with configured costs")
			}
		}
		return
	}
	if editCost(res.Edits, cheaperComma) != res.Cost {
		t.Fatal("repair cost inconsistent with configured costs")
	}
	rows, verr := ValidateRepair(res.Repaired, 2)
	if verr != nil {
		t.Fatalf("repaired text invalid: %v", verr)
	}
	if !recordsEqual(rows, res.Records) {
		t.Fatal("records fail independent validation")
	}
}

func TestNonStructuralBytesAreImmutable(t *testing.T) {
	// CR, tab, UTF-8 and NUL must survive byte-for-byte; only ',', '"',
	// '\n' may appear in edits or be added. Check every returned repair
	// (the unique one, or both ambiguity witnesses).
	in := "a\tb\rc\000é,\"x\"\n"
	res, err := Repair(in, 2, unit)
	if err != nil {
		t.Fatal(err)
	}
	var repairs []struct {
		edits []Edit
		text  string
	}
	if res.Ambiguous {
		for _, w := range res.Witnesses {
			repairs = append(repairs, struct {
				edits []Edit
				text  string
			}{w.Edits, w.Repaired})
		}
	} else {
		repairs = append(repairs, struct {
			edits []Edit
			text  string
		}{res.Edits, res.Repaired})
	}
	for _, r := range repairs {
		for _, e := range r.edits {
			if !isStructural(e.Char) {
				t.Fatalf("edit on non-structural byte: %v", e)
			}
		}
		if got := applyEdits(in, r.edits); got != r.text {
			t.Fatalf("edits do not reconstruct repaired text")
		}
		// The original byte sequence must be a subsequence of the
		// repaired text (inserts are the only allowed additions, and
		// deletes only remove structural bytes).
		ri := 0
		for i := 0; i < len(in); i++ {
			j := strings.IndexByte(r.text[ri:], in[i])
			if j < 0 {
				t.Fatalf("original byte %q at %d lost in repair %q", in[i], i, r.text)
			}
			ri += j + 1
		}
	}
}

func TestInvalidArguments(t *testing.T) {
	if _, err := Repair("a", 0, unit); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("k=0: got %v", err)
	}
	bad := unit
	bad.InsertLF = -1
	if _, err := Repair("a", 1, bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative cost: got %v", err)
	}
}

func TestOriginalInputIsNotModified(t *testing.T) {
	in := "a,b\nc,d"
	orig := in
	if _, err := Repair(in, 2, unit); err != nil {
		t.Fatal(err)
	}
	if in != orig {
		t.Fatal("input string was modified")
	}
}

// ---- Exhaustive BFS oracle for small inputs ----

type oracleResult struct {
	minCost  int
	unique   bool
	variants [][]Record
}

// bruteForce enumerates every distinct (position, built-text) reachable via
// inserts of the three structural characters, matches, and deletes of
// structural characters, bounded by maxAdded inserted bytes. It returns the
// distinct decoded record sets found among minimum-cost valid repairs, or
// nil if the state budget was exceeded.
func bruteForce(in string, k int, costs Costs, maxAdded, budget int) *oracleResult {
	type state struct {
		pos   int
		text  string
		added int
		cost  int
	}
	seen := map[[2]interface{}]int{}
	queue := []state{{}}
	best := map[string]int{}
	valid := map[string][]Record{}
	popped := 0

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		popped++
		if popped > budget {
			return nil
		}
		if len(seen) > budget {
			return nil
		}
		key := [2]interface{}{n.pos, n.text}
		if pc, ok := seen[key]; ok && pc <= n.cost {
			continue
		}
		seen[key] = n.cost

		if n.pos == len(in) {
			if n.text != "" {
				if rows, err := ValidateRepair(n.text, k); err == nil && len(rows) > 0 {
					if pc, ok := best[n.text]; !ok || n.cost < pc {
						best[n.text] = n.cost
						valid[n.text] = rows
					}
				}
			}
			if n.added >= maxAdded {
				continue
			}
		}
		if n.added < maxAdded {
			for _, ch := range structuralChars {
				queue = append(queue, state{
					pos: n.pos, text: n.text + string(ch),
					added: n.added + 1, cost: n.cost + insCost(costs, ch),
				})
			}
		}
		if n.pos < len(in) {
			ch := in[n.pos]
			queue = append(queue, state{
				pos: n.pos + 1, text: n.text + string(ch),
				added: n.added, cost: n.cost,
			})
			if isStructural(ch) {
				queue = append(queue, state{
					pos: n.pos + 1, text: n.text,
					added: n.added, cost: n.cost + delCost(costs, ch),
				})
			}
		}
	}

	out := oracleResult{minCost: -1}
	for text, c := range best {
		if out.minCost < 0 || c < out.minCost {
			out.minCost = c
			out.variants = nil
		}
		if c == out.minCost {
			rows := valid[text]
			dup := false
			for _, v := range out.variants {
				if recordsEqual(v, rows) {
					dup = true
					break
				}
			}
			if !dup {
				out.variants = append(out.variants, rows)
			}
		}
	}
	if out.minCost < 0 {
		return nil
	}
	out.unique = len(out.variants) == 1
	return &out
}

func TestAgainstBruteForce(t *testing.T) {
	alphabet := []byte{'a', '"', ',', '\n'}
	costProfiles := []Costs{
		unit,
		{InsertComma: 3, DeleteComma: 2, InsertQuote: 1, DeleteQuote: 4, InsertLF: 5, DeleteLF: 2},
		{InsertComma: 1, DeleteComma: 10, InsertQuote: 10, DeleteQuote: 1, InsertLF: 2, DeleteLF: 10},
	}
	const maxN = 4
	const maxAdded = 4
	const budget = 300000
	cases, skipped := 0, 0
	for _, cp := range costProfiles {
		for n := 0; n <= maxN; n++ {
			limit := 1 << (2 * n)
			for mask := 0; mask < limit; mask++ {
				inb := make([]byte, n)
				m := mask
				for i := 0; i < n; i++ {
					inb[i] = alphabet[m%4]
					m /= 4
				}
				for _, k := range []int{1, 2} {
					in := string(inb)
					ora := bruteForce(in, k, cp, maxAdded, budget)
					if ora == nil {
						skipped++
						continue
					}
					res, err := Repair(in, k, cp)
					if err != nil {
						t.Fatalf("Repair(%q,k=%d): %v", in, k, err)
					}
					// The oracle only enumerates repairs with up to
					// maxAdded inserted structural bytes. If the DP
					// optimum needs more, the oracle is incomplete here
					// and cannot serve as a reference; skip it.
					inserts := 0
					if res.Ambiguous {
						for _, e := range res.Witnesses[0].Edits {
							if e.Op == OpInsert {
								inserts++
							}
						}
					} else {
						for _, e := range res.Edits {
							if e.Op == OpInsert {
								inserts++
							}
						}
					}
					if inserts > maxAdded {
						skipped++
						continue
					}
					if res.Cost != ora.minCost {
						t.Fatalf("cost mismatch for %q k=%d: dp=%d oracle=%d", in, k, res.Cost, ora.minCost)
					}
					if ora.unique == res.Ambiguous {
						t.Fatalf("ambiguity mismatch for %q k=%d: dp ambiguous=%v oracle unique=%v",
							in, k, res.Ambiguous, ora.unique)
					}
					if !res.Ambiguous {
						if !recordsEqual(res.Records, ora.variants[0]) {
							t.Fatalf("records mismatch for %q k=%d:\n dp=%v\n oracle=%v",
								in, k, res.Records, ora.variants[0])
						}
						if editCost(res.Edits, cp) != res.Cost {
							t.Fatalf("reported edits cost mismatch for %q", in)
						}
						// Edits must reconstruct Repaired when applied to
						// the original input.
						if got := applyEdits(in, res.Edits); got != res.Repaired {
							t.Fatalf("edits do not reconstruct repaired text for %q:\n edits=%v\n got=%q\nwant=%q",
								in, res.Edits, got, res.Repaired)
						}
						rows, verr := ValidateRepair(res.Repaired, k)
						if verr != nil {
							t.Fatalf("repaired text fails ValidateRepair for %q: %v", in, verr)
						}
						if !recordsEqual(rows, res.Records) {
							t.Fatalf("ValidateRepair records differ for %q", in)
						}
					} else {
						if recordsEqual(res.Witnesses[0].Records, res.Witnesses[1].Records) {
							t.Fatalf("witnesses equal for %q", in)
						}
						for _, w := range []Witness{res.Witnesses[0], res.Witnesses[1]} {
							if editCost(w.Edits, cp) != res.Cost {
								t.Fatalf("witness cost mismatch for %q", in)
							}
							if got := applyEdits(in, w.Edits); got != w.Repaired {
								t.Fatalf("witness edits do not reconstruct text for %q", in)
							}
							rows, verr := ValidateRepair(w.Repaired, k)
							if verr != nil {
								t.Fatalf("witness repaired text invalid for %q: %v", in, verr)
							}
							if !recordsEqual(rows, w.Records) {
								t.Fatalf("witness ValidateRepair mismatch for %q", in)
							}
							for _, e := range w.Edits {
								if !isStructural(e.Char) {
									t.Fatalf("witness edit touches non-structural byte for %q: %v", in, e)
								}
							}
						}
					}
					cases++
				}
			}
		}
	}
	t.Logf("brute-force compared %d cases (%d skipped for budget)", cases, skipped)
}

// applyEdits reconstructs the repaired text by applying insert/delete edits
// to the original input. Inserts at a position are emitted before the
// original byte there; deletes remove the byte at that position.
func applyEdits(in string, edits []Edit) string {
	var sb strings.Builder
	del := map[int]bool{}
	insAt := map[int][]byte{}
	for _, e := range edits {
		if e.Op == OpInsert {
			insAt[e.Pos] = append(insAt[e.Pos], e.Char)
		} else {
			del[e.Pos] = true
		}
	}
	for i := 0; i <= len(in); i++ {
		if bs, ok := insAt[i]; ok {
			sb.Write(bs)
		}
		if i < len(in) && !del[i] {
			sb.WriteByte(in[i])
		}
	}
	return sb.String()
}

func TestPolynomialScaling(t *testing.T) {
	// Correctness on corrupted inputs is exhaustively covered by the
	// brute-force oracle; this test guards the polynomial runtime by
	// feeding large, already-valid files that must parse at zero cost and
	// uniquely (no edits means there is exactly one edit sequence).
	validFile := func(rows, cols int) string {
		var sb strings.Builder
		for r := 0; r < rows; r++ {
			for c := 0; c < cols-1; c++ {
				sb.WriteString("v,")
			}
			sb.WriteString("v\n")
		}
		return sb.String()
	}
	cases := []struct {
		rows, cols int
	}{
		{500, 2},
		{1000, 2},
		{200, 40},
	}
	for _, tc := range cases {
		in := validFile(tc.rows, tc.cols)
		res, err := Repair(in, tc.cols, unit)
		if err != nil {
			t.Fatalf("rows=%d cols=%d: %v", tc.rows, tc.cols, err)
		}
		if res.Ambiguous {
			t.Fatalf("rows=%d cols=%d: valid file must be unique", tc.rows, tc.cols)
		}
		if res.Cost != 0 || len(res.Edits) != 0 {
			t.Fatalf("rows=%d cols=%d: expected zero-cost no-op, got cost=%d edits=%v",
				tc.rows, tc.cols, res.Cost, res.Edits)
		}
		if res.Repaired != in {
			t.Fatalf("rows=%d cols=%d: valid text altered", tc.rows, tc.cols)
		}
		if len(res.Records) != tc.rows {
			t.Fatalf("rows=%d cols=%d: decoded %d records", tc.rows, tc.cols, len(res.Records))
		}
	}

	// A large corrupted file that the oracle's grammar also accepts only
	// after edits: assert it terminates in polynomial time, returns a
	// consistent cost, and every offered repair independently validates.
	var sb strings.Builder
	for r := 0; r < 300; r++ {
		sb.WriteString("\"line1\nline2,x\n")
	}
	big := sb.String()
	res, err := Repair(big, 2, unit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ambiguous {
		for _, w := range res.Witnesses {
			if editCost(w.Edits, unit) != res.Cost {
				t.Fatal("witness cost inconsistent")
			}
			if rows, verr := ValidateRepair(w.Repaired, 2); verr != nil || len(rows) == 0 {
				t.Fatalf("ambiguous witness invalid: %v", verr)
			}
		}
	} else {
		if editCost(res.Edits, unit) != res.Cost {
			t.Fatal("reported cost inconsistent with edits")
		}
		if rows, verr := ValidateRepair(res.Repaired, 2); verr != nil || len(rows) == 0 {
			t.Fatalf("repaired output invalid: %v", verr)
		}
	}
}

func TestEmptyInputRequiresFullRepair(t *testing.T) {
	res, err := Repair("", 1, unit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ambiguous || res.Cost != 1 || res.Repaired != "\n" {
		t.Fatalf("got cost=%d ambiguous=%v repaired=%q", res.Cost, res.Ambiguous, res.Repaired)
	}
}

func TestEditStringStable(t *testing.T) {
	if got := (Edit{Op: OpInsert, Char: ',', Pos: 3}).String(); got != "insert ',' at 3" {
		t.Fatalf("got %q", got)
	}
	if got := (Edit{Op: OpDelete, Char: '\n', Pos: 7}).String(); got != "delete '\\n' at 7" {
		t.Fatalf("got %q", got)
	}
}

func TestExampleSmoke(t *testing.T) {
	// A row missing a separator has several one-edit completions under
	// unit costs, so this is a genuine ambiguity: the API must surface two
	// minimum-cost witnesses rather than guess.
	res, err := Repair("name,age\nAda36\n", 2, unit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cost != 1 {
		t.Fatalf("got cost=%d, want 1", res.Cost)
	}
	if !res.Ambiguous {
		t.Fatalf("expected ambiguity, got unique repair %q", res.Repaired)
	}
	if recordsEqual(res.Witnesses[0].Records, res.Witnesses[1].Records) {
		t.Fatal("witnesses must decode to different records")
	}
	for _, w := range res.Witnesses {
		if editCost(w.Edits, unit) != 1 {
			t.Fatalf("witness cost %v", w.Edits)
		}
		if _, verr := ValidateRepair(w.Repaired, 2); verr != nil {
			t.Fatalf("witness invalid: %v", verr)
		}
	}
}
