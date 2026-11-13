package graph

// SetBounds lowers the memory bounds of g so tests can trip them.
func (g *Graph) SetBounds(maxVisited, maxMembers int) {
	g.maxVisited, g.maxMembers = maxVisited, maxMembers
}
