package dashboard

// The dashboard's generated artifacts are committed, so `go build ./...`
// stays the whole build: contributors and CI need the templ and tailwind
// CLIs only when they touch the UI.
//
//   templ:    templ generate ./internal/dashboard/...  (go install github.com/a-h/templ/cmd/templ@latest)
//   tailwind: tailwindcss -i internal/dashboard/assets/css/globals.css \
//                         -o internal/dashboard/assets/css/output.css
//
// CI checks templ freshness (`templ generate -check`); the CSS is regenerated
// by hand when styles or views change, which the check below cannot see —
// if a class is missing in the page, this is why.
//
//go:generate templ generate
//go:generate tailwindcss -i assets/css/globals.css -o assets/css/output.css
