// Package csvfix repairs slightly corrupted CSV exports (for example files
// downloaded with Colly whose commas, double quotes or line feeds were lost
// or duplicated) by searching for a globally minimum-cost repair.
//
// The accepted CSV dialect is fixed:
//   - fields are separated by commas (',');
//   - double quotes ('"') delimit quoted fields, and a literal quote inside a
//     quoted field is escaped by doubling ("");
//   - records are terminated by LF ('\n').
//
// CR ('\r') and every other byte are ordinary field content. Inside a quoted
// field, commas and line feeds are literal content as well.
//
// Repairs may only INSERT or DELETE the three structural characters ',', '"'
// and '\n', each at a caller configured cost. No other byte can ever be
// inserted, deleted or rewritten: the dynamic program below contains no edge
// that could do so. The search cost grows polynomially with the input length
// and the known column count; candidate repaired files are never enumerated
// and the search is never truncated.
package csvfix

import (
	"container/heap"
	"errors"
	"fmt"
)

// Costs holds the non-negative per-character edit costs.
type Costs struct {
	InsertComma int
	DeleteComma int
	InsertQuote int
	DeleteQuote int
	InsertLF    int
	DeleteLF    int
}

// EditOp is the kind of an edit operation.
type EditOp uint8

const (
	// OpInsert inserts one structural character.
	OpInsert EditOp = iota
	// OpDelete deletes one structural character from the original input.
	OpDelete
)

// Edit is one reviewable repair step.
type Edit struct {
	Op   EditOp
	Char byte // ',' or '"' or '\n'
	// Pos is the byte offset in the original input at which the edit
	// applies. An insert sits before the original byte at Pos (Pos ==
	// len(input) appends after the last byte). A delete removes the
	// original byte at Pos.
	Pos int
}

// String renders an edit in a stable, greppable form.
func (e Edit) String() string {
	verb := "insert"
	if e.Op == OpDelete {
		verb = "delete"
	}
	return fmt.Sprintf("%s %q at %d", verb, e.Char, e.Pos)
}

// Record is one decoded CSV record: one string per column.
type Record = []string

// Witness is one minimum-cost repair used to demonstrate that two equally
// cheap repairs decode to different ordered records.
type Witness struct {
	Edits    []Edit
	Repaired string
	Records  []Record
}

// Result is the outcome of a repair search.
type Result struct {
	// Cost is the global minimum edit cost (zero when the input already
	// parses correctly).
	Cost int
	// Edits, Repaired and Records describe a minimum-cost repair and its
	// decoded records. They are populated only when Ambiguous is false.
	Edits []Edit
	// Repaired is the repaired CSV text. The original input string is
	// never modified; Repaired is a separate allocation.
	Repaired string
	// Records are the ordered records shared by every minimum-cost
	// repair.
	Records []Record

	// Ambiguous reports that at least two minimum-cost repairs decode to
	// different ordered records. When true, Edits/Repaired/Records are
	// left empty and Witnesses is populated.
	Ambiguous bool
	// Witnesses holds two minimum-cost repairs whose decoded records
	// differ. It is only populated when Ambiguous is true.
	Witnesses [2]Witness
}

// Sentinel errors.
var (
	// ErrNoRepair means no sequence of permitted edits yields a valid
	// CSV file with the requested column count.
	ErrNoRepair = errors.New("csvfix: no valid repair exists for the given column count")
	// ErrInvalidArgument means columns was non-positive or a cost was
	// negative.
	ErrInvalidArgument = errors.New("csvfix: columns must be positive and all costs must be non-negative")
)

// DFA modes for one column position.
const (
	modeFieldStart = iota // at the first position of a field, nothing read yet
	modeUnquoted          // inside an unquoted field
	modeQuoted            // inside a quoted field
	modeAfterQuote        // closing quote read, only comma/LF/escaped quote legal
)

// Predecessor edge kinds stored for backtracking.
const (
	predSeed   = iota // the DP seed cell (no incoming edge)
	predInsert        // structural char inserted, predecessor in the same layer
	predMatch         // input byte consumed as itself, predecessor in previous layer
	predDelete        // structural input byte deleted, predecessor in previous layer
)

