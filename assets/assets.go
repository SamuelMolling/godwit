// Package assets holds the godwit logo files embedded for the UI.
package assets

import _ "embed"

// Mark is the square godwit mark, assets/mark.svg.
//
//go:embed mark.svg
var Mark []byte
