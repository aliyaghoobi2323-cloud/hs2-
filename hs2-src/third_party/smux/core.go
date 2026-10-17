package smux

// fitCore holds the window adaptWindow just set, from old, to
// NeedStreamBuffer, and sets it (setWindow). The caller holds bufferLock.
func (s *Stream) fitCore(cfg *Config, old uint32) {
	w := s.rcvWin
	if need := needOf(cfg); w > need {
		w = need
	}
	s.rcvWin = old
	s.setWindow(cfg, w)
}
