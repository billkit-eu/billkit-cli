package cli

import "testing"

func TestCoerce(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"true", true},
		{"false", false},
		{"null", nil},
		{"42", int64(42)},
		{"-7", int64(-7)},
		{"0", int64(0)},
		{"12abc", "12abc"}, // partial int stays a string
		{"3.14", "3.14"},   // floats stay strings (BillKit deals in integer cents)
		{"hello", "hello"},
		{"", ""},
	}
	for _, c := range cases {
		if got := coerce(c.in); got != c.want {
			t.Errorf("coerce(%q) = %v (%T), want %v (%T)", c.in, got, got, c.want, c.want)
		}
	}
}
