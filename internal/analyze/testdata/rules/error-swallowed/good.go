package good

import (
	"errors"
	"fmt"
)

func returnsTheError() error {
	var err error
	if err != nil {
		return err
	}
	return nil
}

func wrapsTheError() error {
	var err error
	if err != nil {
		return fmt.Errorf("doing the thing: %w", err)
	}
	return nil
}

func logsAndContinues(log func(...any)) {
	var err error
	if err != nil {
		log("could not do the thing:", err)
	}
}

func blankOnANonError() {
	x := 42
	_ = x
}

func sentinelCheck() bool {
	var err error
	return errors.Is(err, errUnknown)
}

var errUnknown = errors.New("unknown")
