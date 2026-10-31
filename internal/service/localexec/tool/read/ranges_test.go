package read

import "testing"

func TestLineRange(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	u32 := func(v uint32) *uint32 { return &v }
	for _, tc := range []struct {
		offset     *int32
		limit      *uint32
		start, end int
	}{
		{nil, nil, 0, 5},
		{i32(2), u32(2), 1, 3},
		{i32(-2), nil, 3, 5},
		{i32(-10), u32(1), 0, 1},
		{i32(9), nil, 0, 5},
	} {
		if start, end := lineRange(5, tc.offset, tc.limit); start != tc.start || end != tc.end {
			t.Errorf("lineRange(5, %v, %v) = %d, %d; want %d, %d", tc.offset, tc.limit, start, end, tc.start, tc.end)
		}
	}
}
