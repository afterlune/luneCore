package runtime

import (
	"context"

	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/session"
)

type executionTokenBudget struct {
	guard func(context.Context) error
	store       *session.Store
	executionID string
}

func (b *executionTokenBudget) Reserve(ctx context.Context, estimatedInput, requestedOutput int64) (agentruntime.TokenBudgetReservation, bool, error) {
	if b.guard != nil { if err := b.guard(ctx); err != nil { return agentruntime.TokenBudgetReservation{}, false, err } }
	reservation, allowed, err := b.store.ReserveExecutionTokens(ctx, b.executionID, estimatedInput, requestedOutput)
	if err != nil || !allowed {
		return agentruntime.TokenBudgetReservation{}, allowed, err
	}
	return agentruntime.TokenBudgetReservation{
		ID: reservation.ID, OutputLimit: reservation.OutputLimit, OutputWasLimited: reservation.OutputWasLimited,
	}, true, nil
}

func (b *executionTokenBudget) Settle(ctx context.Context, reservationID, actualTokens int64) error {
	return b.store.SettleExecutionTokens(ctx, b.executionID, reservationID, actualTokens)
}
