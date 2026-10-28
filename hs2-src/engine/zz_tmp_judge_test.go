package engine

import "testing"

func TestZZJudgeTunShare(t *testing.T) { RTShare.Set(8192); defer RTShare.Set(0); oneSessionTunPing(t) }
func TestZZJudgeTunShare4(t *testing.T) { RTShare.Set(4096); defer RTShare.Set(0); oneSessionTunPing(t) }
func TestZZJudgeTunRR(t *testing.T)    { LatRR.Set(1); defer LatRR.Set(0); oneSessionTunPing(t) }
func TestZZJudgeTunHold(t *testing.T)  { RTHold.Set(20); defer RTHold.Set(0); oneSessionTunPing(t) }
