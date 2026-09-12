// Package erd renders a database's entity-relationship structure — tables,
// columns, keys, foreign-key edges — as a box-drawn terminal diagram or a
// Mermaid erDiagram. Structure only, never data: the same boundary as the
// schema_of MCP tool.
//
// Identity discipline: a database may hold two tables of the same bare name in
// different schemas (public.orders + analytics.orders). Every renderer
// therefore keys, routes and cross-references by the QUALIFIED
// "schema.table" identity, and only DISPLAYS the shortest unambiguous form
// (bare when unique, qualified otherwise) via Schema.nameView. Keying by bare
// names routed foreign-key lines to the wrong box whenever names collided.
package erd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pgrundev/pgbot/internal/render"
)

type Column struct {
	Name string
	Type string
	PK   bool
	// FKTarget is the qualified "schema.table.column" this column references
	// (Introspect always qualifies). A bare "table.column" — hand-built
	// fixtures, pre-qualification data — still renders and resolves when the
	// bare name is unambiguous.
	FKTarget string
}

type Table struct {
	Schema  string
	Name    string
	Columns []Column
	Indexes []Index // non-primary indexes (the PK marker already covers its index)
}

// Index is one non-primary index, its definition compacted to method+columns.
type Index struct {
	Name   string
	Def    string // e.g. "btree (customer_id)" — from pg_get_indexdef
	Unique bool
}

// DBInfo is the header line: which database this diagram describes.
type DBInfo struct {
	Database  string
	Version   string
	SizeBytes int64
}

// Edge is one foreign-key relationship. The schema fields are the identity:
// FromQual/ToQual are what every renderer keys on; bare names are display
// forms only.
type Edge struct {
	FromSchema string // the referencing (child) table's schema
	FromTable  string // bare table name
	FromColumn string
	ToSchema   string // the referenced (parent) table's schema
	ToTable    string // bare table name
	ToColumn   string
}

// FromQual is the child's qualified "schema.table" identity.
func (e Edge) FromQual() string { return e.FromSchema + "." + e.FromTable }

// ToQual is the parent's qualified "schema.table" identity.
func (e Edge) ToQual() string { return e.ToSchema + "." + e.ToTable }

type Schema struct {
	Tables []Table
	Edges  []Edge
	Info   DBInfo
}

// nameView is the diagram's naming contract: qualified identity in, shortest
// unambiguous display name out. A bare name that exists in more than one
// schema must be displayed qualified or every reference to it becomes a lie.
type nameView struct {
	display map[string]string // qualified -> display form
	bare    map[string]string // bare -> qualified, present only when unique
}

func (s Schema) nameView() nameView {
	counts := make(map[string]int, len(s.Tables))
	for _, t := range s.Tables {
		counts[t.Name]++
	}
	nv := nameView{
		display: make(map[string]string, len(s.Tables)),
		bare:    make(map[string]string, len(s.Tables)),
	}
	for _, t := range s.Tables {
		q := t.Schema + "." + t.Name
		if counts[t.Name] > 1 {
			nv.display[q] = q // ambiguous: only the qualified form identifies it
		} else {
			nv.display[q] = t.Name
			nv.bare[t.Name] = q
		}
	}
	return nv
}

// of returns the display form of a qualified key; unknown keys render as-is.
func (v nameView) of(qualified string) string {
	if d, ok := v.display[qualified]; ok {
		return d
	}
	return qualified
}

// resolve maps a reference — qualified, or bare when unambiguous — to its
// qualified key. ok is false when the reference names nothing known, or names
// a bare table that exists in several schemas (a bare ref to a duplicated name
// cannot be trusted to mean any one of them).
func (v nameView) resolve(ref string) (string, bool) {
	if _, ok := v.display[ref]; ok {
		return ref, true
	}
	if q, ok := v.bare[ref]; ok {
		return q, true
	}
	return "", false
}

// qualEdge is an Edge with its endpoints resolved to qualified keys, so
// renderers never have to guess identity from a bare name.
type qualEdge struct {
	edge     Edge
	from, to string // resolved qualified keys
}

// qualEdges resolves every edge's endpoints against the schema's own table
// list. Bare endpoint names (hand-built fixtures, schema-less data) resolve
// through the name view; unresolvable endpoints keep their raw form and simply
// fail lookup later — an edge to a filtered-out or unknown table was never
// drawn, and must not silently re-route to a same-named table elsewhere.
func (s Schema) qualEdges() []qualEdge {
	nv := s.nameView()
	out := make([]qualEdge, 0, len(s.Edges))
	for _, e := range s.Edges {
		qf, qt := e.FromQual(), e.ToQual()
		// An endpoint with no schema (hand-built fixtures, schema-less data)
		// resolves from its bare name — unambiguously, or not at all: a bare
		// reference to a name that exists in several schemas must not silently
		// pick one of them.
		if e.FromSchema == "" {
			qf = e.FromTable
		}
		if e.ToSchema == "" {
			qt = e.ToTable
		}
		qe := qualEdge{edge: e, from: qf, to: qt}
		if r, ok := nv.resolve(qe.from); ok {
			qe.from = r
		}
		if r, ok := nv.resolve(qe.to); ok {
			qe.to = r
		}
		out = append(out, qe)
	}
	return out
}

