package bad

func ExportedAnyParam(x any) {} // want

func ExportedEmptyInterface(x interface{}) {} // want

func ExportedAnyResult() any { return nil } // want

func ExportedAnySlice(xs []any) {} // want
