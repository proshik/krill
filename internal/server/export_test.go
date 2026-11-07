package server

// LockFirewallForTest takes the lock every firewall-changing handler holds, as
// a lockdown in progress would; the returned func releases it.
func (s *Server) LockFirewallForTest() func() {
	s.fwMu.Lock()
	return s.fwMu.Unlock
}
