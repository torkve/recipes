module recipes

go 1.26

require (
	github.com/google/uuid v1.6.0
	github.com/gorilla/sessions v1.2.1
	github.com/microcosm-cc/bluemonday v1.0.27
	golang.org/x/crypto v0.52.0
	google.golang.org/protobuf v1.36.11
	modernc.org/sqlite v1.46.1
)

require (
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/gorilla/securecookie v1.1.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20260218203240-3dfff04db8fa // indirect
	golang.org/x/sys v0.46.0 // indirect
	modernc.org/libc v1.68.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// golang.org/x/net is pinned to the version present in the offline module cache.
// x/crypto declares a newer minimum (v0.54.0) we cannot fetch here, but the only
// x/net package actually compiled is x/net/html (via bluemonday), and v0.51.0 is
// API-compatible for it.
replace golang.org/x/net => golang.org/x/net v0.51.0

require (
	github.com/torkve/icloud-notes v1.0.0
	golang.org/x/net v0.54.0 // indirect
)
