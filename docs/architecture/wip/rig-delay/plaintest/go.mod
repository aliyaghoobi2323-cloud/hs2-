module plaintest

go 1.24

require (
	example.com/oldsmux v0.0.0
	example.com/upsmux v0.0.0
)

replace example.com/oldsmux => /tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z7/delay/compat/old

replace example.com/upsmux => /tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z7/delay/compat/up