// decodeNode is a hash-consed, persistent representation of a partially
// decoded CSV prefix: the completed records so far, the fields of the record
// currently being built, and its in-progress field. Two repair paths decode
// to the same ordered records exactly when they reach the same *decodeNode
// pointer, regardless of quoting differences ("a" vs a). This is the object
// on which ambiguity is decided — not on the raw edited text.
type decodeNode struct {
	parent *decodeNode
	// op discriminates how this node extends its parent.
	op byte
	// b is the appended content byte; meaningful only when op == opContent.
	b byte
	// opContent: append this byte to the current field.
	// opField:   close the current field (comma).
	// opRecord:  close the current field and record (LF).
}

const (
	opContent = iota
	opField
	opRecord
)

type nodeKey struct {
	parent *decodeNode
	op     byte
	b      byte
}

// pred is one backpointer.
type pred struct {
	from int32 // predecessor DFA state
	ch   byte  // character involved (inserted, matched, or deleted)
	kind byte  // predInsert / predMatch / predDelete
}

// cand is one minimum-cost way to reach a cell: a canonical decoded-prefix
// node and the predecessor edge that reaches it.
type cand struct {
	node *decodeNode
	p    pred
}

// cell is one DP cell: the best cost at one layer/state and the minimum-cost
// candidates reaching it. Candidates are deduplicated by their canonical
// decoded node: a single node means every minimum-cost path decodes to the
// same records, while two distinct nodes mean genuine ambiguity and each
// node carries a backtracking edge that produces it.
type cell struct {
	cost  int
	cands []cand
}

const infinity = int(^uint(0) >> 1)

func (c *cell) reset() {
	c.cost = infinity
	c.cands = nil
}

func (c *cell) amb() bool { return len(c.cands) >= 2 }

// relax folds an incoming edge (nc, node, p) into the cell. It reports
// whether the cell changed in a way that requires (re)processing in the
// insert closure.
func (c *cell) relax(nc int, node *decodeNode, p pred) bool {
	if nc > c.cost {
		return false
	}
	if nc < c.cost {
		c.cost = nc
		c.cands = []cand{{node: node, p: p}}
		return true
	}
	for i := range c.cands {
		if c.cands[i].node == node {
			return false // same decoded prefix already represented
		}
	}
	if len(c.cands) < 2 {
		c.cands = append(c.cands, cand{node: node, p: p})
		return true // a distinct decoded prefix now ties here
	}
	return false
}

// dfa is the fixed CSV grammar. State ids are col*4+mode, plus one dedicated
// start state. The accept state is col 0 / modeFieldStart; it is only
// reachable after at least one completed record, while the start state is
// where parsing begins (behaving like first-column field-start).
type dfa struct {
	k int
}

func (d *dfa) size() int  { return 4*d.k + 1 }
func (d *dfa) start() int { return 4 * d.k }
func (d *dfa) id(col, mode int) int {
	return col*4 + mode
}

// step describes one grammar transition. emit tells whether the transition
// appends content, closes a field, or closes a record.
type step struct {
	dst     int
	emit    bool
	op      byte
	content byte
}

// move applies one character to a state. The same transitions serve matched
// characters (cross-layer, cost 0) and inserted structural characters
// (within-layer, insert cost). Quote open/close and escaped-quote handling
// emit no structural token; only an escaped doubled quote emits content.
func (d *dfa) move(state int, ch byte) (step, bool) {
	col, mode := 0, modeFieldStart
	if state != d.start() {
		col = state / 4
		mode = state % 4
	}
	switch ch {
	case '"':
		switch mode {
		case modeFieldStart:
			return step{dst: d.id(col, modeQuoted)}, true
		case modeQuoted:
			return step{dst: d.id(col, modeAfterQuote)}, true
		case modeAfterQuote: // "" -> literal quote inside quoted field
			return step{dst: d.id(col, modeQuoted), emit: true, op: opContent, content: '"'}, true
		}
		return step{}, false
	case ',':
		switch mode {
		case modeFieldStart, modeUnquoted, modeAfterQuote:
			if col == d.k-1 {
				return step{}, false
			}
			return step{dst: d.id(col+1, modeFieldStart), emit: true, op: opField}, true
		case modeQuoted: // literal comma inside quoted field
			return step{dst: d.id(col, modeQuoted), emit: true, op: opContent, content: ','}, true
		}
		return step{}, false
	case '\n':
		switch mode {
		case modeFieldStart, modeUnquoted, modeAfterQuote:
			if col != d.k-1 {
				return step{}, false
			}
			return step{dst: d.id(0, modeFieldStart), emit: true, op: opRecord}, true
		case modeQuoted: // literal line feed inside quoted field
			return step{dst: d.id(col, modeQuoted), emit: true, op: opContent, content: '\n'}, true
		}
		return step{}, false
	default:
		// Non-structural byte: immutable content. There is never an
		// insert/delete edge for it.
		switch mode {
		case modeFieldStart:
			return step{dst: d.id(col, modeUnquoted), emit: true, op: opContent, content: ch}, true
		case modeUnquoted, modeQuoted:
			return step{dst: d.id(col, mode), emit: true, op: opContent, content: ch}, true
		}
		return step{}, false
	}
}