// fkParentQual returns the parent-table reference inside an FKTarget ("a.b.c"
// → "a.b"; "b.c" → "b"). It is a REFERENCE, not yet a resolved identity.
func fkParentQual(target string) string {
	parts := strings.Split(target, ".")
	if len(parts) >= 3 {
		return parts[0] + "." + parts[1]
	}
	if i := strings.LastIndexByte(target, '.'); i > 0 {
		return target[:i]
	}
	return target
}

// fkDisplay renders an FKTarget for the box marker under the same ambiguity
// rule as every other reference in the diagram: bare parent when unambiguous,
// qualified when another schema owns the same name.
func fkDisplay(target string, nv nameView) string {
	parent := fkParentQual(target)
	q, ok := nv.resolve(parent)
	if !ok {
		return target // unknown target: show the raw marker, route nothing
	}
	return nv.of(q) + "." + strings.TrimPrefix(strings.TrimPrefix(target, parent), ".")
}

// headerLine summarizes the database and the diagram: name, server version,
// size, and the counts of what is drawn.
func (s Schema) headerLine() string {
	idx := 0
	for _, t := range s.Tables {
		idx += len(t.Indexes)
	}
	parts := []string{}
	if s.Info.Database != "" {
		parts = append(parts, s.Info.Database)
	}
	if s.Info.Version != "" {
		parts = append(parts, s.Info.Version)
	}
	parts = append(parts, fmt.Sprintf("%d tables", len(s.Tables)), fmt.Sprintf("%d FKs", len(s.Edges)))
	if idx > 0 {
		parts = append(parts, fmt.Sprintf("%d indexes", idx))
	}
	if s.Info.SizeBytes > 0 {
		parts = append(parts, render.HumanBytes(s.Info.SizeBytes))
	}
	return strings.Join(parts, " · ")
}

// sortTables orders tables canonically: schema, then name.
func sortTables(tables []Table) {
	sort.Slice(tables, func(i, j int) bool {
		if tables[i].Schema != tables[j].Schema {
			return tables[i].Schema < tables[j].Schema
		}
		return tables[i].Name < tables[j].Name
	})
}

// sortEdges orders resolved edges canonically: parent, then child, then the
// referencing column — a total order even with duplicated bare names.
func sortEdges(edges []qualEdge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].to != edges[j].to {
			return edges[i].to < edges[j].to
		}
		if edges[i].from != edges[j].from {
			return edges[i].from < edges[j].from
		}
		return edges[i].edge.FromColumn < edges[j].edge.FromColumn
	})
}

