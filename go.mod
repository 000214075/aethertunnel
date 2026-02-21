module github.com/aethertunnel/aethertunnel

go 1.22.2

require (
	github.com/BurntSushi/toml v1.3.2
	github.com/gorilla/websocket v1.5.3
	github.com/libp2p/go-sctp v0.0.0-00010101000000-000000000000
	golang.org/x/crypto v0.17.0
)

require golang.org/x/sys v0.15.0 // indirect

replace github.com/libp2p/go-sctp => ./sctp-fake
