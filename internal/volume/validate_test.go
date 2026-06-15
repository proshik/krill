package volume

import "testing"

func TestValidateAppVolume(t *testing.T) {
	valid := []struct{ name, path string }{
		{"data", "/data"},
		{"uploads", "/var/lib/app/uploads"},
		{"x", "/srv"},
		{"a1-b2", "/opt/app-data"},
	}
	for _, c := range valid {
		if err := ValidateAppVolume(c.name, c.path); err != nil {
			t.Errorf("ValidateAppVolume(%q,%q) = %v, want nil", c.name, c.path, err)
		}
	}

	invalid := []struct{ desc, name, path string }{
		{"empty name", "", "/data"},
		{"uppercase name", "Data", "/data"},
		{"name too long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "/data"}, // 33 chars
		{"name with slash", "a/b", "/data"},
		{"name with dot", "a.b", "/data"},
		{"relative path", "data", "data"},
		{"traversal", "data", "/data/../etc"},
		{"trailing slash", "data", "/data/"},
		{"double slash", "data", "/data//x"},
		{"space in path", "data", "/data dir"},
		{"backtick in path", "data", "/da`ta"},
		{"leading dash segment", "data", "/-data"},
		{"root", "data", "/"},
		{"etc", "data", "/etc"},
		{"proc", "data", "/proc"},
		{"proc subdir", "data", "/proc/sys"},
		{"sys", "data", "/sys"},
		{"dev", "data", "/dev"},
		{"var run", "data", "/var/run"},
		{"root home", "data", "/root"},
		{"boot", "data", "/boot"},
	}
	for _, c := range invalid {
		if err := ValidateAppVolume(c.name, c.path); err == nil {
			t.Errorf("ValidateAppVolume(%q,%q) [%s] = nil, want error", c.name, c.path, c.desc)
		}
	}
}
