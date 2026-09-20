package good

// Nothing here may produce a finding. These are the forms that make a type
// assertion safe, plus the shapes that have tripped naive implementations.

func commaOk(x any) string {
	s, ok := x.(string)
	if !ok {
		return ""
	}
	return s
}

func typeSwitchBare(x any) string {
	switch x.(type) {
	case string:
		return "s"
	}
	return ""
}

func typeSwitchBinding(x any) string {
	switch v := x.(type) {
	case string:
		return v
	case int:
		_ = v
	}
	return ""
}

func commaOkInIf(x any) bool {
	if _, ok := x.(error); ok {
		return true
	}
	return false
}

func excused(x any) string {
	s := x.(string) //nolint:forcetypeassert // validated by the caller
	return s
}

func judgedAbove(x any) string {
	// quality:false-positive the caller guarantees the type by construction
	return x.(string)
}

func judgedInline(x any) string {
	return x.(string) // quality:accepted until=2027-01-01 legacy path, see the migration ticket
}

// A judgement on the doc comment must NOT excuse the body: that is how a
// blanket exemption hides in a docstring. The scope is the comment's own line
// and the one below it, nothing more.
// quality:false-positive this annotation is out of scope on purpose
func docCommentDoesNotExcuseTheBody(x any) string {
	_ = 1
	return x.(string) // want-unjudged
}
