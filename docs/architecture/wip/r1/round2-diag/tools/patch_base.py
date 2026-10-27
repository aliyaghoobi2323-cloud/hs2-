"""Lab-only trace patch for the base binary (b780b63, the production code):
the same Q/R/W trace lines as r1-diag (HS2_LAB_TRACE), nothing else. Usage:
patch_base.py HS2_SRC SMUX_LABTRACE_A ENGINE_LABTRACE_A"""
import sys, os, re
src, smux_a, eng_a = sys.argv[1:4]

def sub(path, old, new, count=1):
    s = open(path).read()
    n = s.count(old)
    if n != count:
        sys.exit("%s: %d matches for %r" % (path, n, old[:70]))
    s = s.replace(old, new)
    open(path, "w").write(s)

def addimport(path, after, imp):
    s = open(path).read()
    head = s.split(")")[0]
    if '\t"%s"\n' % imp in head:
        return
    sub(path, '\t"%s"\n' % after, '\t"%s"\n\t"%s"\n' % (after, imp))

# smux: the A file without the stream stats (base streams have no txBytes)
s = open(smux_a).read()
s = s[:s.index("var labWin sync.Map")]
s = s.replace('\t"sync"\n\t"sync/atomic"\n', "")
s = s.replace("and the bulk streams' window waits every\n// labStatEvery.", "(base: no S lines).")
s = s.replace("\tlabStatEvery = 250 * time.Millisecond\n", "")
open(os.path.join(src, "third_party/smux/labtrace.go"), "w").write(s)

os.chdir(os.path.join(src, "third_party/smux"))
sub("session.go", """	// the frame's data as sendLoop reads it (nil: the frame has none)
	data *frameData
}""", """	// the frame's data as sendLoop reads it (nil: the frame has none)
	data *frameData
	// lab only (labtrace.go): when a traced frame was queued, wall ns
	labAt int64
}""")
sub("session.go", """					if written, err := io.ReadFull(s.conn, newbuf); err == nil {
						s.rcvd.Add(uint64(written))""", """					if written, err := io.ReadFull(s.conn, newbuf); err == nil {
						if labSmall(cmdPSH, written) {
							LabTrace(fmt.Sprintf("R %p %d %d %s %d\\n", s, sid, written, LabKey(newbuf[:written]), labWall()))
						}
						s.rcvd.Add(uint64(written))""")
sub("session.go", """	if len(f.data) > 0 {
		req.data = &frameData{b: f.data}
	}
	select {
	case s.shaper <- req:""", """	if len(f.data) > 0 {
		req.data = &frameData{b: f.data}
	}
	if labSmall(f.cmd, len(f.data)) {
		req.labAt = labWall()
	}
	select {
	case s.shaper <- req:""")
sub("session.go", """		case request := <-s.writes:
			buf[0] = request.frame.ver""", """		case request := <-s.writes:
			if request.labAt != 0 {
				LabTrace(fmt.Sprintf("Q %p %d %d %s %d %d -\\n", s, request.frame.sid, len(request.frame.data), LabKey(request.frame.data), request.labAt, labWall()))
			}
			buf[0] = request.frame.ver""")
addimport("session.go", "errors", "fmt")

# engine: the trace and the shaper's W lines only
e = open(eng_a).read()
e = e[:e.index("// labPinAvoid")]
e = re.sub(r"(?s)// labSock applies.*?\n}\n\n", "", e)
e = e.replace("""	labPaceBps uint64
	labWclamp  int
	labPinPort int
	labNewB    = uint64(classNewBytes)
""", "")
e = re.sub(r"(?s)\tif v, err := strconv.ParseFloat.*?labNewB = uint64\(v\)\n\t}\n", "", e)
e = e.replace('H %d %d pace=%d wclamp=%d pin=%d newbytes=%d args=%q\\n", os.Getpid(), time.Now().UnixNano(), labPaceBps, labWclamp, labPinPort, labNewB, os.Args',
              'H %d %d base args=%q\\n", os.Getpid(), time.Now().UnixNano(), os.Args')
e = e.replace('\t"strconv"\n', "")
open(os.path.join(src, "engine/labtrace.go"), "w").write(e)

os.chdir(os.path.join(src, "engine"))
sub("shape.go", """	wmu  sync.Mutex
	wbuf []byte // reused frame-build scratch (write side)
""", """	wmu  sync.Mutex
	wbuf []byte // reused frame-build scratch (write side)

	// lab only (labtrace.go, HS2_LAB_TRACE)
	labTCP  *net.TCPConn
	labIn   uint64
	labOut  uint64
	labPend []labPend
""")
sub("shape.go", """	c.wmu.Lock()
	defer c.wmu.Unlock()
	written := 0""", """	c.wmu.Lock()
	defer c.wmu.Unlock()
	if labTraceOn {
		c.labNote(p)
	}
	written := 0""")
sub("shape.go", """		if _, err := c.Conn.Write(f); err != nil {
			return written, err
		}
		written += len(d)""", """		if _, err := c.Conn.Write(f); err != nil {
			return written, err
		}
		if labTraceOn {
			c.labRecord(len(d))
		}
		written += len(d)""")
sub("stream.go", """	var c net.Conn = newShapedConn(gate, sampler)""", """	sh := newShapedConn(gate, sampler)
	sh.labTCP = gate.tcp // lab only (labtrace.go)
	var c net.Conn = sh""")
print("base patched")
