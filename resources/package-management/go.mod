module github.com/Bpoe/dsc-extras/resources/package-management

go 1.25.0

require github.com/ProtonMail/go-crypto v1.5.2

require (
	github.com/cloudflare/circl v1.6.3 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/term v0.43.0 // indirect
)

require (
	github.com/Bpoe/dsc-extras/resources v0.0.0
	github.com/creack/pty v1.1.24
)

replace github.com/Bpoe/dsc-extras/resources => ..
