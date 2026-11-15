package compiler

import (
	"strconv"
	"strings"
)

// The layout of the architecture view (ST-GEN-UC08-R04): apply computes the place of each element of each diagram
// from the architecture model alone, on a fixed grid, so the same model always gives the same picture and the
// viewer holds no layout engine. Each diagram has a wide form and a narrow form for screens of 480 px and less.

// viewLayout is the layout block that the view embeds beside the model.
type viewLayout struct {
	Overview   diagramPair `json:"overview"`
	Components diagramPair `json:"components"`
	Data       diagramPair `json:"data"`
	Flows      flowLayout  `json:"flows"`
}

// diagramPair is the wide and the narrow form of one diagram.
type diagramPair struct {
	Wide   diagram `json:"wide"`
	Narrow diagram `json:"narrow"`
}

// diagram is one diagram: its size, its lanes, the box of each element, the lines between them and the regions of
// the trust boundaries over it.
type diagram struct {
	W       int          `json:"w"`
	H       int          `json:"h"`
	Lanes   []layoutLane `json:"lanes,omitempty"`
	Nodes   []layoutNode `json:"nodes"`
	Edges   []layoutEdge `json:"edges,omitempty"`
	Regions []region     `json:"regions,omitempty"`
}

// layoutLane is the column (wide) or the band (narrow) of one layer.
type layoutLane struct {
	Layer string `json:"layer"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
}

// layoutNode is the box of one element of the model, by its ID.
type layoutNode struct {
	ID    string `json:"id"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Shape string `json:"shape"`
	Layer string `json:"layer,omitempty"`
}

// layoutEdge is a line of the overview from one element to another, with its fixed label.
type layoutEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
}

// region is where one trust boundary shows over the component lanes: a rectangle over each run of lanes that the
// boundary holds whole, and an outline round each other element that it holds.
type region struct {
	ID       string   `json:"id"`
	Rects    [][4]int `json:"rects"`
	Outlines []string `json:"outlines"`
}

// flowLayout gives the lane of each step of each flow, as an index into the layers.
type flowLayout struct {
	W     int              `json:"w"`
	LaneW int              `json:"lane_w"`
	Steps map[string][]int `json:"steps"`
}

// layoutArchitecture computes the layout of every diagram of the view from the model.
func layoutArchitecture(m *ArchitectureModel) viewLayout {
	return viewLayout{
		Overview:   diagramPair{Wide: overviewLayout(m, false), Narrow: overviewLayout(m, true)},
		Components: diagramPair{Wide: componentLayout(m, false), Narrow: componentLayout(m, true)},
		Data:       diagramPair{Wide: dataLayout(m, false), Narrow: dataLayout(m, true)},
		Flows:      flowLanes(m),
	}
}

// entryGroup is one collection with the operations of it that cross the network boundary.
type entryGroup struct {
	id  string
	ops int
}

// networkEntries gives the collections whose operations cross the network boundary, in model order, and the
// number of their operations that cross it.
func networkEntries(m *ArchitectureModel) []entryGroup {
	crossing := map[string]bool{}
	for _, b := range m.Boundaries {
		if b.Kind == "network" {
			for _, id := range b.CrossedBy {
				crossing[id] = true
			}
		}
	}
	var groups []entryGroup
	for _, c := range m.Collections {
		n := 0
		for _, op := range c.Operations {
			if crossing[op] {
				n++
			}
		}
		if n > 0 {
			groups = append(groups, entryGroup{id: c.ID, ops: n})
		}
	}
	return groups
}

