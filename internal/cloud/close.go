package cloud

// Close encerra o fluxo de trades: os workers de Publish enviam o que já está
// na fila e retornam.
//
// ATENÇÃO: só chame depois que o motor parou. O motor envia em TradeCh, e enviar
// em um canal fechado causa panic. Chamar duas vezes também causa panic.
func (p *AWSPublisher) Close() { close(p.TradeCh) }
