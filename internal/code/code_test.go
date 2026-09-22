package code

import "testing"

func TestCodeIsCanonicalAndSeparated(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse(a.String())
	if err != nil || b.Room() != a.Room() {
		t.Fatal("round trip failed", err)
	}
	if len(a.String()) != 43 || len(a.Room()) != 32 {
		t.Fatal("unexpected code format")
	}
	if a.Token("claim") == a.Token("status") || a.Token("claim") == a.String() {
		t.Fatal("derived values are not separated")
	}
	other, _ := New()
	if other.String() == a.String() || other.Room() == a.Room() {
		t.Fatal("random code reused")
	}
	for _, bad := range []string{"", a.String() + "=", a.String() + "!", "amber-river"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
