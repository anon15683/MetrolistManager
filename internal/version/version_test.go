package version

import "testing"

func TestCompare(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  int
	}{
		{left: "14.0.1", right: "14.0.1", want: 0},
		{left: "14.0.2", right: "14.0.1", want: 1},
		{left: "13.9.9", right: "14.0.0", want: -1},
		{left: "14.0.0-a5", right: "14.0.0-a4", want: 1},
		{left: "14.0.0-a5", right: "14.0.0", want: -1},
		{left: "14.0.0", right: "14.0.0-a5", want: 1},
		{left: "18446744073709551615.0.0", right: "14.0.0", want: 1},
	}
	for _, test := range tests {
		got, err := Compare(test.left, test.right)
		if err != nil || got != test.want {
			t.Fatalf("Compare(%q, %q) = %d, %v; want %d", test.left, test.right, got, err, test.want)
		}
	}
}

func TestValidateRejectsUnsupportedVersions(t *testing.T) {
	for _, value := range []string{"", "14", "14.0", "14.0.0-beta1", "14.00.0", "14.0.0-a01", "18446744073709551616.0.0"} {
		if err := Validate(value); err == nil {
			t.Errorf("Validate(%q) error = nil", value)
		}
	}
}
