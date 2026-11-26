package topology

import "testing"

func itemAt(t *testing.T, g CanvasGroup, key string) CanvasItem {
	t.Helper()
	for _, it := range g.Items {
		if it.Key == key {
			return it
		}
	}
	t.Fatalf("group %q has no item %q: %+v", g.Name, key, g.Items)
	return CanvasItem{}
}

func TestLayoutCanvasSystemsAndLoose(t *testing.T) {
	res := func(key string) ResourceView { return ResourceView{EnvResource: EnvResource{Key: key}} }
	view := EnvView{
		Systems: []SystemView{
			{ID: 1, Name: "shop", Apps: []AppView{{EnvApp: EnvApp{ID: 10}}, {EnvApp: EnvApp{ID: 11}}, {EnvApp: EnvApp{ID: 12}}},
				Resources: []ResourceView{res("ldb:1"), res("inst:5")}},
			// uses ldb:1 too: it stays where the first system placed it
			{ID: 2, Name: "back office", Apps: []AppView{{EnvApp: EnvApp{ID: 13}}}, Resources: []ResourceView{res("ldb:1"), res("ldb:2")}},
		},
		Ungrouped: SystemView{Apps: []AppView{{EnvApp: EnvApp{ID: 14}}}},
	}
	all := []EnvResource{{Key: "ldb:1"}, {Key: "ldb:2"}, {Key: "bucket:7"}}
	c := LayoutCanvas(view, all)

	if len(c.Groups) != 3 {
		t.Fatalf("want 2 systems + loose, got %d groups", len(c.Groups))
	}
	shop, office, loose := c.Groups[0], c.Groups[1], c.Groups[2]
	if shop.Y != 0 || shop.W != 736 || shop.H != 64+3*124+8 {
		t.Fatalf("shop frame: %+v", shop)
	}
	if it := itemAt(t, shop, "app:12"); it.X != 24 || it.Y != 64+2*124 {
		t.Fatalf("third app of shop: %+v", it)
	}
	if it := itemAt(t, shop, "inst:5"); it.X != 424 || it.Y != 64+124 {
		t.Fatalf("second resource of shop: %+v", it)
	}
	if office.Y != shop.H+32 {
		t.Fatalf("back office must stack under shop: %+v", office)
	}
	for _, it := range office.Items {
		if it.Key == "ldb:1" {
			t.Fatal("a shared resource must be placed once, in the first system")
		}
	}
	itemAt(t, office, "ldb:2")
	if !loose.Loose || loose.X != 760 || loose.W != 336 {
		t.Fatalf("loose group beside the systems: %+v", loose)
	}
	if itemAt(t, loose, "app:14").Y != 64 || itemAt(t, loose, "bucket:7").Y != 64+124 {
		t.Fatalf("loose items stack apps first, then unused resources: %+v", loose.Items)
	}
	if c.W != 1096 || c.H != shop.H+32+office.H {
		t.Fatalf("plane size: %dx%d", c.W, c.H)
	}
}

func TestLayoutCanvasWithoutSystems(t *testing.T) {
	view := EnvView{Ungrouped: SystemView{
		Apps:      []AppView{{EnvApp: EnvApp{ID: 1}}},
		Resources: []ResourceView{{EnvResource: EnvResource{Key: "ldb:3"}}},
	}}
	c := LayoutCanvas(view, []EnvResource{{Key: "ldb:3"}})
	if len(c.Groups) != 1 {
		t.Fatalf("want one group, got %d", len(c.Groups))
	}
	g := c.Groups[0]
	if !g.Loose || g.X != 0 || g.W != 736 {
		t.Fatalf("with no systems the loose group takes the main column: %+v", g)
	}
	if it := itemAt(t, g, "ldb:3"); it.X != 424 || it.Y != 64 {
		t.Fatalf("resources keep the right column: %+v", it)
	}
	if c.W != 736 || c.H != 200 {
		t.Fatalf("plane: %dx%d", c.W, c.H)
	}
}

func TestLayoutCanvasEmpty(t *testing.T) {
	c := LayoutCanvas(EnvView{}, nil)
	if len(c.Groups) != 0 || c.H != 200 {
		t.Fatalf("an empty environment has no groups and the minimum height: %+v", c)
	}
}

func TestBuildEnvViewMarksDetectedUses(t *testing.T) {
	v := BuildEnvView(EnvViewInput{
		Apps:      []EnvApp{{ID: 1}},
		Resources: map[string]EnvResource{"ldb:1": {Key: "ldb:1", Kind: "ldb"}, "ldb:2": {Key: "ldb:2", Kind: "ldb"}},
		Links: []EnvLink{
			{AppID: 1, ResKey: "ldb:1", VarName: "A"},
			{AppID: 1, ResKey: "ldb:2", VarName: "B", Detected: true},
			// a modeled link wins over a detected one to the same resource
			{AppID: 1, ResKey: "ldb:1", VarName: "C", Detected: true},
		},
	})
	a := v.Ungrouped.Apps[0]
	if len(a.Uses) != 2 || len(a.Detected) != 1 || a.Detected[0] != "ldb:2" {
		t.Fatalf("uses %v detected %v", a.Uses, a.Detected)
	}
}
