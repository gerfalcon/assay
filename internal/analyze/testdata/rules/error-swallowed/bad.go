package bad

import "errors"

func discardToBlank() {
	err := errors.New("x")
	_ = err // want
}

func emptyErrorBranch() {
	var err error
	if err != nil { // want
	}
}

func returnsNilOnError() error {
	var err error
	if err != nil { // want
		return nil
	}
	return nil
}
