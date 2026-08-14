package dashboard

import _ "embed"

// The page is self-contained: styles, htmx, and the chart renderer are all
// compiled into the binary, so `pilot dashboard` works on a machine with no
// network beyond its SSH to the fleet.
var (
	//go:embed assets/css/output.css
	outputCSS []byte

	//go:embed assets/js/htmx.min.js
	htmxJS []byte

	//go:embed components/chart/chart.js
	chartJS []byte
)
