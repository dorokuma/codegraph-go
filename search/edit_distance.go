package search

// BoundedEditDistance computes the Damerau-Levenshtein edit distance between
// a and b, returning maxDist+1 as soon as the distance is known to exceed maxDist.
// This early-exit makes the fuzzy fallback cheap even over tens of thousands of names.
//
// Pure DP, O(min(len(a), len(b))) memory. Compares case-folded inputs;
// callers should pass lowercase(a), lowercase(b).
//
// Ported from official search/query-parser.ts.
func BoundedEditDistance(a, b string, maxDist int) int {
	if a == b {
		return 0
	}
	al := len(a)
	bl := len(b)
	if abs(al-bl) > maxDist {
		return maxDist + 1
	}
	if al == 0 {
		return bl
	}
	if bl == 0 {
		return al
	}

	prev := make([]int, bl+1)
	cur := make([]int, bl+1)
	for j := 0; j <= bl; j++ {
		prev[j] = j
	}

	for i := 1; i <= al; i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= bl; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			insertion := cur[j-1] + 1
			deletion := prev[j] + 1
			substitution := prev[j-1] + cost
			cur[j] = min3(insertion, deletion, substitution)
			if cur[j] < rowMin {
				rowMin = cur[j]
			}
		}
		if rowMin > maxDist {
			return maxDist + 1
		}
		prev, cur = cur, prev
	}
	return prev[bl]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
