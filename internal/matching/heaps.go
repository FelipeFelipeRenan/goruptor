package matching

type MinPriceHeap []uint64

func (h MinPriceHeap) Len() int           { return len(h) }
func (h MinPriceHeap) Less(i, j int) bool { return h[i] < h[j] } // Menor sobe (heap de minima)
func (h MinPriceHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *MinPriceHeap) Push(x interface{}) {
	*h = append(*h, x.(uint64))
}

func (h *MinPriceHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

type MaxPriceHeap []uint64

func (h MaxPriceHeap) Len() int           { return len(h) }
func (h MaxPriceHeap) Less(i, j int) bool { return h[i] > h[j] }
func (h MaxPriceHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *MaxPriceHeap) Push(x interface{}) {
	*h = append(*h, x.(uint64))
}

func (h *MaxPriceHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}
