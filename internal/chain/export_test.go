package chain

// WaitReplay waits until no replay runs, nor the move that follows one.
func (c *Chain) WaitReplay() {
	for {
		c.processMu.Lock()
		r := c.replay
		c.processMu.Unlock()
		if r == nil {
			return
		}
		<-r.done
	}
}
