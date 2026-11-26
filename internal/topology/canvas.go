package topology

import "strconv"

// The environment canvas places an environment's apps and the resources they
// use as cards on a plane: one framed group per system (apps in the left
// column, the resources they use in the right one) stacked top to bottom,
// and everything in no system in a group of its own. Like the rest of the
// package it is pure; the client only draws the wires between cards.

// Canvas geometry, in CSS pixels on the unscaled plane. CardW mirrors the
// stylesheet's --card-w; CanvasRow has to fit the tallest card (the .svc
// rules in input.css), and the new-app ghost card is placed with it too.
const (
	CanvasCardW     = 288
	canvasPad       = 24  // inner left/right padding of a group
	canvasTop       = 64  // a group's first row, below its name
	CanvasRow       = 124 // vertical step between two cards
	canvasBottom    = 8   // below the last card's row step
	canvasGroupW    = 736 // a two-column group
	canvasLooseW    = 336 // the one-column group beside the systems
	canvasColGap    = 400 // left edge of the right column within a group
	canvasGroupGap  = 32  // between two stacked groups
	canvasSideGap   = 24  // between the systems and the loose group
	canvasMinHeight = 200
)

// CanvasItem is one card: an app ("app:<id>") or a resource (its EnvResource
// key: "ldb:<id>", "bucket:<id>", "inst:<id>").
type CanvasItem struct {
	Key  string
	X, Y int
}

// CanvasGroup is a frame on the plane. Loose marks the group of everything in
// no system; SystemID is 0 for it.
type CanvasGroup struct {
	SystemID   int64
	Name       string
	Loose      bool
	X, Y, W, H int
	Items      []CanvasItem
}

// Canvas is the laid-out plane.
type Canvas struct {
	W, H   int
	Groups []CanvasGroup
}

// AppKey is the canvas key of an app card.
func AppKey(id int64) string { return "app:" + strconv.FormatInt(id, 10) }

// LayoutCanvas places the environment. view groups the apps into systems with
// the resources each system uses; all lists every resource of the
// environment, so that ones no app uses still get a card. A resource used by
// several systems is placed once, in the first of them; the wires reach it
// from the others.
func LayoutCanvas(view EnvView, all []EnvResource) Canvas {
	placed := map[string]bool{}
	var c Canvas
	y := 0
	for _, sy := range view.Systems {
		var res []string
		for _, r := range sy.Resources {
			if !placed[r.Key] {
				res = append(res, r.Key)
				placed[r.Key] = true
			}
		}
		apps := make([]string, 0, len(sy.Apps))
		for _, a := range sy.Apps {
			apps = append(apps, AppKey(a.ID))
		}
		g := CanvasGroup{SystemID: sy.ID, Name: sy.Name, X: 0, Y: y, W: canvasGroupW}
		g.Items, g.H = twoColumns(apps, res)
		c.Groups = append(c.Groups, g)
		y += g.H + canvasGroupGap
	}
	systemsH := 0
	if len(c.Groups) > 0 {
		systemsH = y - canvasGroupGap
	}

	// Everything else: apps in no system, then the resources they use that
	// no system placed, then resources nobody uses.
	var apps, res []string
	for _, a := range view.Ungrouped.Apps {
		apps = append(apps, AppKey(a.ID))
	}
	for _, r := range view.Ungrouped.Resources {
		if !placed[r.Key] {
			res = append(res, r.Key)
			placed[r.Key] = true
		}
	}
	for _, r := range all {
		if !placed[r.Key] {
			res = append(res, r.Key)
			placed[r.Key] = true
		}
	}
	width := canvasGroupW
	if len(apps)+len(res) > 0 {
		loose := CanvasGroup{Loose: true}
		if len(c.Groups) == 0 {
			// Nothing is grouped: the loose group takes the main column, in
			// the same two-column shape as a system.
			loose.W = canvasGroupW
			loose.Items, loose.H = twoColumns(apps, res)
		} else {
			loose.X = canvasGroupW + canvasSideGap
			loose.W = canvasLooseW
			for i, k := range append(apps, res...) {
				loose.Items = append(loose.Items, CanvasItem{Key: k, X: canvasPad, Y: canvasTop + i*CanvasRow})
			}
			loose.H = groupHeight(len(apps) + len(res))
			width = loose.X + loose.W
		}
		c.Groups = append(c.Groups, loose)
		if loose.H > systemsH {
			systemsH = loose.H
		}
	}
	c.W = width
	c.H = systemsH
	if c.H < canvasMinHeight {
		c.H = canvasMinHeight
	}
	return c
}

// twoColumns lays apps out on the left and resources on the right, row by
// row, and returns the items with the group's height.
func twoColumns(apps, res []string) ([]CanvasItem, int) {
	items := make([]CanvasItem, 0, len(apps)+len(res))
	for i, k := range apps {
		items = append(items, CanvasItem{Key: k, X: canvasPad, Y: canvasTop + i*CanvasRow})
	}
	for i, k := range res {
		items = append(items, CanvasItem{Key: k, X: canvasColGap + canvasPad, Y: canvasTop + i*CanvasRow})
	}
	rows := len(apps)
	if len(res) > rows {
		rows = len(res)
	}
	return items, groupHeight(rows)
}

func groupHeight(rows int) int {
	if rows < 1 {
		rows = 1
	}
	return canvasTop + rows*CanvasRow + canvasBottom
}
