module almanack

// 1.25 is the floor. crypto/hkdf entering the standard library in 1.24 is what
// removes one of the two reasons this project would otherwise need x/crypto; the
// extra minor comes from modernc.org/sqlite, which requires it.
go 1.25.0

require (
	golang.org/x/crypto v0.54.0
	modernc.org/sqlite v1.54.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
