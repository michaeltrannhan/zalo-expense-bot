package domain

// State machines. The database mirrors these with CHECK constraints; domain
// validation is the first line of defence, the constraint is the last.
//
// Receipt (plan §9.1):
//
//	received → queued → downloading → stored → extracting → review_required → confirmed → deleted
//	downloading/extracting → failed_transient | failed_permanent
//	failed_transient → queued
var receiptTransitions = map[ReceiptStatus][]ReceiptStatus{
	ReceiptReceived:    {ReceiptQueued, ReceiptDeleted, ReceiptFailedPermanent},
	ReceiptQueued:      {ReceiptDownloading, ReceiptDeleted},
	ReceiptDownloading: {ReceiptStored, ReceiptFailedTransient, ReceiptFailedPermanent},
	// stored is an in-flight processing state like downloading/extracting:
	// object-store and bookkeeping failures after the transition must be
	// representable, or a receipt strands in 'stored' forever.
	ReceiptStored:          {ReceiptExtracting, ReceiptDeleted, ReceiptFailedTransient, ReceiptFailedPermanent},
	ReceiptExtracting:      {ReceiptReviewRequired, ReceiptFailedTransient, ReceiptFailedPermanent},
	ReceiptReviewRequired:  {ReceiptConfirmed, ReceiptDeleted},
	ReceiptConfirmed:       {ReceiptDeleted},
	ReceiptFailedTransient: {ReceiptQueued, ReceiptFailedPermanent, ReceiptDeleted},
	ReceiptFailedPermanent: {ReceiptDeleted},
	ReceiptDeleted:         {},
}

// CanTransition reports whether a receipt may move from s to next.
func (s ReceiptStatus) CanTransition(next ReceiptStatus) bool {
	for _, allowed := range receiptTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Transaction (plan §9.2):
//
//	draft → awaiting_confirmation → confirmed → amended → deleted
//
// confirmed → deleted and amended → deleted are the user-facing delete paths.
// awaiting_confirmation → deleted is the "discard suggestion" path.
var txTransitions = map[TxStatus][]TxStatus{
	TxDraft:                {TxAwaitingConfirmation, TxDeleted},
	TxAwaitingConfirmation: {TxConfirmed, TxDeleted},
	TxConfirmed:            {TxAmended, TxDeleted},
	TxAmended:              {TxDeleted},
	TxDeleted:              {},
}

// CanTransition reports whether a transaction may move from s to next.
func (s TxStatus) CanTransition(next TxStatus) bool {
	for _, allowed := range txTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Insights consider only confirmed and amended, non-deleted transactions.
func (s TxStatus) CountsTowardInsights() bool {
	return s == TxConfirmed || s == TxAmended
}