func isStructural(ch byte) bool {
	return ch == ',' || ch == '"' || ch == '\n'
}

var structuralChars = [3]byte{',', '"', '\n'}

type solver struct {
	data    []byte
	k       int
	costs   Costs
	grammar dfa
	intern  map[nodeKey]*decodeNode
	root    *decodeNode
	layers  [][]cell // layers[i] is the DP layer after consuming data[:i]
}

func (s *solver) insCost(ch byte) int {
	switch ch {
	case ',':
		return s.costs.InsertComma
	case '"':
		return s.costs.InsertQuote
	default:
		return s.costs.InsertLF
	}
}

func (s *solver) delCost(ch byte) int {
	switch ch {
	case ',':
		return s.costs.DeleteComma
	case '"':
		return s.costs.DeleteQuote
	default:
		return s.costs.DeleteLF
	}
}

// extend applies a grammar step to a canonical decoded prefix, hash-consing
// the resulting node. Non-emitting transitions (quote toggles) return the
// same node, which is what makes "a" and a canonicalize identically.
func (s *solver) extend(tail *decodeNode, st step) *decodeNode {
	if !st.emit {
		return tail
	}
	key := nodeKey{parent: tail, op: st.op, b: st.content}
	if n, ok := s.intern[key]; ok {
		return n
	}
	n := &decodeNode{parent: tail, op: st.op, b: st.content}
	s.intern[key] = n
	return n
}

// heapItem / priority queue boilerplate.
type heapItem struct {
	state int
	cost  int
}

type priorityQueue []heapItem

func (q priorityQueue) Len() int            { return len(q) }
func (q priorityQueue) Less(i, j int) bool  { return q[i].cost < q[j].cost }
func (q priorityQueue) Swap(i, j int)       { q[i], q[j] = q[j], q[i] }
func (q *priorityQueue) Push(x interface{}) { *q = append(*q, x.(heapItem)) }
func (q *priorityQueue) Pop() interface{} {
	old := *q
	n := len(old)
	it := old[n-1]
	*q = old[:n-1]
	return it
}

// closure relaxes all within-layer insert edges with Dijkstra over
// non-negative insert costs, starting from cells already populated by
// cross-layer (match/delete) edges. Each cell is reprocessed when its cost
// drops or a distinct decoded prefix ties at the same cost; every retained
// candidate propagates its own canonical node so ambiguity flows onward.
func (s *solver) closure(layer []cell) {
	q := &priorityQueue{}
	gen := make([]int, len(layer))
	for st := range layer {
		if layer[st].cost < infinity {
			*q = append(*q, heapItem{st, layer[st].cost})
		}
	}
	heap.Init(q)
	for q.Len() > 0 {
		top := heap.Pop(q).(heapItem)
		u := top.state
		if top.cost != layer[u].cost {
			continue // stale entry
		}
		curGen := layer[u].cost*16 + len(layer[u].cands)
		if gen[u] == curGen {
			continue
		}
		gen[u] = curGen
		for _, cand := range layer[u].cands {
			for _, ch := range structuralChars {
				st, ok := s.grammar.move(u, ch)
				if !ok {
					continue
				}
				nc := layer[u].cost + s.insCost(ch)
				node := s.extend(cand.node, st)
				p := pred{from: int32(u), ch: ch, kind: predInsert}
				if layer[st.dst].relax(nc, node, p) {
					heap.Push(q, heapItem{st.dst, layer[st.dst].cost})
				}
			}
		}
	}
}

