package rawstore

import "testing"

func TestValidators(t *testing.T) {
	a := Validators{ETag: `"abc"`, LastModified: "Sun, 08 Nov 2026 10:05:20 GMT", Size: 10}
	if !a.Known() || len(a.ID()) != 16 {
		t.Fatalf("Known %v ID %q", a.Known(), a.ID())
	}
	if a.ID() != (Validators{ETag: ` "abc" `, LastModified: a.LastModified, Size: 10}).ID() {
		t.Error("ID depends on surrounding spaces")
	}
	for _, b := range []Validators{
		{ETag: `"abd"`, LastModified: a.LastModified, Size: 10},
		{ETag: a.ETag, LastModified: "Mon, 09 Nov 2026 10:05:20 GMT", Size: 10},
		{ETag: a.ETag, LastModified: a.LastModified, Size: 11},
	} {
		if b.ID() == a.ID() {
			t.Errorf("%+v has the same ID as %+v", b, a)
		}
	}
	if (Validators{Size: 10}).Known() || (Validators{ETag: " "}).Known() {
		t.Error("size alone counts as known")
	}
	if !(Validators{LastModified: a.LastModified}).Known() {
		t.Error("Last-Modified alone is not known")
	}
}
