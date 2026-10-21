module github.com/xjetry/heron

go 1.27.3

tool (
	connectrpc.com/connect/cmd/protoc-gen-connect-go
	golang.org/x/vuln/cmd/govulncheck
	google.golang.org/protobuf/cmd/protoc-gen-go
	honnef.co/go/tools/cmd/staticcheck
)

require (
	connectrpc.com/connect v1.21.0
	github.com/ebitengine/purego v0.11.1
	github.com/fxamacker/cbor/v2 v2.9.2
	github.com/go-webauthn/webauthn v0.17.4
	github.com/oschwald/maxminddb-golang/v2 v2.6.0
	golang.org/x/crypto v0.57.0
	golang.org/x/net v0.60.0
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
	google.golang.org/protobuf v1.36.12
	modernc.org/sqlite v1.59.0
)

require (
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/go-webauthn/x v0.2.6 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/telemetry v0.0.0-20260908163034-4bcc4b2ee518 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/tools v0.50.0 // indirect
	golang.org/x/vuln v1.8.0 // indirect
	honnef.co/go/tools v0.8.1 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