func (s *solver) solve() (*cell, error) {
	size := s.grammar.size()

	layer := make([]cell, size)
	for i := range layer {
		layer[i].reset()
	}
	layer[s.grammar.start()].relax(0, s.root, pred{kind: predSeed})
	s.closure(layer) // inserts before the first input byte
	s.layers = append(s.layers, layer)

	for i := range s.data {
		ch := s.data[i]
		prev := s.layers[i]
		cur := make([]cell, size)
		for j := range cur {
			cur[j].reset()
		}
		// Cross-layer edges, expanded per candidate. Match consumes the
		// original byte unchanged (the only edge for non-structural
		// bytes); delete exists for structural bytes alone.
		for p := range prev {
			if prev[p].cost == infinity {
				continue
			}
			for _, cand := range prev[p].cands {
				if st, ok := s.grammar.move(p, ch); ok {
					node := s.extend(cand.node, st)
					cur[st.dst].relax(prev[p].cost, node,
						pred{from: int32(p), ch: ch, kind: predMatch})
				}
				if isStructural(ch) {
					cur[p].relax(prev[p].cost+s.delCost(ch), cand.node,
						pred{from: int32(p), ch: ch, kind: predDelete})
				}
			}
		}
		s.closure(cur)
		s.layers = append(s.layers, cur)
	}

	accept := s.grammar.id(0, modeFieldStart) // == 0
	last := s.layers[len(s.data)]
	if last[accept].cost == infinity {
		return nil, ErrNoRepair
	}
	return &last[accept], nil
}

type edge struct {
	layer int
	p     pred
}

// backtrackToNode reconstructs the edge path leading to the accept cell's
// candidate whose canonical decoded node is target. At every cell it picks
// the predecessor candidate that, after the candidate's grammar step, yields
// the node being traced. This node-directed trace is deterministic and, for
// two distinct accept nodes, necessarily yields two different decoded
// record sets (unlike indexing into predecessor lists, which can converge).
func (s *solver) backtrackToNode(accept int, target *decodeNode) []edge {
	var edges []edge
	layerIdx := len(s.data)
	state := accept
	want := target
	for {
		c := &s.layers[layerIdx][state]
		var chosen cand
		matched := false
		for _, cd := range c.cands {
			if cd.node == want {
				chosen = cd
				matched = true
				break
			}
		}
		if !matched {
			// Should be unreachable: every retained node traces to root.
			return edges
		}
		p := chosen.p
		if p.kind == predSeed {
			break
		}
		edges = append([]edge{{layer: layerIdx, p: p}}, edges...)
		prevState := int(p.from)
		prevLayer := layerIdx
		if p.kind != predInsert {
			prevLayer = layerIdx - 1
		}
		prevCell := &s.layers[prevLayer][prevState]
		var prevWant *decodeNode
		switch p.kind {
		case predDelete:
			// Deletion adds no token: the node is unchanged.
			prevWant = want
		default:
			st, _ := s.grammar.move(prevState, p.ch)
			for _, cd := range prevCell.cands {
				if s.extend(cd.node, st) == want {
					prevWant = cd.node
					break
				}
			}
		}
		if prevWant == nil && len(prevCell.cands) > 0 {
			prevWant = prevCell.cands[0].node
		}
		state = prevState
		want = prevWant
		if p.kind != predInsert {
			layerIdx--
		}
	}
	return edges
}

// materialize turns a backtracked edge path into edits, repaired text and the
// canonical decoded node reached.
func (s *solver) materialize(edges []edge) (edits []Edit, repaired []byte, node *decodeNode) {
	node = s.root
	for _, e := range edges {
		p := e.p
		switch p.kind {
		case predInsert:
			edits = append(edits, Edit{Op: OpInsert, Char: p.ch, Pos: e.layer})
			repaired = append(repaired, p.ch)
			st, _ := s.grammar.move(int(p.from), p.ch)
			node = s.extend(node, st)
		case predMatch:
			repaired = append(repaired, s.data[e.layer-1])
			st, _ := s.grammar.move(int(p.from), p.ch)
			node = s.extend(node, st)
		case predDelete:
			edits = append(edits, Edit{Op: OpDelete, Char: p.ch, Pos: e.layer - 1})
		}
	}
	return edits, repaired, node
}

// decodeNodeToRecords walks a canonical prefix node back to the root and
// reconstructs the ordered records, each with exactly k fields.
func decodeNodeToRecords(node *decodeNode, k int) []Record {
	type frame struct {
		op byte
		b  byte
	}
	var frames []frame
	for n := node; n != nil && n.parent != nil; n = n.parent {
		frames = append(frames, frame{op: n.op, b: n.b})
	}
	for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
		frames[i], frames[j] = frames[j], frames[i]
	}

	var records []Record
	rec := make(Record, 0, k)
	var field []byte
	flushField := func() {
		rec = append(rec, string(field))
		field = field[:0]
	}
	for _, f := range frames {
		switch f.op {
		case opContent:
			field = append(field, f.b)
		case opField:
			flushField()
		case opRecord:
			flushField()
			records = append(records, rec)
			rec = make(Record, 0, k)
		}
	}
	return records
}

