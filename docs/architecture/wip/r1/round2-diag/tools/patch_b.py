"""Lab-only trace patch for angle B (b7f117f): R (smux reader), W (shaper:
the record that carried a small frame, the socket's TCP_INFO), T (a TUN ICMP
packet into a link's queue) lines, HS2_LAB_TRACE; nothing else. Usage:
patch_b.py HS2_SRC BASE_SMUX_LABTRACE BASE_ENGINE_LABTRACE A_ENGINE_LABTRACE"""
import sys, os, re
src, smux_base, eng_base, eng_a = sys.argv[1:5]

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

open(os.path.join(src, "third_party/smux/labtrace.go"), "w").write(open(smux_base).read())
e = open(eng_base).read()
a = open(eng_a).read()
e += "\n" + a[a.index("// labTun:"):]
open(os.path.join(src, "engine/labtrace.go"), "w").write(e)

os.chdir(os.path.join(src, "third_party/smux"))
sub("session.go", """					if written, err := io.ReadFull(s.conn, newbuf); err == nil {
						s.rcvd.Add(uint64(written))""", """					if written, err := io.ReadFull(s.conn, newbuf); err == nil {
						if labSmall(cmdPSH, written) {
							LabTrace(fmt.Sprintf("R %p %d %d %s %d\\n", s, sid, written, LabKey(newbuf[:written]), labWall()))
						}
						s.rcvd.Add(uint64(written))""")
addimport("session.go", "errors", "fmt")

os.chdir(os.path.join(src, "engine"))
sub("shape.go", """	pend  []byte
	pendT int
""", """	pend  []byte
	pendT int

	// lab only (labtrace.go, HS2_LAB_TRACE)
	labTCP  *net.TCPConn
	labIn   uint64
	labOut  uint64
	labPend []labPend
""")
sub("shape.go", """func (c *shapedConn) writeHold(p []byte, hold bool) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()""", """func (c *shapedConn) writeHold(p []byte, hold bool) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if labTraceOn {
		c.labNote(p)
	}""")
sub("shape.go", """	clear(f[shapeHdrLen+len(d):])
	_, err := c.Conn.Write(f)
	return err""", """	clear(f[shapeHdrLen+len(d):])
	_, err := c.Conn.Write(f)
	if labTraceOn && err == nil {
		c.labRecord(len(d))
	}
	return err""")
sub("stream.go", """	var c net.Conn = newShapedConn(low, sampler)""", """	sh := newShapedConn(low, sampler)
	sh.labTCP = tcp // lab only (labtrace.go)
	var c net.Conn = sh""")
sub("l3_link.go", """func (l *l3Link) enqueue(b *[]byte, tag, flow uint32) bool {
	if l.ls != nil {""", """func (l *l3Link) enqueue(b *[]byte, tag, flow uint32) bool {
	if labTraceOn && len(*b) > 20 && (*b)[0]>>4 == 4 && (*b)[9] == 1 {
		labTun(l, *b)
	}
	if l.ls != nil {""")
print("B patched")
