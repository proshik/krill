package i18n

import "testing"

// TestCatalogsInSync guarantees en and ru have exactly the same key set, so a
// new UI string can never be added to one catalog and forgotten in the other.
func TestCatalogsInSync(t *testing.T) {
	for k := range en {
		if _, ok := ru[k]; !ok {
			t.Errorf("key %q present in en, missing in ru", k)
		}
	}
	for k := range ru {
		if _, ok := en[k]; !ok {
			t.Errorf("key %q present in ru, missing in en", k)
		}
	}
}
