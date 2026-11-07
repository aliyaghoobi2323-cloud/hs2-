package engine

import "io"

// The contract between the multi-link connection (mConn, mconn.go) and the
// tunnel that carries it (mconn_link.go). See mconn_link.go for HS2_MCONN.

// mPlane is the kind of link a sub-stream is opened on.
type mPlane int

const (
	planeLight mPlane = iota // a link carrying no heavy stream (interactive traffic)
	planeBulk                // a link that already carries heavy streams (downloads)
)

// mSub is one sub-stream (a smux stream with its kindMConn header already
// written on the edge, already read on the exit).
type mSub interface{ io.ReadWriteCloser }

// mOpener opens a new sub on a link of the given plane, not on any of the
// links in avoid (opaque link keys from earlier calls). Edge (Iran) side only.
type mOpener func(p mPlane, avoid []any) (sub mSub, linkKey any, err error)