// ValidateRepair parses repaired CSV text under the package's fixed dialect
// and returns its ordered records, each with exactly columns fields. It is
// the independent accept check used to verify repaired output and as the
// oracle's reference decoder.
func ValidateRepair(repaired string, columns int) ([]Record, error) {
	if columns <= 0 {
		return nil, ErrInvalidArgument
	}
	g := dfa{k: columns}
	state := g.start()
	intern := map[nodeKey]*decodeNode{}
	root := &decodeNode{}
	node := root
	emitted := 0
	for i := 0; i < len(repaired); i++ {
		st, ok := g.move(state, repaired[i])
		if !ok {
			return nil, fmt.Errorf("csvfix: invalid byte %q at offset %d for columns=%d",
				repaired[i], i, columns)
		}
		state = st.dst
		if st.emit {
			emitted++
			key := nodeKey{parent: node, op: st.op, b: st.content}
			if nn, ok := intern[key]; ok {
				node = nn
			} else {
				nn := &decodeNode{parent: node, op: st.op, b: st.content}
				intern[key] = nn
				node = nn
			}
		}
	}
	if state != g.id(0, modeFieldStart) || emitted == 0 {
		return nil, errors.New("csvfix: repaired text does not end on a complete record boundary")
	}
	records := decodeNodeToRecords(node, columns)
	for _, rec := range records {
		if len(rec) != columns {
			return nil, fmt.Errorf("csvfix: record has %d fields, want %d", len(rec), columns)
		}
	}
	return records, nil
}

// Repair searches the globally minimum-cost repair of data.
//
// columns is the known, fixed number of columns per record. costs configures
// the insertion and deletion price of each of the three structural
// characters. The original data is never modified. The algorithm runs in
// O(n·k·log k) time and O(n·k) space for input length n and column count k;
// it never enumerates candidate files.
//
// If every minimum-cost repair decodes to the same ordered records, Result
// contains that repair with Cost and Edits. Otherwise Result.Ambiguous is
// true and two minimum-cost witnesses with different decoded records are
// returned.
func Repair(data string, columns int, costs Costs) (*Result, error) {
	if columns <= 0 {
		return nil, ErrInvalidArgument
	}
	if costs.InsertComma < 0 || costs.DeleteComma < 0 ||
		costs.InsertQuote < 0 || costs.DeleteQuote < 0 ||
		costs.InsertLF < 0 || costs.DeleteLF < 0 {
		return nil, ErrInvalidArgument
	}

	s := &solver{
		data:    []byte(data),
		k:       columns,
		costs:   costs,
		grammar: dfa{k: columns},
		intern:  make(map[nodeKey]*decodeNode),
		root:    &decodeNode{},
	}
	acceptCell, err := s.solve()
	if err != nil {
		return nil, err
	}
	res := &Result{Cost: acceptCell.cost}

	materialize := func(edges []edge) Witness {
		edits, repaired, node := s.materialize(edges)
		return Witness{
			Edits:    edits,
			Repaired: string(repaired),
			Records:  decodeNodeToRecords(node, s.k),
		}
	}

	if !acceptCell.amb() {
		edges := s.backtrackToNode(0, acceptCell.cands[0].node)
		w := materialize(edges)
		res.Edits = w.Edits
		res.Repaired = w.Repaired
		res.Records = w.Records
		return res, nil
	}

	// Ambiguous: the accept cell carries two distinct canonical nodes.
	// Trace each one independently; because the nodes differ, the
	// reconstructed records differ.
	res.Ambiguous = true
	for i := 0; i < 2; i++ {
		edges := s.backtrackToNode(0, acceptCell.cands[i].node)
		res.Witnesses[i] = materialize(edges)
	}
	if recordsEqualSlices(res.Witnesses[0].Records, res.Witnesses[1].Records) {
		return nil, errors.New("csvfix: internal error: ambiguous witnesses decode to equal records")
	}
	return res, nil
}

// recordsEqualSlices compares two ordered record sets.
func recordsEqualSlices(a, b []Record) bool {
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
