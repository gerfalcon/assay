package good

func earlyReturnNoElse(a int) int {
	if a > 0 {
		return 1
	}
	return 2
}

func elseWithoutTerminator(a int) int {
	n := 0
	if a > 0 {
		n = 1
	} else {
		n = 2
	}
	return n
}

func elseIfChain(a int) int {
	if a > 0 {
		n := 1
		_ = n
	} else if a < 0 {
		n := 2
		_ = n
	}
	return a
}
