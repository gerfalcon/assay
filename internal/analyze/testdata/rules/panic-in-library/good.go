package good

import "encoding/json"

// The must* prefix is the Go convention announcing a deliberate panic —
// regexp.MustCompile, template.Must. Flagging it flags the standard library's
// own idiom.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func MustParse(s string) int {
	if s == "" {
		panic("empty")
	}
	return 0
}

func returnsAnErrorInstead(s string) (int, error) {
	if s == "" {
		return 0, errEmpty
	}
	return 0, nil
}

var errEmpty = sentinel("empty")

type sentinel string

func (s sentinel) Error() string { return string(s) }
