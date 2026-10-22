package model

// EstimateInputItemTokens estimates one input item on its own. Callers that
// price many items (compaction planning) use it to avoid re-estimating the whole
// request for every candidate change.
func EstimateInputItemTokens(item InputItem) int64 {
	estimate := tokenEstimator{}
	estimate.addInputItem(item)
	return estimate.value()
}