// overviewLayout places the context view: the collections that callers enter by on the left, the service
// containers inside the network edge in the middle, and the containers that needs state on the right.
func overviewLayout(m *ArchitectureModel, narrow bool) diagram {
	entries := networkEntries(m)
	var services, others []ArchContainer
	for _, c := range m.Containers {
		if c.Kind == "service" {
			services = append(services, c)
		} else {
			others = append(others, c)
		}
	}
	d := diagram{}
	var network [][4]int
	if !narrow {
		const rowH, boxH = 58, 46
		rows := max(len(entries), len(others), len(services)*2, 3)
		d.W, d.H = 900, 40+rows*rowH+10
		for i, e := range entries {
			d.Nodes = append(d.Nodes, layoutNode{ID: e.id, X: 20, Y: 40 + i*rowH, W: 180, H: boxH, Shape: "entry"})
		}
		top := (d.H - len(services)*100) / 2
		for i, c := range services {
			d.Nodes = append(d.Nodes, layoutNode{ID: c.ID, X: 330, Y: top + i*100, W: 240, H: 80, Shape: "box", Layer: "handler"})
		}
		if len(services) > 0 {
			network = append(network, [4]int{300, top - 30, 300, len(services)*100 + 40})
		}
		for i, c := range others {
			d.Nodes = append(d.Nodes, layoutNode{ID: c.ID, X: 700, Y: 40 + i*rowH, W: 180, H: boxH + 6, Shape: "store", Layer: "store"})
		}
	} else {
		const boxH = 42
		d.W = 360
		y := 30
		for i, e := range entries {
			d.Nodes = append(d.Nodes, layoutNode{ID: e.id, X: 10 + (i%2)*175, Y: y + (i/2)*54, W: 165, H: boxH, Shape: "entry"})
		}
		y += (len(entries)+1)/2*54 + 40
		for i, c := range services {
			d.Nodes = append(d.Nodes, layoutNode{ID: c.ID, X: 60, Y: y + i*96, W: 240, H: 72, Shape: "box", Layer: "handler"})
		}
		if len(services) > 0 {
			network = append(network, [4]int{30, y - 24, 300, len(services)*96 + 30})
		}
		y += len(services)*96 + 30
		for i, c := range others {
			d.Nodes = append(d.Nodes, layoutNode{ID: c.ID, X: 10 + (i%2)*175, Y: y + (i/2)*60, W: 165, H: 48, Shape: "store", Layer: "store"})
		}
		y += (len(others)+1)/2*60 + 10
		d.H = y
	}
	for _, b := range m.Boundaries {
		if b.Kind == "network" && len(network) > 0 {
			d.Regions = append(d.Regions, region{ID: b.ID, Rects: network, Outlines: []string{}})
		}
	}
	for _, s := range services {
		for _, e := range entries {
			d.Edges = append(d.Edges, layoutEdge{From: e.id, To: s.ID, Label: plural(e.ops, "operation")})
		}
		for _, o := range others {
			d.Edges = append(d.Edges, layoutEdge{From: s.ID, To: o.ID, Label: o.Kind})
		}
	}
	return d
}

// plural gives a count with its noun, as "1 operation" or "4 operations".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// laneItems gives the elements of each layer of the component view, in model order: the components of the layer,
// and in the slot layer each slot.
func laneItems(m *ArchitectureModel) map[string][]layoutNode {
	items := map[string][]layoutNode{}
	for _, c := range m.Components {
		shape := "box"
		if c.Layer == "store" {
			shape = "store"
		}
		items[c.Layer] = append(items[c.Layer], layoutNode{ID: c.ID, Shape: shape, Layer: c.Layer})
	}
	for _, s := range m.Slots {
		items["slot"] = append(items["slot"], layoutNode{ID: s.ID, Shape: "slot", Layer: "slot"})
	}
	return items
}

