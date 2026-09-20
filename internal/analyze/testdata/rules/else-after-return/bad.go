package bad

func elseAfterReturn(a int) int {
	if a > 0 {
		return 1
	} else { // want
		return 2
	}
}

func elseAfterContinue(xs []int) {
	for _, x := range xs {
		if x > 0 {
			continue
		} else { // want
			_ = x
		}
	}
}
