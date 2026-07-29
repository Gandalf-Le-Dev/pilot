package proto

import "flag"

// updateGolden rewrites the recorded schema instead of comparing against it.
//
// It deliberately refreshes only the record, never SchemaDigest. Updating the
// digest is the decision this whole mechanism exists to force; a flag that
// made it for you would defeat the point.
var updateGolden = false

func init() {
	flag.BoolVar(&updateGolden, "update", false, "rewrite the recorded configuration schema")
}