// componentLayout places every component of the active set in the lane of its layer, and each slot in the slot
// lane, then the regions of the trust boundaries over them.
func componentLayout(m *ArchitectureModel, narrow bool) diagram {
	items := laneItems(m)
	d := diagram{}
	if !narrow {
		const cw, gap, top, nh, vg = 150, 14, 34, 46, 14
		rows := 1
		for _, l := range m.Layers {
			rows = max(rows, len(items[l]))
		}
		d.W, d.H = len(m.Layers)*(cw+gap)+gap, top+rows*(nh+vg)+24
		for i, l := range m.Layers {
			x := gap + i*(cw+gap)
			d.Lanes = append(d.Lanes, layoutLane{Layer: l, X: x - 6, Y: 6, W: cw + 12, H: d.H - 12})
			for j, n := range items[l] {
				n.X, n.Y, n.W, n.H = x, top+j*(nh+vg), cw, nh
				d.Nodes = append(d.Nodes, n)
			}
		}
	} else {
		const lw, lx, nw, nh = 360, 8, 160, 42
		y := 6
		for _, l := range m.Layers {
			list := items[l]
			rows := max((len(list)+1)/2, 1)
			h := 24 + rows*(nh+10)
			d.Lanes = append(d.Lanes, layoutLane{Layer: l, X: lx - 4, Y: y, W: lw - 8, H: h})
			for j, n := range list {
				n.X, n.Y, n.W, n.H = lx+6+(j%2)*(nw+14), y+22+(j/2)*(nh+10), nw, nh
				d.Nodes = append(d.Nodes, n)
			}
			y += h + 8
		}
		d.W, d.H = lw, y
	}
	d.Regions = boundaryRegions(m, d, narrow)
	return d
}

// boundaryRegions computes where each trust boundary shows over the component lanes. A boundary holds a container
// through the components in it, and a fill through the slots it implements. A lane whose every element the
// boundary holds is held whole, and so is an empty lane between two such lanes; each run of whole lanes gives one
// rectangle, and each other element held gets an outline. A boundary that holds no element of the diagram gets no
// region.
func boundaryRegions(m *ArchitectureModel, d diagram, narrow bool) []region {
	laneOf := map[string]string{}
	for _, n := range d.Nodes {
		laneOf[n.ID] = n.Layer
	}
	regions := []region{}
	for _, b := range m.Boundaries {
		held := map[string]bool{}
		for _, id := range b.Contains {
			switch {
			case strings.HasPrefix(id, "container:"):
				for _, c := range m.Components {
					if c.Container == id {
						held[c.ID] = true
					}
				}
				for _, s := range m.Slots {
					held[s.ID] = true
				}
			case strings.HasPrefix(id, "fill:"):
				for _, s := range m.Slots {
					for _, f := range s.Fills {
						if f == id {
							held[s.ID] = true
						}
					}
				}
			default:
				if _, ok := laneOf[id]; ok {
					held[id] = true
				}
			}
		}
		if len(held) == 0 {
			continue
		}
		whole := make([]bool, len(d.Lanes))
		empty := make([]bool, len(d.Lanes))
		for i, lane := range d.Lanes {
			n, in := 0, 0
			for _, node := range d.Nodes {
				if node.Layer == lane.Layer {
					n++
					if held[node.ID] {
						in++
					}
				}
			}
			whole[i] = n > 0 && in == n
			empty[i] = n == 0
		}
		r := region{ID: b.ID, Rects: [][4]int{}, Outlines: []string{}}
		for i := 0; i < len(d.Lanes); {
			if !whole[i] {
				i++
				continue
			}
			j := i
			for j+1 < len(d.Lanes) && (whole[j+1] || (empty[j+1] && j+2 < len(d.Lanes) && whole[j+2])) {
				j++
			}
			a, z := d.Lanes[i], d.Lanes[j]
			if narrow {
				r.Rects = append(r.Rects, [4]int{a.X - 2, a.Y - 2, a.W + 4, z.Y + z.H - a.Y + 4})
			} else {
				r.Rects = append(r.Rects, [4]int{a.X - 2, a.Y - 2, z.X + z.W - a.X + 4, a.H + 4})
			}
			i = j + 1
		}
		for i, lane := range d.Lanes {
			if whole[i] {
				continue
			}
			for _, node := range d.Nodes {
				if node.Layer == lane.Layer && held[node.ID] {
					r.Outlines = append(r.Outlines, node.ID)
				}
			}
		}
		regions = append(regions, r)
	}
	return regions
}

