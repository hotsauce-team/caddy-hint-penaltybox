package penaltybox

import "testing"

func TestStrictLevelParse(t *testing.T) {
	cases := []struct {
		name string
		vals []string
		want int
	}{
		{"absent", nil, 1},
		{"empty value", []string{""}, 1},
		{"one", []string{"1"}, 1},
		{"two", []string{"2"}, 2},
		{"three", []string{"3"}, 3},
		{"zero", []string{"0"}, 1},
		{"four", []string{"4"}, 1},
		{"ten", []string{"10"}, 1},
		{"garbage", []string{"abc"}, 1},
		{"leading space", []string{" 2"}, 1},
		{"trailing space", []string{"2 "}, 1},
		{"zero-padded", []string{"03"}, 1},
		{"negative", []string{"-1"}, 1},
		{"multi-value", []string{"3", "3"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseLevel(c.vals); got != c.want {
				t.Errorf("parseLevel(%q) = %d, want %d", c.vals, got, c.want)
			}
		})
	}
}
