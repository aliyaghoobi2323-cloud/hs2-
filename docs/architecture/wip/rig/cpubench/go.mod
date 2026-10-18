module cpubench

go 1.24

require (
	example.com/smuxb v0.0.0
	example.com/smuxn v0.0.0
	example.com/smuxp v0.0.0
)

replace example.com/smuxb => ./smuxb

replace example.com/smuxn => ./smuxn

replace example.com/smuxp => ./smuxp
