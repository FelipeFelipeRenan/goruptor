package disruptor

// Side define se é uma ordem de compra ou venda
// Usamos uint8 porque só precisamos de 0 ou 1
type Side uint8

const (
	Buy  Side = 0
	Sell Side = 1
)

type OrderEvent struct {
	OrderID   uint64
	AccountID uint64
	Price     uint64
	Quantity  uint64
	AssetID   uint32
	Side      Side
}
