import re
p='/opt/hs2lab/bh95/labsrc/lab/probe/'
s=open(p+'streams.go').read()
s=s.replace('''	Max   float64 `json:"max_ms"`
}''','''	Max   float64 `json:"max_ms"`
	FailT []float64 `json:"fail_t,omitempty"` // bh95: unix time of each failure
	Churn int       `json:"churn,omitempty"`  // bh95: clean planned reconnects
}

// crowdChurn (bh95 -crowdchurn): mean planned lifetime of a crowd connection (0 = forever)
var crowdChurn time.Duration''',1)
s=s.replace('''			fail := func() {
				mu.Lock()
				res.Fail++
				mu.Unlock()
			}''','''			fail := func() {
				mu.Lock()
				res.Fail++
				res.FailT = append(res.FailT, float64(time.Now().UnixNano())/1e9)
				mu.Unlock()
			}''',1)
s=s.replace('''				ok := true
				if _, err := c.Write([]byte{'R'}); err != nil {''','''				ok := true
				churned := false
				var life time.Time
				if crowdChurn > 0 {
					life = time.Now().Add(time.Duration(float64(crowdChurn) * (0.5 + rand.Float64())))
				}
				if _, err := c.Write([]byte{'R'}); err != nil {''',1)
s=s.replace('''				for ok {
					if !wait(time.Duration(float64(every) * (0.5 + rand.Float64()))) {
						break
					}''','''				for ok {
					if !wait(time.Duration(float64(every) * (0.5 + rand.Float64()))) {
						break
					}
					if !life.IsZero() && time.Now().After(life) {
						churned = true
						break
					}''',1)
s=s.replace('''				default:
				}
				fail()
				if !wait(time.Second) {''','''				default:
				}
				if churned {
					mu.Lock()
					res.Churn++
					mu.Unlock()
					continue
				}
				fail()
				if !wait(time.Second) {''',1)
assert s.count('churned')==4 and 'FailT = append' in s, s.count('churned')
open(p+'streams.go','w').write(s)
m=open(p+'main.go').read()
m=m.replace('''	crowdMax := flag.Int(''','''	flag.DurationVar(&crowdChurn, "crowdchurn", 0, "bh95: crowd connection mean lifetime (0.5x-1.5x), then a clean reconnect")
	crowdMax := flag.Int(''',1)
open(p+'main.go','w').write(m)
r=open('/opt/hs2lab/bh95/labsrc/lab/run86b.sh').read()
old='''     ip netns exec $IR iptables -A INPUT $src -d 192.168.5$k.1 -m statistic --mode random --probability $pr -j DROP
     echo "$(date +%T.%N) pair ${src:+192.168.49.2}${src:-ALL}>192.168.5$k.1 drop $pr" >> "${LOGDIR:-$W}/ev.txt"'''
new='''     op=-A; case $pr in *d) pr=${pr%d}; op=-D;; esac  # bh95: PROBd = remove that rule (episode end)
     ip netns exec $IR iptables $op INPUT $src -d 192.168.5$k.1 -m statistic --mode random --probability $pr -j DROP
     if [ $op = -A ]; then echo "$(date +%T.%N) pair ${src:+192.168.49.2}${src:-ALL}>192.168.5$k.1 drop $pr" >> "${LOGDIR:-$W}/ev.txt"
     else echo "$(date +%T.%N) pair ${src:+192.168.49.2}${src:-ALL}>192.168.5$k.1 undrop $pr" >> "${LOGDIR:-$W}/ev.txt"; fi'''
assert old in r
r=r.replace(old,new)
r=r.replace('''[ "$CROWD" != 0 ] && PX="$PX -crowd $CROWD -crowdevery $CROWD_EVERY"''','''[ "$CROWD" != 0 ] && PX="$PX -crowd $CROWD -crowdevery $CROWD_EVERY ${CROWD_CHURN:+-crowdchurn $CROWD_CHURN}"''')
open('/opt/hs2lab/bh95/labsrc/lab/run86b.sh','w').write(r)
print("ok")
