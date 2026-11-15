package compiler

import (
	"strings"
	"unicode/utf8"
)

// The boundary map of the trust view (ST-GEN-UC08-R11): each trust boundary of the model drawn as a region nested in
// the one that holds it, with the elements it holds as chips inside it, and each element that crosses it as a gate
// on its top border. The network edge holds the verified identity, and the verified identity holds the tenant
// scope, the fill code and the process; a boundary that the model does not hold is left out, and its inner regions
// go into the next one out. Each element shows once as a chip, in the innermost region that holds it; an element
// that crosses a boundary shows as a gate of it too.

// trustPair is the wide and the narrow form of the boundary map.
type trustPair struct {
	Wide   trustDiagram `json:"wide"`
	Narrow trustDiagram `json:"narrow"`
}

// trustDiagram is the boundary map at one width.
type trustDiagram struct {
	W       int           `json:"w"`
	H       int           `json:"h"`
	Regions []trustRegion `json:"regions"`
	Nodes   []layoutNode  `json:"nodes"`
}

// trustRegion is the box of one boundary.
type trustRegion struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Depth int    `json:"depth"`
}

// trustBox is a boundary while it is laid out: its gates, its chips and its inner boxes.
type trustBox struct {
	boundary ArchBoundary
	gates    []layoutNode
	chips    []layoutNode
	inner    []*trustBox
}

// The sizes of the boundary map.
const (
	chipH      = 30
	chipGap    = 8
	regionPad  = 12
	legendH    = 22
	chipMinW   = 92
	chipMaxW   = 168
	innerMinW  = 250
	innerSpace = 26
)

// chipWidth gives the width of a chip for its label: about 7 px a character, in its bounds.
func chipWidth(label string) int {
	return min(max(utf8.RuneCountInString(label)*7+22, chipMinW), chipMaxW)
}

// trustLabel gives the label of an element on the map, as the viewer names it.
func trustLabel(m *ArchitectureModel, id string) string {
	if strings.HasPrefix(id, "slot:") {
		for _, s := range m.Slots {
			if s.ID == id {
				return shortID(s.Collection) + "." + s.Name
			}
		}
	}
	for _, a := range m.Actors {
		if a.ID == id {
			return a.Name
		}
	}
	return shortID(id)
}

// trustChip gives the chip of an element, coloured by its layer.
func trustChip(m *ArchitectureModel, id, shape string) layoutNode {
	layer := ""
	switch {
	case strings.HasPrefix(id, "component:"):
		for _, c := range m.Components {
			if c.ID == id {
				layer = c.Layer
			}
		}
	case strings.HasPrefix(id, "slot:"), strings.HasPrefix(id, "fill:"):
		layer = "slot"
	case strings.HasPrefix(id, "entity:"):
		layer = "repository"
	case strings.HasPrefix(id, "collection:"), strings.HasPrefix(id, "operation:"), strings.HasPrefix(id, "need:"):
		layer = "edge"
	case strings.HasPrefix(id, "actor:"):
		layer = "outside call"
	}
	return layoutNode{ID: id, W: chipWidth(trustLabel(m, id)), H: chipH, Shape: shape, Layer: layer}
}

