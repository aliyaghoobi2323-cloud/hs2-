module github.com/xtaci/smux

// Upstream v1.5.24 says go 1.21. Raised to 1.24, the floor the other fork
// (third_party/utls) already has: with 1.21 these packages alone were compiled
// with pre-1.22 loop-variable semantics while the rest of the binary (main
// module go 1.27) has per-iteration ones. No loop here depended on the old
// semantics (alloc.go already copies k), so the generated code is unchanged.
go 1.24
