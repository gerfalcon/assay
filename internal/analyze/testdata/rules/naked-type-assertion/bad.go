package bad

// Each `want` marks a line that MUST produce a finding.

func plainAssert(x any) string {
	s := x.(string) // want
	return s
}

func assertInCallArg(x any) int {
	return consume(x.(int)) // want
}

func consume(i int) int { return i }

func twoOnSeparateLines(a, b any) {
	p := a.(int)    // want
	q := b.(string) // want
	_, _ = p, q
}

func assertOnStructField(x any) {
	v := x.(struct{ N int }) // want
	_ = v
}