// trustLayout lays out the boundary map at a width: the caller and the token issuers above the network edge, then
// each boundary nested in the one that holds it.
func trustLayout(m *ArchitectureModel, width int) trustDiagram {
	byKind := map[string]ArchBoundary{}
	for _, b := range m.Boundaries {
		byKind[b.Kind] = b
	}
	network, ok := byKind["network"]
	if !ok {
		return trustDiagram{W: width, Regions: []trustRegion{}, Nodes: []layoutNode{}}
	}
	root := &trustBox{boundary: network}
	identity := root
	if b, ok := byKind["verified identity"]; ok {
		identity = &trustBox{boundary: b}
		root.inner = append(root.inner, identity)
	}
	var inner []*trustBox
	for _, kind := range []string{"tenant scope", "fill code", "process"} {
		if b, ok := byKind[kind]; ok {
			inner = append(inner, &trustBox{boundary: b})
		}
	}
	identity.inner = append(identity.inner, inner...)
	// Where each element shows as a chip: the innermost box that holds it.
	placed := map[string]bool{}
	for _, box := range inner {
		for _, id := range box.boundary.Contains {
			if !placed[id] {
				placed[id] = true
				box.chips = append(box.chips, trustChip(m, id, chipShape(id)))
			}
		}
	}
	if identity != root {
		for _, id := range identity.boundary.Contains {
			if !placed[id] {
				placed[id] = true
				identity.chips = append(identity.chips, trustChip(m, id, chipShape(id)))
			}
		}
	}
	crossers := map[string]bool{}
	for _, box := range append([]*trustBox{root, identity}, inner...) {
		if box == identity && identity == root {
			continue
		}
		for _, id := range box.boundary.CrossedBy {
			crossers[id] = true
		}
	}
	// The network edge holds the service container: each of its components that no inner box holds or that
	// crosses no inner boundary shows in it.
	for _, holds := range network.Contains {
		for _, c := range m.Components {
			if c.Container == holds && !placed[c.ID] && !crossers[c.ID] {
				placed[c.ID] = true
				root.chips = append(root.chips, trustChip(m, c.ID, "chip"))
			}
		}
	}
	// The gates of the network edge: the collections whose operations cross it, and each other element that
	// crosses it; the gates of each other box are the elements that cross it.
	entry := map[string]bool{}
	for _, e := range networkEntries(m) {
		root.gates = append(root.gates, trustChip(m, e.id, "gate"))
		for _, c := range m.Collections {
			if c.ID == e.id {
				for _, op := range c.Operations {
					entry[op] = true
				}
			}
		}
	}
	for _, id := range network.CrossedBy {
		if !entry[id] {
			root.gates = append(root.gates, trustChip(m, id, "gate"))
		}
	}
	for _, box := range append([]*trustBox{identity}, inner...) {
		if box == root {
			continue
		}
		for _, id := range box.boundary.CrossedBy {
			box.gates = append(box.gates, trustChip(m, id, "gate"))
		}
	}
	d := trustDiagram{W: width, Regions: []trustRegion{}, Nodes: []layoutNode{}}
	// The caller and the token issuers, outside the network edge.
	x, y := 8, 8
	for _, a := range m.Actors {
		if a.Kind != actorCaller && a.Kind != actorTokenIssuer {
			continue
		}
		n := trustChip(m, a.ID, "actor")
		n.W, n.H = max(n.W, 120), 40
		if x+n.W > width-8 {
			x, y = 8, y+n.H+chipGap
		}
		n.X, n.Y = x, y
		d.Nodes = append(d.Nodes, n)
		x += n.W + chipGap
	}
	if x > 8 {
		y += 40 + innerSpace
	} else {
		y += innerSpace
	}
	h := layoutBox(&d, root, 4, y, width-8, 0)
	d.H = y + h + 10
	return d
}

// chipShape gives the shape of a chip: a slot is a hexagon, as in the component view.
func chipShape(id string) string {
	if strings.HasPrefix(id, "slot:") {
		return "slot"
	}
	return "chip"
}

// rowsOf places chips in rows from x within a width, starting at y, and gives the y under the last row.
func rowsOf(d *trustDiagram, chips []layoutNode, x, y, width int) int {
	if len(chips) == 0 {
		return y
	}
	cx, cy := x, y
	for _, c := range chips {
		if cx > x && cx+c.W > x+width {
			cx, cy = x, cy+chipH+chipGap
		}
		c.X, c.Y = cx, cy
		d.Nodes = append(d.Nodes, c)
		cx += c.W + chipGap
	}
	return cy + chipH
}

// layoutBox lays out a box at x, y with a width and gives its height. Its first row of gates sits across its top
// border, after its legend; more gates go in rows under it. Its chips follow, then its inner boxes, side by side
// when each gets at least innerMinW, else one under the other.
func layoutBox(d *trustDiagram, box *trustBox, x, y, width, depth int) int {
	legend := utf8.RuneCountInString(box.boundary.Kind)*8 + 28
	ix, iw := x+regionPad, width-2*regionPad
	cy := y + legendH
	if len(box.gates) > 0 {
		gx, gy := x+legend, y-chipH/2
		for i, g := range box.gates {
			if gx > x+legend && gx+g.W > x+width-regionPad {
				gx, gy = ix, max(gy+chipH+chipGap, y+legendH)
			}
			box.gates[i].X, box.gates[i].Y = gx, gy
			gx += g.W + chipGap
		}
		cy = max(cy, box.gates[len(box.gates)-1].Y+chipH+chipGap+4)
		d.Nodes = append(d.Nodes, box.gates...)
	}
	cy = rowsOf(d, box.chips, ix, cy, iw)
	if len(box.chips) > 0 {
		cy += chipGap
	}
	if n := len(box.inner); n > 0 {
		cy += innerSpace
		if iw >= n*innerMinW+(n-1)*regionPad {
			w := (iw - (n-1)*regionPad) / n
			tallest := 0
			heights := make([]int, n)
			mark := len(d.Regions)
			for i, b := range box.inner {
				heights[i] = layoutBox(d, b, ix+i*(w+regionPad), cy, w, depth+1)
				tallest = max(tallest, heights[i])
			}
			// The boxes side by side take the height of the tallest.
			for i := mark; i < len(d.Regions); i++ {
				if d.Regions[i].Depth == depth+1 {
					d.Regions[i].H = tallest
				}
			}
			cy += tallest
		} else {
			for i, b := range box.inner {
				if i > 0 {
					cy += innerSpace
				}
				cy += layoutBox(d, b, ix, cy, iw, depth+1)
			}
		}
	}
	h := cy + regionPad - y
	d.Regions = append(d.Regions, trustRegion{ID: box.boundary.ID, Kind: box.boundary.Kind, X: x, Y: y, W: width, H: h, Depth: depth})
	return h
}
