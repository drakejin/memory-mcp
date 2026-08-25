package knowledge

import "testing"

func TestNormalizeStrings(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil becomes empty non-nil", nil, []string{}},
		{"trims", []string{" a ", "b"}, []string{"a", "b"}},
		{"drops blanks", []string{"", "   ", "a"}, []string{"a"}},
		{"keeps order", []string{"z", "a"}, []string{"z", "a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeStrings(tc.in)
			if got == nil {
				t.Fatal("normalizeStrings must never return nil (hot JSON stores [] not null)")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("normalizeStrings(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("normalizeStrings(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}
