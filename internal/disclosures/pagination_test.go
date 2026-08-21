package disclosures

import (
	"strings"
	"testing"
)

func TestDisclosurePaginationAdvance(t *testing.T) {
	tests := []struct {
		name     string
		pages    [][3]int // start, raw row count, total
		wantDone bool
		wantErr  string
	}{
		{name: "empty result set", pages: [][3]int{{0, 0, 0}}, wantDone: true},
		{name: "terminal partial page", pages: [][3]int{{0, 2, 3}, {2, 1, 3}}, wantDone: true},
		{name: "terminal empty page", pages: [][3]int{{0, 2, 2}, {2, 0, 2}}, wantDone: true},
		{name: "changed total", pages: [][3]int{{0, 1, 2}, {1, 0, 1}}, wantErr: "recordsTotal changed"},
		{name: "nonzero first start", pages: [][3]int{{1, 1, 2}}, wantErr: "expected 0"},
		{name: "noncontiguous start", pages: [][3]int{{0, 1, 3}, {2, 1, 3}}, wantErr: "expected 1"},
		{name: "overrun", pages: [][3]int{{0, 2, 2}, {2, 1, 2}}, wantErr: "beyond recordsTotal"},
		{name: "premature empty", pages: [][3]int{{0, 0, 1}}, wantErr: "empty page before"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var pagination disclosurePagination
			var done bool
			for _, page := range test.pages {
				var err error
				done, err = pagination.advance("test source", "recordsTotal", page[0], page[1], page[2])
				if err != nil {
					if test.wantErr == "" || !strings.Contains(err.Error(), test.wantErr) {
						t.Fatalf("advance(%v) error = %v, want %q", page, err, test.wantErr)
					}
					return
				}
			}
			if test.wantErr != "" {
				t.Fatalf("advance succeeded, want error containing %q", test.wantErr)
			}
			if done != test.wantDone {
				t.Fatalf("done = %t, want %t", done, test.wantDone)
			}
		})
	}
}