// referenceDepth gives each entity its depth by its references: an entity that refers to no other is at depth 0,
// and one that refers to others is one deeper than the deepest of them. A cycle stops at the entity it returns to.
func referenceDepth(m *ArchitectureModel) map[string]int {
	to := map[string][]string{}
	for _, r := range m.References {
		if r.From != r.To {
			to[r.From] = append(to[r.From], r.To)
		}
	}
	depth := map[string]int{}
	var walk func(id string, seen map[string]bool) int
	walk = func(id string, seen map[string]bool) int {
		if d, ok := depth[id]; ok {
			return d
		}
		if seen[id] {
			return 0
		}
		seen[id] = true
		d := 0
		for _, t := range to[id] {
			d = max(d, walk(t, seen)+1)
		}
		delete(seen, id)
		depth[id] = d
		return d
	}
	for _, e := range m.Entities {
		walk(e.ID, map[string]bool{})
	}
	return depth
}

// entityHeight is the height of the box of an entity: its title, a row for each field and its footer.
func entityHeight(e ArchEntity) int { return 30 + len(e.Fields)*18 + 26 }

// dataLayout places each entity by its reference depth: the entities that others refer to on the left, and those
// that refer to them on their right. The narrow form stacks them in one column in the same order.
func dataLayout(m *ArchitectureModel, narrow bool) diagram {
	depth := referenceDepth(m)
	cols := 1
	for _, d := range depth {
		cols = max(cols, d+1)
	}
	d := diagram{}
	if !narrow {
		y := make([]int, cols)
		for i := range y {
			y[i] = 10
		}
		for _, e := range m.Entities {
			c := depth[e.ID]
			h := entityHeight(e)
			d.Nodes = append(d.Nodes, layoutNode{ID: e.ID, X: 20 + c*430, Y: y[c], W: 300, H: h, Shape: "entity"})
			y[c] += h + 26
		}
		d.W, d.H = 20+cols*430-110, 10
		for _, v := range y {
			d.H = max(d.H, v)
		}
		d.W = max(d.W, 340)
	} else {
		y := 10
		for c := 0; c < cols; c++ {
			for _, e := range m.Entities {
				if depth[e.ID] != c {
					continue
				}
				h := entityHeight(e)
				d.Nodes = append(d.Nodes, layoutNode{ID: e.ID, X: 10, Y: y, W: 300, H: h, Shape: "entity"})
				y += h + 26
			}
		}
		d.W, d.H = 360, y
	}
	return d
}

// stepLane gives the layer of a step: the layer that its kind names, or for a step of an authentication, an
// authorization, an event or an outside call, the layer of the component it names.
func stepLane(m *ArchitectureModel, s ArchStep) string {
	fixed := map[string]string{"operation": "edge", "handle": "handler", "slot": "slot", "repository": "repository",
		"outbox": "repository", "store": "store"}
	if l, ok := fixed[s.Kind]; ok {
		return l
	}
	for _, c := range m.Components {
		if c.ID == s.Element {
			return c.Layer
		}
	}
	if s.Kind == "authenticate" || s.Kind == "authorize" {
		return "authentication"
	}
	return "outside call"
}

// flowLanes gives the lane of each step of each flow.
func flowLanes(m *ArchitectureModel) flowLayout {
	const laneW = 96
	index := map[string]int{}
	for i, l := range m.Layers {
		index[l] = i
	}
	f := flowLayout{W: laneW * len(m.Layers), LaneW: laneW, Steps: map[string][]int{}}
	for _, flow := range m.Flows {
		lanes := make([]int, len(flow.Steps))
		for i, s := range flow.Steps {
			lanes[i] = index[stepLane(m, s)]
		}
		f.Steps[flow.ID] = lanes
	}
	return f
}
