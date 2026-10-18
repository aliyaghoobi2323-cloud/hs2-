module compat

go 1.24

require (
	example.com/oldsmux v0.0.0
	example.com/upsmux v0.0.0
	github.com/xtaci/smux v1.5.24
)

replace example.com/oldsmux => ./old

replace example.com/upsmux => ./up

replace github.com/xtaci/smux => ../tree/hs2-src/third_party/smux
