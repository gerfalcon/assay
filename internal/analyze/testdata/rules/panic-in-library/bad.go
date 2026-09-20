package bad

func parseOrDie(s string) int {
	if s == "" {
		panic("empty") // want
	}
	return 0
}