// RenderASCII draws one box per table (name, columns, PK/FK markers) with each
// foreign key ROUTED as a line in a left gutter — corner at the FK row, a
// vertical lane, an arrowhead into the parent's title row — followed by a
// crow's-foot relationship forest. Deterministic: sorted tables, sorted edges,
// lanes assigned in order. Boxes and routing are keyed by qualified identity,
// so same-named tables in different schemas each get their own lines.
func RenderASCII(s Schema, color bool) string {
	if len(s.Tables) == 0 {
		return "no tables found (empty schema, or the role cannot see them)\n"
	}
	var b strings.Builder
	b.WriteString(s.headerLine() + "\n\n")

	nv := s.nameView()
	tables := append([]Table(nil), s.Tables...)
	sortTables(tables)

	// Render boxes to lines, remembering each table's title row and each FK
	// column's row. titleRow is keyed by the QUALIFIED name — with
	// public.orders and analytics.orders both present, the bare key would
	// overwrite one box with the other and re-route its edges.
	var lines []string
	titleRow := map[string]int{}
	type conn struct{ childRow, parentRow int }
	var conns []conn
	var fkRows []struct {
		row    int
		target string // resolved qualified parent
	}
	for _, t := range tables {
		titleRow[t.Schema+"."+t.Name] = len(lines)
		var box strings.Builder
		writeTableBox(&box, t, nv)
		boxLines := strings.Split(strings.TrimRight(box.String(), "\n"), "\n")
		for i, c := range t.Columns {
			if c.FKTarget == "" {
				continue
			}
			parent := fkParentQual(c.FKTarget)
			if q, ok := nv.resolve(parent); ok {
				parent = q
			}
			fkRows = append(fkRows, struct {
				row    int
				target string
			}{len(lines) + 1 + i, parent})
		}
		lines = append(lines, boxLines...)
		lines = append(lines, "")
	}
	for _, fk := range fkRows {
		if pr, ok := titleRow[fk.target]; ok {
			conns = append(conns, conn{childRow: fk.row, parentRow: pr})
		}
	}

	// Lane assignment: longest spans take the outer lanes; a lane is reused
	// when row ranges don't overlap. Capped so a monster schema degrades to
	// the textual FK markers instead of an unreadable gutter.
	const maxLanes = 8
	type lane struct{ spans [][2]int }
	var lanes []lane
	laneOf := make([]int, len(conns))
	sort.SliceStable(conns, func(i, j int) bool {
		si := abs(conns[i].childRow - conns[i].parentRow)
		sj := abs(conns[j].childRow - conns[j].parentRow)
		if si != sj {
			return si > sj
		}
		return conns[i].childRow < conns[j].childRow
	})
	for i, c := range conns {
		lo, hi := minInt(c.childRow, c.parentRow), maxInt(c.childRow, c.parentRow)
		laneOf[i] = -1
		for li := range lanes {
			free := true
			for _, sp := range lanes[li].spans {
				if lo <= sp[1] && sp[0] <= hi {
					free = false
					break
				}
			}
			if free {
				lanes[li].spans = append(lanes[li].spans, [2]int{lo, hi})
				laneOf[i] = li
				break
			}
		}
		if laneOf[i] == -1 && len(lanes) < maxLanes {
			lanes = append(lanes, lane{spans: [][2]int{{lo, hi}}})
			laneOf[i] = len(lanes) - 1
		}
	}

	gw := len(lanes) * 2 // gutter width: 2 columns per lane
	if gw > 0 {
		gw += 2 // room for the horizontal run and arrowhead next to the boxes
		grid := make([][]rune, len(lines))
		for i := range grid {
			grid[i] = []rune(strings.Repeat(" ", gw))
		}
		put := func(row, col int, r rune) {
			cur := grid[row][col]
			switch {
			case cur == ' ':
				grid[row][col] = r
			case (cur == '│' && r == '─') || (cur == '─' && r == '│'):
				grid[row][col] = '┼'
			}
		}
		for i, c := range conns {
			if laneOf[i] < 0 {
				continue // over the lane cap: the FK → marker still tells the story
			}
			col := laneOf[i] * 2 // outer lanes (longest spans) leftmost
			lo, hi := minInt(c.childRow, c.parentRow), maxInt(c.childRow, c.parentRow)
			for r := lo + 1; r < hi; r++ {
				put(r, col, '│')
			}
			for x := col + 1; x < gw-1; x++ {
				put(lo, x, '─')
				put(hi, x, '─')
			}
			put(lo, col, '┌')
			put(hi, col, '└')
			// Both endpoints get a horizontal run to the box edge; the parent
			// row additionally carries the arrowhead. Direction is expressed by
			// WHICH row carries ▶, not by different glyphs, so the two
			// row-orders below are one statement (the old code had a
			// byte-identical if/else here — a dead branch masking intent).
			grid[c.parentRow][gw-1] = '▶'
			put(c.childRow, gw-1, '─')
		}
		for i, l := range lines {
			b.WriteString(strings.TrimRight(string(grid[i])+l, " "))
			b.WriteString("\n")
		}
	} else {
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	writeForest(&b, s)
	return b.String()
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// writeTableBox renders one table:
//
//	┌─ public.orders ───────────────────┐
//	│ id           bigint   PK          │
//	│ customer_id  bigint   FK → customers.id │
//	└───────────────────────────────────┘
//
// The box title is always schema-qualified; FK targets follow the diagram's
// ambiguity rule (bare parent when unique, qualified when duplicated).
func writeTableBox(b *strings.Builder, t Table, nv nameView) {
	nameW, typeW := 0, 0
	for _, c := range t.Columns {
		nameW = max(nameW, len(c.Name))
		typeW = max(typeW, len(c.Type))
	}
	var rows []string
	for _, c := range t.Columns {
		marker := ""
		switch {
		case c.PK && c.FKTarget != "":
			marker = "PK FK → " + fkDisplay(c.FKTarget, nv)
		case c.PK:
			marker = "PK"
		case c.FKTarget != "":
			marker = "FK → " + fkDisplay(c.FKTarget, nv)
		}
		rows = append(rows, strings.TrimRight(
			fmt.Sprintf("%-*s  %-*s  %s", nameW, c.Name, typeW, c.Type, marker), " "))
	}
	var idxRows []string
	for _, ix := range t.Indexes {
		row := ix.Name + "  " + ix.Def
		if ix.Unique {
			row += "  UNIQUE"
		}
		idxRows = append(idxRows, row)
	}
	title := t.Schema + "." + t.Name
	inner := len(title) + 4
	for _, r := range append(append([]string(nil), rows...), idxRows...) {
		inner = max(inner, len(r)+2)
	}
	fmt.Fprintf(b, "┌─ %s %s┐\n", title, strings.Repeat("─", inner-len(title)-3))
	for _, r := range rows {
		fmt.Fprintf(b, "│ %-*s│\n", inner-1, r)
	}
	if len(idxRows) > 0 {
		fmt.Fprintf(b, "├%s┤\n", strings.Repeat("─", inner))
		for _, r := range idxRows {
			fmt.Fprintf(b, "│ %-*s│\n", inner-1, r)
		}
	}
	fmt.Fprintf(b, "└%s┘\n", strings.Repeat("─", inner))
}

// writeForest prints the FK graph as parent-owns-children trees:
//
//	customers
//	 └─< orders (customer_id)
//	     └─< order_items (order_id)   · also < products
//
// Each child appears once, under its first (canonically sorted) parent;
// additional parents show as a cross-link. Cycle-safe via a visited set. All
// keys are qualified identities; display names go through the name view, so a
// duplicated bare name never prints (or resolves) as its foreign twin.
func writeForest(b *strings.Builder, s Schema) {
	if len(s.Edges) == 0 {
		return
	}
	nv := s.nameView()
	b.WriteString("Relationships\n")

	edges := s.qualEdges()
	sortEdges(edges)
	children := map[string][]qualEdge{} // qualified parent → edges into it
	firstParent := map[string]string{}
	hasParent := map[string]bool{}
	for _, e := range edges {
		children[e.to] = append(children[e.to], e)
		hasParent[e.from] = true
		if _, ok := firstParent[e.from]; !ok {
			firstParent[e.from] = e.to
		}
	}

	var roots []string
	for parent := range children {
		if !hasParent[parent] {
			roots = append(roots, parent)
		}
	}
	sort.Strings(roots)

	visited := map[string]bool{}
	var walk func(table, indent string)
	walk = func(table, indent string) {
		if visited[table] {
			return
		}
		visited[table] = true
		kids := children[table]
		for i, e := range kids {
			branch := "├─<"
			childIndent := indent + "│   "
			if i == len(kids)-1 {
				branch = "└─<"
				childIndent = indent + "    "
			}
			line := fmt.Sprintf("%s%s %s (%s)", indent, branch, nv.of(e.from), e.edge.FromColumn)
			if firstParent[e.from] != table {
				line += "  · also above"
				fmt.Fprintln(b, line)
				continue
			}
			fmt.Fprintln(b, line)
			walk(e.from, childIndent)
		}
	}
	for _, r := range roots {
		fmt.Fprintln(b, nv.of(r))
		walk(r, " ")
	}
	// Cycles (every member has a parent) still deserve printing.
	var leftovers []string
	for parent := range children {
		if !visited[parent] {
			leftovers = append(leftovers, parent)
		}
	}
	sort.Strings(leftovers)
	for _, r := range leftovers {
		fmt.Fprintln(b, nv.of(r)+"  (cycle)")
		walk(r, " ")
	}
}

// mermaidID makes a display name safe as a mermaid entity identifier
// (alphanumerics and underscore; a qualified "analytics.orders" becomes
// "analytics_orders"). Edges and entity blocks are generated from the same
// transformed id, so they always reference each other.
func mermaidID(display string) string {
	var b strings.Builder
	for _, r := range display {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// RenderMermaid emits a mermaid erDiagram — pasteable into GitHub markdown or
// mermaid.live for an interactive pan/zoom view. Entities whose bare name is
// ambiguous across schemas render with their schema folded into the id.
func RenderMermaid(s Schema) string {
	nv := s.nameView()
	var b strings.Builder
	b.WriteString("erDiagram\n")
	edges := s.qualEdges()
	sortEdges(edges)
	for _, e := range edges {
		fmt.Fprintf(&b, "    %s ||--o{ %s : %s\n", mermaidID(nv.of(e.to)), mermaidID(nv.of(e.from)), e.edge.FromColumn)
	}
	tables := append([]Table(nil), s.Tables...)
	sortTables(tables)
	for _, t := range tables {
		fmt.Fprintf(&b, "    %s {\n", mermaidID(nv.of(t.Schema+"."+t.Name)))
		for _, c := range t.Columns {
			marker := ""
			switch {
			case c.PK && c.FKTarget != "":
				marker = " PK, FK"
			case c.PK:
				marker = " PK"
			case c.FKTarget != "":
				marker = " FK"
			}
			// Mermaid types must be bare words: "character varying(64)" breaks it.
			typ := strings.NewReplacer(" ", "_", "(", "_", ")", "", ",", "_").Replace(c.Type)
			fmt.Fprintf(&b, "        %s %s%s\n", typ, c.Name, marker)
		}
		b.WriteString("    }\n")
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
