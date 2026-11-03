package format

import "testing"

func TestBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{in: 0, want: "0 Б"},
		{in: 999, want: "999 Б"},
		{in: 1024, want: "1.0 КиБ"},
		{in: 50 << 30, want: "50.0 ГиБ"},
		{in: 900 << 40, want: "900.0 ТиБ"},
	}

	for _, tc := range cases {
		if got := Bytes(tc.in); got != tc.want {
			t.Errorf("Bytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
